// Package workspace owns the fixed, local rootless Podman workspace lifecycle.
package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	Image         = "localhost/mcp-remote-workspace:dev"
	managedLabel  = "io.github.r-agatsuma.mcp-remote-workspace.managed"
	idLabel       = "io.github.r-agatsuma.mcp-remote-workspace.workspace-id"
	createdLabel  = "io.github.r-agatsuma.mcp-remote-workspace.created-at"
	containerPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

var (
	ErrNotFound        = errors.New("unknown workspace ID")
	workspaceIDPattern = regexp.MustCompile(`^ws_[0-9a-f]{64}$`)
	containerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Workspace struct {
	ID        string
	CreatedAt time.Time
}

type record struct {
	Workspace
	containerID string
}

type commandRunner func(context.Context, ...string) ([]byte, error)

type Podman struct {
	mu         sync.Mutex
	run        commandRunner
	workspaces map[string]record
	// Only successful destroys in this server lifetime are remembered.
	destroyed map[string]bool
}

// NewPodman refuses rootful/remote operation and recovers before serving MCP.
func NewPodman(ctx context.Context) (*Podman, error) {
	return newPodman(ctx, runPodman)
}

func runPodman(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Rootless default devices become OCI bind mounts that ordinary inspect
	// does not expose. OCI precreate hooks can change the spec after inspection.
	// Clear both in the last-loaded config, even with host array appending enabled.
	config, err := os.CreateTemp("", "mcp-workspace-containers-*.conf")
	if err != nil {
		return nil, fmt.Errorf("create Podman config override: %w", err)
	}
	defer os.Remove(config.Name())
	_, writeErr := config.WriteString("[containers]\ndevices = [{append = false}]\n[engine]\nhooks_dir = [{append = false}]\n")
	closeErr := config.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return nil, fmt.Errorf("write Podman config override: %w", err)
	}
	// Explicitly override remote connection environment/configuration. Never
	// invoke a shell, sudo, or a container-engine API socket.
	cmd := exec.CommandContext(ctx, "podman", append([]string{"--remote=false", "--default-mounts-file=/dev/null"}, args...)...)
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if key != "CONTAINER_HOST" && key != "CONTAINER_CONNECTION" && key != "CONTAINER_SSHKEY" && key != "DOCKER_HOST" && key != "CONTAINERS_CONF_OVERRIDE" && key != "NOTIFY_SOCKET" {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "CONTAINERS_CONF_OVERRIDE="+config.Name())
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("podman %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func newPodman(ctx context.Context, run commandRunner) (*Podman, error) {
	p := &Podman{run: run, workspaces: make(map[string]record), destroyed: make(map[string]bool)}
	data, err := run(ctx, "info", "--format=json")
	if err != nil {
		return nil, err
	}
	var info struct {
		Host struct {
			Security        struct{ Rootless bool }
			ServiceIsRemote bool
			CgroupVersion   string
		}
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("decode podman info: %w", err)
	}
	if !info.Host.Security.Rootless || info.Host.ServiceIsRemote {
		return nil, errors.New("local rootless Podman is required")
	}
	if info.Host.CgroupVersion != "v2" {
		return nil, errors.New("cgroups v2 is required for rootless resource limits")
	}
	data, err = run(ctx, "ps", "--all", "--no-trunc", "--filter=label="+managedLabel+"=true", "--format={{.ID}}")
	if err != nil {
		return nil, err
	}
	for _, id := range strings.Fields(string(data)) {
		if !containerIDPattern.MatchString(id) {
			return nil, errors.New("invalid managed container ID")
		}
		c, err := p.inspect(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := c.validate(); err != nil {
			return nil, fmt.Errorf("recover managed container: %w", err)
		}
		wsID := c.Config.Labels[idLabel]
		if _, exists := p.workspaces[wsID]; exists {
			return nil, errors.New("duplicate managed workspace ID")
		}
		created, _ := time.Parse(time.RFC3339Nano, c.Config.Labels[createdLabel])
		p.workspaces[wsID] = record{Workspace{wsID, created}, id}
	}
	return p, nil
}

func (p *Podman) Create(ctx context.Context) (Workspace, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Workspace{}, fmt.Errorf("generate workspace ID: %w", err)
	}
	w := Workspace{ID: "ws_" + hex.EncodeToString(random[:]), CreatedAt: time.Now().UTC()}
	if _, exists := p.workspaces[w.ID]; exists || p.destroyed[w.ID] {
		return Workspace{}, errors.New("workspace ID collision")
	}
	args := []string{
		"create", "--name=mcp-remote-workspace-" + w.ID, "--pull=never",
		"--label=" + managedLabel + "=true", "--label=" + idLabel + "=" + w.ID,
		"--label=" + createdLabel + "=" + w.CreatedAt.Format(time.RFC3339Nano),
		"--privileged=false", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--userns=nomap", "--user=0:0", "--pid=private", "--ipc=private", "--uts=private",
		"--network=slirp4netns:allow_host_loopback=false", "--cgroups=enabled", "--cgroupns=private",
		"--cpu-period=100000", "--cpu-quota=200000", "--memory=1073741824", "--memory-swap=1073741824", "--pids-limit=256",
		"--image-volume=ignore", "--http-proxy=false", "--unsetenv-all", "--sdnotify=ignore",
		"--env=HOME=/root", "--env=PATH=" + containerPath,
		"--workdir=/workspace", "--entrypoint=/usr/bin/sleep", Image, "infinity",
	}
	data, err := p.run(ctx, args...)
	if err != nil {
		p.workspaces[w.ID] = record{Workspace: w}
		return Workspace{}, p.rollbackCreate(ctx, w.ID, err)
	}
	id := strings.TrimSpace(string(data))
	if !containerIDPattern.MatchString(id) {
		p.workspaces[w.ID] = record{Workspace: w}
		return Workspace{}, p.rollbackCreate(ctx, w.ID, errors.New("invalid created container ID"))
	}
	// Record even an incompletely started workspace so its managed state is
	// recoverable. Failure handling does not retry Podman commands.
	p.workspaces[w.ID] = record{w, id}
	c, err := p.inspect(ctx, id)
	if err == nil {
		err = c.validate()
	}
	if err == nil && (c.Config.Labels[idLabel] != w.ID || c.Config.Labels[createdLabel] != w.CreatedAt.Format(time.RFC3339Nano)) {
		err = errors.New("created workspace labels disagree")
	}
	if err == nil {
		_, err = p.run(ctx, "start", id)
	}
	if err != nil {
		return Workspace{}, p.rollbackCreate(ctx, w.ID, err)
	}
	return w, nil
}

// Resolve an interrupted create by its exact labels rather than trusting its
// stdout or deleting a container that merely has the expected name.
func (p *Podman) resolveContainer(ctx context.Context, w record) (string, error) {
	if w.containerID != "" {
		return w.containerID, nil
	}
	data, err := p.run(ctx, "ps", "--all", "--no-trunc",
		"--filter=label="+managedLabel+"=true",
		"--filter=label="+idLabel+"="+w.ID,
		"--filter=label="+createdLabel+"="+w.CreatedAt.Format(time.RFC3339Nano), "--format={{.ID}}")
	if err != nil {
		return "", err
	}
	ids := strings.Fields(string(data))
	if len(ids) == 0 {
		return "", nil
	}
	if len(ids) != 1 || !containerIDPattern.MatchString(ids[0]) {
		return "", errors.New("unexpected interrupted-create container list")
	}
	w.containerID = ids[0]
	p.workspaces[w.ID] = w
	return ids[0], nil
}

func (p *Podman) rollbackCreate(ctx context.Context, id string, cause error) error {
	// A canceled request must not cancel discovery or removal. On failure keep
	// the handle so a subsequent explicit destroy can resolve and remove it.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := p.removeContainer(cleanupCtx, p.workspaces[id]); err != nil {
		return fmt.Errorf("create workspace %s: %w; rollback failed: %v", id, cause, err)
	}
	delete(p.workspaces, id)
	return cause
}

func (p *Podman) removeContainer(ctx context.Context, w record) error {
	id, err := p.resolveContainer(ctx, w)
	if err != nil || id == "" {
		return err
	}
	// --force stops a running container; --ignore tolerates external removal.
	_, err = p.run(ctx, "rm", "--force", "--ignore", "--volumes", id)
	return err
}

func (p *Podman) Destroy(ctx context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.destroyed[id] {
		return nil
	}
	w, exists := p.workspaces[id]
	if !exists {
		return ErrNotFound
	}
	if err := p.removeContainer(ctx, w); err != nil {
		return err
	}
	delete(p.workspaces, id)
	p.destroyed[id] = true
	return nil
}

type container struct {
	ID           string `json:"Id"`
	ImageName    string
	Mounts       []json.RawMessage
	BoundingCaps []string
	Config       struct {
		Labels     map[string]string
		Env        []string
		WorkingDir string
		User       string
	}
	HostConfig struct {
		Privileged                                bool
		NetworkMode, PidMode, IpcMode, UsernsMode string
		Binds, VolumesFrom, Devices               []json.RawMessage
		SecurityOpt                               []string
		Memory, PidsLimit, CpuPeriod, CpuQuota    int64
	}
}

func (p *Podman) inspect(ctx context.Context, id string) (container, error) {
	data, err := p.run(ctx, "container", "inspect", id)
	if err != nil {
		return container{}, err
	}
	var containers []container
	if err := json.Unmarshal(data, &containers); err != nil {
		return container{}, fmt.Errorf("decode podman inspect: %w", err)
	}
	if len(containers) != 1 || containers[0].ID != id {
		return container{}, errors.New("unexpected podman inspect result")
	}
	return containers[0], nil
}

// Check the effective configuration before starting: containers.conf and
// mounts.conf must not silently introduce host mounts, devices or credentials.
func (c container) validate() error {
	if c.Config.Labels[managedLabel] != "true" || !workspaceIDPattern.MatchString(c.Config.Labels[idLabel]) {
		return errors.New("invalid workspace management labels")
	}
	if _, err := time.Parse(time.RFC3339Nano, c.Config.Labels[createdLabel]); err != nil {
		return errors.New("invalid workspace creation timestamp")
	}
	h := c.HostConfig
	if c.ImageName != Image || c.Config.WorkingDir != "/workspace" || c.Config.User != "0:0" || h.Privileged ||
		(h.NetworkMode != "slirp4netns" && h.NetworkMode != "slirp4netns:allow_host_loopback=false") || h.PidMode != "private" || h.IpcMode != "private" || h.UsernsMode != "private" ||
		len(c.Mounts)+len(h.Binds)+len(h.VolumesFrom)+len(h.Devices)+len(c.BoundingCaps) != 0 {
		return errors.New("container violates workspace isolation profile")
	}
	if h.Memory != 1073741824 || h.PidsLimit != 256 || h.CpuPeriod != 100000 || h.CpuQuota != 200000 {
		return errors.New("container violates workspace resource limits")
	}
	noNewPrivileges := false
	for _, opt := range h.SecurityOpt {
		if opt == "no-new-privileges" || opt == "no-new-privileges=true" {
			noNewPrivileges = true
		}
	}
	if !noNewPrivileges {
		return errors.New("container requires no-new-privileges")
	}
	for _, env := range c.Config.Env {
		if env != "HOME=/root" && env != "PATH="+containerPath && env != "container=podman" && !strings.HasPrefix(env, "HOSTNAME=") {
			return errors.New("unexpected container environment")
		}
	}
	return nil
}
