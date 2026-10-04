// Package workspace manages disposable containers through local rootless Podman.
package workspace

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"time"
)

//go:embed containers.conf
var containersConfig []byte

// Runner is the narrow command boundary mocked by lifecycle tests. Production
// always uses /usr/bin/podman, an explicit environment, and no host shell.
type Runner interface {
	Run(context.Context, ...string) ([]byte, error)
}

type podman struct {
	dir string
	env []string
}

func newPodman() (*podman, error) {
	if os.Getuid() == 0 || os.Geteuid() != os.Getuid() {
		return nil, errors.New("mcp-workspaced requires a non-root service user")
	}
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return nil, fmt.Errorf("resolve service user: %w", err)
	}
	// The prepared host must already provide /run/user/UID. Application-owned
	// configuration has a stable path: conmon's exit cleanup may use it after
	// this daemon exits. Do not consult TMPDIR or caller-supplied paths.
	dir := "/run/user/" + u.Uid + "/mcp-workspaced-v0"
	if err := prepareConfiguration(dir); err != nil {
		return nil, err
	}
	return &podman{dir: dir, env: podmanEnvironment(u.HomeDir, u.Uid, dir)}, nil
}

func prepareConfiguration(dir string) error {
	if err := privateDirectory(dir); err != nil {
		return err
	}
	configPath := filepath.Join(dir, "containers.conf")
	file, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, writeErr := file.Write(containersConfig)
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	// Preserve any existing file; conflicting configuration requires explicit
	// operator intervention rather than an automatic repair or overwrite.
	info, err := os.Lstat(configPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("workspace configuration must be a private regular file")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, containersConfig) {
		return errors.New("existing workspace configuration differs from the embedded v0 profile")
	}
	// An explicit empty hooks directory also disables Podman's legacy fallback
	// to system hook directories. It cannot contain host/user supplied hooks.
	hooksPath := filepath.Join(dir, "hooks")
	if err := privateDirectory(hooksPath); err != nil {
		return err
	}
	hooks, err := os.ReadDir(hooksPath)
	if err != nil {
		return err
	}
	if len(hooks) != 0 {
		return errors.New("project hook directory must be empty")
	}
	return nil
}

func podmanEnvironment(home, uid, configDir string) []string {
	runtimeDir := "/run/user/" + uid
	return []string{
		"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "HOME=" + home,
		"XDG_RUNTIME_DIR=" + runtimeDir,
		"XDG_CONFIG_HOME=" + configDir,
		"CONTAINERS_CONF=" + filepath.Join(configDir, "containers.conf"),
		// Required only by the fixed systemd cgroup manager. Never forwarded to
		// the container. The prepared host must provide this user bus.
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtimeDir + "/bus",
	}
}

func (p *podman) command(ctx context.Context, args ...string) *exec.Cmd {
	global := []string{"--remote=false", "--hooks-dir=" + filepath.Join(p.dir, "hooks"), "--default-mounts-file=/dev/null"}
	cmd := exec.CommandContext(ctx, "/usr/bin/podman", append(global, args...)...)
	cmd.Env = append([]string(nil), p.env...)
	cmd.Dir = "/"
	return cmd
}

func (p *podman) Run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := p.command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// Stderr may contain host paths or identifiers. Keep MCP errors bounded
		// to the failed operation and OS error; stdout is never protocol output.
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return stdout.Bytes(), fmt.Errorf("podman %s: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}

func privateDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("workspace configuration directory must be a private directory")
	}
	return nil
}

// Open validates the prepared host and recovers the registry before serving MCP.
// Immutable runtime configuration and workspaces survive daemon exit.
func Open(ctx context.Context) (*Manager, error) {
	p, err := newPodman()
	if err != nil {
		return nil, err
	}
	return New(ctx, p)
}
