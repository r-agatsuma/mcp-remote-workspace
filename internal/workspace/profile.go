package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

const (
	memoryBytes   int64 = 2 * 1024 * 1024 * 1024
	cpuPeriod     int64 = 100000
	cpuQuota      int64 = 200000
	pidsLimit     int64 = 256
	containerPath       = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

func createArgs(w Workspace, image string) []string {
	return []string{
		"create", "--pull=never", "--name=mcp-workspace-" + w.ID,
		"--label=" + managedLabel + "=true", "--label=" + idLabel + "=" + w.ID,
		"--label=" + createdLabel + "=" + w.CreatedAt.Format(time.RFC3339Nano),
		"--label=" + profileLabel + "=" + profileVersion,
		"--privileged=false", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--security-opt=seccomp=/usr/share/containers/seccomp.json",
		"--pid=private", "--ipc=private", "--uts=private", "--cgroupns=private",
		// Auto allocates a separate user namespace from the prepared user's
		// subordinate IDs; container root is not the host service user's UID.
		"--userns=auto:size=65536", "--user=0:0",
		"--network=slirp4netns:allow_host_loopback=false", "--http-proxy=false",
		"--read-only=false", "--image-volume=ignore", "--workdir=/workspace",
		"--cgroups=enabled", "--cpu-period=100000", "--cpu-quota=200000",
		"--memory=2147483648", "--memory-swap=2147483648", "--pids-limit=256",
		"--sdnotify=ignore", "--systemd=false", "--init=false", "--restart=no",
		"--log-driver=k8s-file", "--log-opt=max-size=1048576", "--stop-timeout=0",
		"--unsetenv-all", "--env=" + containerPath, "--env=HOME=/root", "--env=LANG=C.UTF-8",
		"--entrypoint=/usr/bin/sleep", image, "infinity",
	}
}

// Fields come from Podman 5's inspect API, not Docker's. Inspection asserts the
// already deterministic request; it never sanitizes arbitrary inherited config.
type containerInspection struct {
	ID            string
	Image         string
	EffectiveCaps []string
	BoundingCaps  []string
	Mounts        []json.RawMessage
	State         *struct{ Running bool }
	Config        *struct {
		Labels         map[string]string
		WorkingDir     string
		User           string
		Env            []string
		Entrypoint     []string
		Cmd            []string
		Secrets        []json.RawMessage
		SystemdMode    bool
		SdNotifyMode   string
		SdNotifySocket string
	}
	HostConfig *struct {
		Privileged     *bool
		ReadonlyRootfs *bool
		Binds          []string
		Tmpfs          map[string]string
		Devices        []json.RawMessage
		CapAdd         []string
		SecurityOpt    []string
		PidMode        string
		IpcMode        string
		UTSMode        string
		UsernsMode     string
		CgroupMode     string
		Cgroups        string
		CgroupManager  string
		NetworkMode    string
		PortBindings   map[string]json.RawMessage
		CpuPeriod      int64
		CpuQuota       int64
		Memory         int64
		MemorySwap     int64
		PidsLimit      int64
		AutoRemove     bool
		Init           bool
		RestartPolicy  struct{ Name string }
	}
}

func (c *containerInspection) identity() (Workspace, error) {
	if c.Config == nil {
		return Workspace{}, errors.New("missing workspace configuration")
	}
	l := c.Config.Labels
	if l[managedLabel] != "true" || l[profileLabel] != profileVersion || !workspaceIDPattern.MatchString(l[idLabel]) {
		return Workspace{}, errors.New("invalid workspace ownership labels")
	}
	timestamp := l[createdLabel]
	created, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil || created.Format(time.RFC3339Nano) != timestamp || created.Location() != time.UTC {
		return Workspace{}, errors.New("invalid workspace creation timestamp label")
	}
	return Workspace{ID: l[idLabel], CreatedAt: created}, nil
}

func (c *containerInspection) verify(image string) error {
	if c.Config == nil || c.HostConfig == nil {
		return errors.New("missing container profile")
	}
	config, h := c.Config, c.HostConfig
	checks := []struct {
		name string
		ok   bool
	}{
		{"fixed image", imageID(c.Image) == image},
		{"writable /workspace", config.WorkingDir == "/workspace" && h.ReadonlyRootfs != nil && !*h.ReadonlyRootfs},
		{"unprivileged", h.Privileged != nil && !*h.Privileged},
		{"no capabilities", len(c.EffectiveCaps) == 0 && len(c.BoundingCaps) == 0 && len(h.CapAdd) == 0},
		{"no mounts or devices", len(c.Mounts) == 0 && len(h.Binds) == 0 && len(h.Tmpfs) == 0 && len(h.Devices) == 0 && len(config.Secrets) == 0},
		{"security options", slices.Equal(h.SecurityOpt, []string{"no-new-privileges", "seccomp=/usr/share/containers/seccomp.json"})},
		{"private namespaces", h.PidMode == "private" && h.IpcMode == "private" && h.UTSMode == "private" && h.UsernsMode == "private" && h.CgroupMode == "private"},
		{"outbound network", h.NetworkMode == "slirp4netns" && len(h.PortBindings) == 0},
		{"resource limits", h.Cgroups == "default" && h.CgroupManager == "systemd" && h.CpuPeriod == cpuPeriod && h.CpuQuota == cpuQuota && h.Memory == memoryBytes && h.MemorySwap == memoryBytes && h.PidsLimit == pidsLimit},
		{"container user", config.User == "0:0"},
		{"container process", slices.Equal(config.Entrypoint, []string{"/usr/bin/sleep"}) && slices.Equal(config.Cmd, []string{"infinity"})},
		{"disposable runtime", !h.AutoRemove && !h.Init && h.RestartPolicy.Name == "no" && !config.SystemdMode},
		{"sd-notify disabled", config.SdNotifyMode == "ignore" && config.SdNotifySocket == ""},
	}
	for _, check := range checks {
		if !check.ok {
			return fmt.Errorf("workspace profile assertion failed: %s", check.name)
		}
	}
	// No image/config/host environment survives --unsetenv-all. Compare as sets
	// because Podman does not promise a stable environment ordering.
	want := []string{containerPath, "HOME=/root", "LANG=C.UTF-8"}
	got := slices.Clone(config.Env)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		return errors.New("workspace profile assertion failed: container environment")
	}
	return nil
}
