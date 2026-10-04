package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Requires a prepared local rootless Podman host and the project image. This
// test neither installs prerequisites nor builds/pulls images.
func TestRootlessPodmanIntegration(t *testing.T) {
	if os.Getenv("MCP_WORKSPACE_PODMAN_TEST") != "1" {
		t.Skip("set MCP_WORKSPACE_PODMAN_TEST=1 on a prepared rootless Podman host")
	}
	// Host defaults can add hidden devices or run hooks after inspection.
	// A selected precreate hook would mark its invocation and prevent startup.
	dir := t.TempDir()
	hooksDir := filepath.Join(dir, "hooks.d")
	if err := os.Mkdir(hooksDir, 0700); err != nil {
		t.Fatal(err)
	}
	hookMarker := filepath.Join(dir, "hook-invoked")
	hook, err := json.Marshal(map[string]any{
		"version": "1.0.0",
		"hook": map[string]any{
			"path": "/bin/sh",
			"args": []string{"/bin/sh", "-ec", `printf invoked > "$1"; exit 1`, "workspace-hook", hookMarker},
		},
		"when":   map[string]any{"always": true},
		"stages": []string{"precreate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "workspace-forbidden.json"), hook, 0600); err != nil {
		t.Fatal(err)
	}
	hostConfig := filepath.Join(dir, "containers.conf")
	config := fmt.Sprintf("[containers]\ndevices = [\"/dev/null:/dev/workspace-forbidden-device:rwm\", {append = true}]\n[engine]\nhooks_dir = [%q, {append = true}]\n", hooksDir)
	if err := os.WriteFile(hostConfig, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_CONF", hostConfig)
	t.Setenv("CONTAINERS_CONF_OVERRIDE", hostConfig)
	// Emulate a service launched with a real systemd notification socket.
	notifyPath := filepath.Join(dir, "notify.sock")
	notify, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: notifyPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { notify.Close() })
	t.Setenv("NOTIFY_SOCKET", notifyPath)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p, err := NewPodman(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := len(p.workspaces)
	w, err := p.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := p.Destroy(cleanupCtx, w.ID); err != nil {
			t.Errorf("test workspace cleanup: %v", err)
		}
	})
	recovered, err := NewPodman(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.workspaces) != before+1 || recovered.workspaces[w.ID].Workspace != w {
		t.Fatal("create/recovery did not preserve exactly one workspace")
	}
	if _, err := os.Stat(hookMarker); !os.IsNotExist(err) {
		t.Fatalf("host OCI hook was invoked or marker could not be checked: %v", err)
	}
	id := recovered.workspaces[w.ID].containerID
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// The host home path is an argument, never interpolated into a shell command.
	check := `test "$(id -u)" = 0
test "$PWD" = /workspace
test ! -e "$1"
test ! -e /var/run/docker.sock
test ! -e /run/podman/podman.sock
test ! -e /run/secrets
test ! -e /run/notify
test -z "${NOTIFY_SOCKET+x}"
test ! -e /dev/workspace-forbidden-device
test -z "$(find /run /var/run -type s -print -quit)"
touch /workspace/writable
for tool in curl git jq rg patch make gcc g++ python3; do command -v "$tool"; done
curl --fail --silent --show-error --max-time 20 https://example.com/ >/dev/null
`
	if _, err := p.run(ctx, "exec", id, "/bin/sh", "-ec", check, "workspace-check", home); err != nil {
		t.Fatal(err)
	}
	if err := recovered.Destroy(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	data, err := p.run(ctx, "ps", "--all", "--no-trunc", "--filter=label="+idLabel+"="+w.ID, "--format={{.ID}}")
	if err != nil || strings.TrimSpace(string(data)) != "" {
		t.Fatalf("destroy left container: %s (%v)", data, err)
	}
	if err := recovered.Destroy(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
}
