// Package workspace manages disposable containers through local rootless Podman.
package workspace

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"debug/elf"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/r-agatsuma/mcp-remote-workspace/internal/control"
)

//go:embed containers.conf
var containersConfig []byte

// Runner is the narrow command boundary mocked by lifecycle tests. Production
// always uses /usr/bin/podman, an explicit environment, and no host shell.
type Runner interface {
	Run(context.Context, ...string) ([]byte, error)
}

type podman struct {
	dir      string
	stateDir string
	env      []string
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
	stateDir := filepath.Join(u.HomeDir, ".local/share/mcp-workspaced-v0")
	if err := privateDirectory(stateDir); err != nil {
		return nil, err
	}
	return &podman{dir: dir, stateDir: stateDir, env: podmanEnvironment(u.HomeDir, u.Uid, dir)}, nil
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

type limitedBuffer struct {
	bytes.Buffer
	limit    int64
	overflow bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	n := len(data)
	remaining := b.limit - int64(b.Len())
	if int64(len(data)) > remaining {
		b.overflow = true
		data = data[:int(remaining)]
	}
	_, _ = b.Buffer.Write(data)
	return n, nil
}

func (p *podman) Operate(ctx context.Context, input []byte, limit int64, args ...string) ([]byte, error) {
	cmd := p.command(ctx, args...)
	cmd.Stdin = bytes.NewReader(input)
	stdout := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: control.StreamLimit}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("podman control transport failed: %w", err)
	}
	if stdout.overflow || stderr.overflow {
		return nil, errors.New("control transport exceeded response limit")
	}
	return stdout.Bytes(), nil
}

// Refuse a dynamically linked or unexpected control program. Operators install
// this fixed artifact; the daemon never builds it, downloads it or repairs it.
func verifyControlRuntime(path string) error {
	for current := path; current != "/"; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("control runtime must be installed: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("control runtime and parents must not be symlinks or writable by other users")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
			return errors.New("control runtime and parents must be owned by root or the service user")
		}
		if current == path && (!info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0) {
			return errors.New("control runtime must be an executable regular file")
		}
	}
	return verifyControlArtifact(path)
}

func verifyControlArtifact(path string) error {
	file, err := elf.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP {
			return errors.New("control runtime must be statically linked")
		}
	}
	if libraries, err := file.ImportedLibraries(); err != nil || len(libraries) != 0 {
		return errors.New("control runtime must not import shared libraries")
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return err
	}
	if info.Path != "github.com/r-agatsuma/mcp-remote-workspace/cmd/mcp-control" {
		return errors.New("unexpected control runtime program")
	}
	for _, setting := range info.Settings {
		if setting.Key == "CGO_ENABLED" && setting.Value == "0" {
			return nil
		}
	}
	return errors.New("control runtime must be built with CGO_ENABLED=0")
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
	limit := control.DefaultTextLimit
	// This service/operator setting is never supplied by a tool caller.
	if value := os.Getenv("MCP_MAX_TEXT_BYTES"); value != "" {
		var err error
		limit, err = strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || limit < 1 || limit > 64*1024*1024 {
			return nil, errors.New("MCP_MAX_TEXT_BYTES must be between 1 and 67108864")
		}
	}
	if err := verifyControlRuntime(controlHostPath); err != nil {
		return nil, err
	}
	p, err := newPodman()
	if err != nil {
		return nil, err
	}
	m, err := New(ctx, p)
	if err != nil {
		return nil, err
	}
	m.maxTextBytes = limit
	return m, nil
}
