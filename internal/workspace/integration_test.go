package workspace

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// Opt-in only: requires a dedicated prepared account and prebuilt fixed image.
// This test never installs dependencies, builds/pulls images, or edits host config.
func TestRootlessPodmanIntegration(t *testing.T) {
	if os.Getenv("MCP_WORKSPACE_INTEGRATION") != "1" {
		t.Skip("set MCP_WORKSPACE_INTEGRATION=1 on a prepared rootless Podman host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p, err := newPodman()
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	w, err := m.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := m.Destroy(cleanupCtx, w.ID); err != nil {
			t.Errorf("integration cleanup: %v", err)
		}
	})
	id := m.entries[w.ID].container
	// Test-only execution probes actual runtime behavior; it is not an MCP exec
	// backend or a host shell. Resource files assert enforcement, not just flags.
	probe := `import os, pathlib
p = pathlib.Path('/workspace/probe.txt')
p.write_text('workspace writable\n')
assert p.read_text() == 'workspace writable\n'
status = dict(line.split(':', 1) for line in pathlib.Path('/proc/self/status').read_text().splitlines() if ':' in line)
assert int(status['CapEff'].strip(), 16) == 0
assert int(status['CapBnd'].strip(), 16) == 0
assert status['NoNewPrivs'].strip() == '1'
assert os.environ['HOSTNAME'] == 'mcp-workspace'
assert os.uname().nodename == 'mcp-workspace'
for name in ('NOTIFY_SOCKET', 'LISTEN_PID', 'LISTEN_FDS', 'LISTEN_FDNAMES', 'SSH_AUTH_SOCK', 'GITHUB_TOKEN', 'OPENAI_API_KEY', 'HTTP_PROXY', 'HTTPS_PROXY'):
    assert name not in os.environ, name
for name in ('/root/.ssh', '/root/.codex', '/root/.config/containers/auth.json', '/run/podman/podman.sock', '/var/run/docker.sock', '/dev/sda', '/dev/kvm', '/dev/fuse'):
    assert not pathlib.Path(name).exists(), name
assert pathlib.Path('/sys/fs/cgroup/memory.max').read_text().strip() == '2147483648'
assert pathlib.Path('/sys/fs/cgroup/memory.swap.max').read_text().strip() == '0'
assert pathlib.Path('/sys/fs/cgroup/pids.max').read_text().strip() == '256'
assert pathlib.Path('/sys/fs/cgroup/cpu.max').read_text().strip() == '200000 100000'
assert p.stat().st_uid == 0
`
	if _, err := p.Run(ctx, "exec", id, "/usr/bin/python3", "-c", probe); err != nil {
		t.Fatalf("runtime isolation probe: %v", err)
	}
	if _, err := p.Run(ctx, "exec", id, "/usr/bin/curl", "--fail", "--silent", "--show-error", "--max-time", "20", "https://deb.debian.org/debian/README"); err != nil {
		t.Fatalf("outbound Internet: %v", err)
	}
	restarted, err := New(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if recovered := restarted.entries[w.ID]; recovered == nil || !recovered.CreatedAt.Equal(w.CreatedAt) {
		t.Fatal("restart lost workspace ID")
	}
	if err := restarted.Destroy(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := restarted.discover(ctx, w.ID)
	if err != nil || len(ids) != 0 {
		t.Fatalf("destroy left compute: %v, %v", ids, err)
	}
	if err := restarted.Destroy(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Destroy(ctx, "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ID: %v", err)
	}
}
