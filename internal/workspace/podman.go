package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type commandRunner interface {
	run(context.Context, ...string) ([]byte, error)
}

type localCLI struct{ path string }

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8<<20 {
		return 0, errors.New("Podman output exceeds adapter bound")
	}
	return b.Buffer.Write(p)
}

func (c localCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	return runHostCommand(ctx, c.path, append([]string{"--remote=false"}, args...)...)
}

func hostCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	// No caller-controlled connection settings, engine socket or shell. HOME
	// and runtime directory belong to the service account's launch environment.
	for _, key := range []string{"HOME", "PATH", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.WaitDelay = time.Second
	return cmd
}

func runHostCommand(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := hostCommand(ctx, path, args...)
	var stdout, stderr boundedOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// Runtime diagnostics may contain sensitive workload output; do not
		// expose them through the lifecycle API.
		return nil, fmt.Errorf("local Podman %s failed: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}

type Podman struct {
	runner      commandRunner
	image       string
	initialized bool
	policy      *Policy
	// Host-owned read-only proc/cgroup roots. Nil selects verified Linux mounts.
	processRoots  func() (*os.Root, *os.Root, error)
	publicDNS     []string                                                 // service/deployment-owned public resolver defaults
	profileDir    string                                                   // private service-owned empty hooks/default-mount configuration
	hostRunner    func(context.Context, string, ...string) ([]byte, error) // private command fixture
	removeTimeout time.Duration                                            // private test seam; zero selects the bounded ordinary removal lifetime
}

func NewPodman(path, image string, publicDNS ...string) *Podman {
	return NewPodmanWithPolicy(path, image, ReferencePolicy(), publicDNS...)
}

func NewPodmanWithPolicy(path, image string, policy Policy, publicDNS ...string) *Podman {
	return &Podman{runner: localCLI{path}, image: image, policy: &policy, publicDNS: append([]string(nil), publicDNS...)}
}

func (p *Podman) resourcePolicy() Policy {
	if p.policy == nil {
		return ReferencePolicy()
	}
	return *p.policy
}

func (p *Podman) Initialize(ctx context.Context) error {
	p.initialized = false
	resolvers, err := validatedPublicDNS(p.publicDNS)
	if err != nil {
		return err
	}
	p.publicDNS = resolvers
	if err := prepareRuntimeProfile(p.profileDir); err != nil {
		return err
	}
	if err := p.resourcePolicy().Validate(); err != nil {
		return err
	}
	if !validImage(p.image) {
		return errors.New("digest-pinned canonical image required")
	}
	if cli, ok := p.runner.(localCLI); ok {
		if os.Geteuid() == 0 || !filepath.IsAbs(cli.path) {
			return errors.New("host-side rootless Podman under a non-root service account required")
		}
	}
	data, err := p.runner.run(ctx, "info", "--format=json")
	if err != nil {
		return err
	}
	var info struct {
		Host struct {
			Security struct {
				Rootless bool `json:"rootless"`
			} `json:"security"`
		} `json:"host"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return err
	}
	if !info.Host.Security.Rootless {
		return errors.New("runtime did not attest rootless mode")
	}
	p.initialized = true
	return nil
}

type containerInspection struct {
	ID        string `json:"Id"`
	Image     string
	Name      string
	ImageName string
	Config    struct{ Labels map[string]string }
	State     struct {
		Status                            string
		Running, Paused, Restarting, Dead bool
		Pid                               int
		StartedAt                         string
		CgroupPath                        string
	}
}

func (v containerInspection) object() Object {
	image := v.ImageName
	if claimed := v.Config.Labels[imageLabel]; immutableImage(claimed) {
		image = "" // a matching mutable name cannot substitute for actual image ID
		if strings.TrimPrefix(v.Image, "sha256:") == strings.TrimPrefix(claimed, "sha256:") {
			image = claimed
		}
	}
	return Object{ID: v.ID, Name: v.Name, Labels: v.Config.Labels, Image: image}
}

func (p *Podman) inspectContainer(ctx context.Context, id string) (containerInspection, error) {
	if !validHex(id) {
		return containerInspection{}, errors.New("invalid runtime object ID")
	}
	data, err := p.runner.run(ctx, "container", "inspect", "--", id)
	if err != nil {
		return containerInspection{}, err
	}
	var inspected []containerInspection
	if err := json.Unmarshal(data, &inspected); err != nil {
		return containerInspection{}, err
	}
	if len(inspected) != 1 || inspected[0].ID != id {
		return containerInspection{}, errors.New("unexpected exact container inspection")
	}
	return inspected[0], nil
}

func (p *Podman) inspect(ctx context.Context, id string) (Object, error) {
	v, err := p.inspectContainer(ctx, id)
	return v.object(), err
}

func (p *Podman) List(ctx context.Context) ([]Object, error) {
	if !p.initialized {
		return nil, ErrNotReady
	}
	data, err := p.runner.run(ctx, "container", "list", "--all", "--no-trunc", "--format=json")
	if err != nil {
		return nil, err
	}
	var listed []struct {
		ID     string `json:"Id"`
		Names  []string
		Labels map[string]string
	}
	if err := json.Unmarshal(data, &listed); err != nil {
		return nil, err
	}
	if listed == nil {
		return nil, errors.New("runtime list must be a JSON array")
	}
	objects := make([]Object, 0, len(listed))
	for _, item := range listed {
		if !validHex(item.ID) || len(item.Names) == 0 {
			return nil, errors.New("incomplete runtime list entry")
		}
		for _, name := range item.Names {
			if name == "" {
				return nil, errors.New("empty runtime object name")
			}
		}
		claimed := false
		for _, name := range item.Names {
			claimed = claimed || strings.HasPrefix(name, "mcpws-")
		}
		for label := range item.Labels {
			claimed = claimed || strings.HasPrefix(label, labelPrefix)
		}
		if !claimed {
			continue
		}
		if len(item.Names) != 1 {
			return nil, errors.New("ambiguous service object names")
		}
		o, err := p.inspect(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		if o.Name != item.Names[0] {
			return nil, errors.New("service object changed during discovery")
		}
		if _, owned, err := classify(o); err != nil || !owned {
			return nil, errors.New("unrecognized service runtime object")
		}
		if o.Image != p.image && !immutableImage(o.Image) {
			return nil, errors.New("runtime object has a different canonical image")
		}
		objects = append(objects, o)
	}
	return objects, nil
}

func (p *Podman) Create(ctx context.Context, e Exact, image string) (err error) {
	defer func() { err = localRuntimeError(ctx, err) }()
	if !p.initialized {
		return ErrNotReady
	}
	if err := e.validate(); err != nil {
		return err
	}
	if !immutableImage(image) && (image != p.image || !validImage(image)) {
		return errors.New("digest-pinned canonical image required")
	}
	if immutableImage(image) {
		if err := p.confirmBuiltImage(ctx, e, image); err != nil {
			return err
		}
	}
	args := []string{"container", "create", "--pull=never", "--name", objectName(e),
		"--network=" + workspaceNetwork(), "--image-volume=ignore", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--no-healthcheck", "--hooks-dir=" + filepath.Join(p.profileDir, "hooks"),
		"--default-mounts-file=" + filepath.Join(p.profileDir, "mounts.conf"),
		"--http-proxy=false", "--no-hosts", "--dns-search=.", "--publish-all=false",
		"--privileged=false", "--pid=private", "--ipc=private", "--uts=private", "--cgroupns=private",
		"--systemd=false", "--sdnotify=ignore", "--seccomp-policy=default"}
	for _, dns := range p.publicDNS {
		args = append(args, "--dns="+dns)
	}
	policy := p.resourcePolicy()
	args = append(args, "--memory="+strconv.FormatInt(policy.MemoryBytes, 10),
		"--memory-swap="+strconv.FormatInt(policy.MemoryBytes+policy.SwapBytes, 10),
		"--cpu-quota="+strconv.FormatInt(policy.CPUQuota, 10),
		"--cgroup-conf=cpu.weight="+strconv.FormatUint(policy.CPUWeight, 10),
		"--pids-limit="+strconv.FormatUint(policy.PIDsMax, 10),
		"--ulimit=nofile="+strconv.FormatUint(policy.NoFile, 10)+":"+strconv.FormatUint(policy.NoFile, 10))
	labels := objectLabels(e, image)
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--label", key+"="+labels[key])
	}
	args = append(args, "--", image)
	data, err := p.runner.run(ctx, args...)
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(data))
	o, err := p.inspect(ctx, id)
	if err != nil {
		return err
	}
	actual, owned, err := classify(o)
	if err != nil || !owned || actual != e || o.Image != image {
		return errors.New("created object did not match exact canonical generation")
	}
	_, err = p.runner.run(ctx, "container", "start", "--", id)
	return err
}

func (p *Podman) Destroy(ctx context.Context, e Exact) (err error) {
	// Synchronous preflight failures leave no destructive backend work alive.
	// Once removal is submitted, ambiguity must retain backend authority as well
	// as generation intent. Neither classification by itself proves absence.
	submitted := false
	defer func() {
		if err != nil {
			if !submitted {
				err = CompletedBackendError(err)
			} else {
				err = errors.Join(ErrOutcomeUnknown, err)
			}
		}
	}()
	if err := e.validate(); err != nil {
		return err
	}
	objects, err := p.List(ctx)
	if err != nil {
		return err
	}
	var target string
	for _, o := range objects {
		actual, _, err := classify(o)
		if err != nil {
			return err
		}
		if actual == e {
			if target != "" {
				return ErrProcessUnsafe
			}
			target = o.ID
		}
	}
	if target == "" {
		return ctx.Err()
	}
	lifetime := p.removeTimeout
	if lifetime == 0 {
		lifetime = 5 * time.Second
	}
	ordinary, cancel := context.WithTimeout(ctx, lifetime)
	submitted = true
	_, removeErr := p.runner.run(ordinary, "container", "rm", "--force", "--", target)
	timedOut := ordinary.Err() == context.DeadlineExceeded
	cancel()
	if timedOut {
		removeErr = errors.Join(removeErr, context.DeadlineExceeded)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if removeErr != nil {
		// Only our bounded stop deadline plus newly attested PID saturation
		// permits fallback. Caller cancellation/deadlines and arbitrary runtime
		// errors cannot create kill authority.
		if !timedOut {
			return removeErr
		}
		if err := p.recoverPressureScope(ctx, e, target); err != nil {
			return err
		}
		retry, cancel := context.WithTimeout(ctx, lifetime)
		_, err = p.runner.run(retry, "container", "rm", "--force", "--", target)
		ended := retry.Err()
		cancel()
		if err != nil || ended != nil {
			return errors.Join(err, ended)
		}
	}
	objects, err = p.List(ctx)
	if err != nil {
		return err
	}
	for _, o := range objects {
		actual, _, err := classify(o)
		if err != nil || actual == e || o.ID == target {
			return ErrOutcomeUnknown
		}
	}
	return ctx.Err()
}

func (p *Podman) host(ctx context.Context, executable string, args ...string) ([]byte, error) {
	if p.hostRunner != nil {
		return p.hostRunner(ctx, executable, args...)
	}
	return runHostCommand(ctx, executable, args...)
}
