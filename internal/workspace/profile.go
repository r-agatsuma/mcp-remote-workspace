package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	memoryBytes          int64 = 2 * 1024 * 1024 * 1024
	cpuPeriod            int64 = 100000
	cpuQuota             int64 = 200000
	pidsLimit            int64 = 256
	containerPath              = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	containerHostname          = "mcp-workspace"
	controlHostPath            = "/usr/local/lib/mcp-workspaced/mcp-control"
	controlContainerPath       = "/run/mcp-control"
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
		"--mount=type=bind,source=" + controlHostPath + ",destination=" + controlContainerPath + ",ro=true",
		"--mount=type=volume,source=" + volumeName(w.ID) + ",destination=/workspace",
		"--cgroups=enabled", "--cpu-period=100000", "--cpu-quota=200000",
		"--memory=2147483648", "--memory-swap=2147483648", "--pids-limit=256",
		"--sdnotify=ignore", "--systemd=false", "--init=false", "--restart=no",
		"--log-driver=k8s-file", "--log-opt=max-size=1048576", "--stop-timeout=0",
		"--hostname=" + containerHostname,
		"--unsetenv-all", "--env=" + containerPath, "--env=HOME=/root", "--env=LANG=C.UTF-8", "--env=HOSTNAME=" + containerHostname,
		"--entrypoint=" + controlContainerPath, image, "init",
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
	State         *struct {
		Status  string
		Running bool
	}
	Config *struct {
		Labels         map[string]string
		Hostname       string
		CreateCommand  []string
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
		IDMappings     *struct {
			UIDMap []string
			GIDMap []string
		}
		CgroupMode    string
		Cgroups       string
		CgroupManager string
		NetworkMode   string
		PortBindings  map[string]json.RawMessage
		CpuPeriod     int64
		CpuQuota      int64
		Memory        int64
		MemorySwap    int64
		PidsLimit     int64
		AutoRemove    bool
		Init          bool
		RestartPolicy struct{ Name string }
	}
}

func (c *containerInspection) identity() (Workspace, error) {
	if c.Config == nil {
		return Workspace{}, errors.New("missing workspace configuration")
	}
	return identityFromLabels(c.Config.Labels)
}

func identityFromLabels(l map[string]string) (Workspace, error) {
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
		{"service mounts only", c.verifyMounts() && len(h.Tmpfs) == 0 && len(h.Devices) == 0 && len(config.Secrets) == 0},
		{"security options", slices.Equal(h.SecurityOpt, []string{"no-new-privileges", "seccomp=/usr/share/containers/seccomp.json"})},
		{"private namespaces", h.PidMode == "private" && h.IpcMode == "private" && h.UTSMode == "private" && h.CgroupMode == "private"},
		{"private auto user namespace", c.verifyUserNamespace()},
		{"fixed hostname", config.Hostname == containerHostname},
		{"outbound network", h.NetworkMode == "slirp4netns" && len(h.PortBindings) == 0},
		{"resource limits", h.Cgroups == "default" && h.CgroupManager == "systemd" && h.CpuPeriod == cpuPeriod && h.CpuQuota == cpuQuota && h.Memory == memoryBytes && h.MemorySwap == memoryBytes && h.PidsLimit == pidsLimit},
		{"container user", config.User == "0:0"},
		{"container process", slices.Equal(config.Entrypoint, []string{controlContainerPath}) && slices.Equal(config.Cmd, []string{"init"})},
		{"disposable runtime", !h.AutoRemove && !h.Init && h.RestartPolicy.Name == "no" && !config.SystemdMode},
		{"sd-notify disabled", config.SdNotifyMode == "ignore" && config.SdNotifySocket == ""},
	}
	for _, check := range checks {
		if !check.ok {
			return fmt.Errorf("workspace profile assertion failed: %s", check.name)
		}
	}
	// Set HOSTNAME explicitly so Podman's start-time addition cannot change the
	// allowlist. No image/config/host environment survives --unsetenv-all. Compare as sets
	// because Podman does not promise a stable environment ordering.
	want := []string{containerPath, "HOME=/root", "LANG=C.UTF-8", "HOSTNAME=" + containerHostname}
	got := slices.Clone(config.Env)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		return errors.New("workspace profile assertion failed: container environment")
	}
	return nil
}

func (c *containerInspection) verifyMounts() bool {
	if len(c.Mounts) != 2 {
		return false
	}
	seen := map[string]bool{}
	workspaceSource := ""
	for _, raw := range c.Mounts {
		var mount struct {
			Type, Source, Destination, Name string
			RW                              *bool
		}
		if json.Unmarshal(raw, &mount) != nil || seen[mount.Destination] || mount.RW == nil {
			return false
		}
		seen[mount.Destination] = true
		switch mount.Destination {
		case controlContainerPath:
			if mount.Type != "bind" || mount.Source != controlHostPath || *mount.RW {
				return false
			}
		case "/workspace":
			if mount.Type != "volume" || mount.Name != volumeName(c.Config.Labels[idLabel]) || !*mount.RW {
				return false
			}
			workspaceSource = mount.Source
		default:
			return false
		}
	}
	// Binds is a redundant saved representation on some Podman versions.
	// Restrict it as well; effective mounts above remain the primary assertion.
	for _, bind := range c.HostConfig.Binds {
		parts := strings.Split(bind, ":")
		if len(parts) != 3 {
			return false
		}
		if parts[0] == controlHostPath && parts[1] == controlContainerPath && contains(strings.Split(parts[2], ","), "ro") {
			continue
		}
		// Some versions save the resolved volume source and omit the default
		// rw option. The effective volume name/RW flag is asserted above.
		volumeSource := parts[0] == volumeName(c.Config.Labels[idLabel]) || (workspaceSource != "" && parts[0] == workspaceSource)
		if volumeSource && parts[1] == "/workspace" && !contains(strings.Split(parts[2], ","), "ro") {
			continue
		}
		return false
	}
	return true
}

func (c *containerInspection) verifyUserNamespace() bool {
	h := c.HostConfig
	// Podman 5 persists auto allocation and ID mappings at create time, but
	// adds the OCI user namespace only when initializing the runtime. Inspect
	// calls this pre-runtime state "created". Recovery must also accept it for
	// an interrupted transaction, while initialized/running/stopped containers
	// must assert the effective private namespace.
	if h.UsernsMode != "private" && !(h.UsernsMode == "" && c.State != nil && c.State.Status == "created" && !c.State.Running) {
		return false
	}
	options := 0
	for _, arg := range c.Config.CreateCommand {
		if arg == "--userns" || strings.HasPrefix(arg, "--userns=") {
			if arg != "--userns=auto:size=65536" {
				return false
			}
			options++
		}
	}
	return options == 1 && h.IDMappings != nil && validAutoIDMap(h.IDMappings.UIDMap) && validAutoIDMap(h.IDMappings.GIDMap)
}

// Inspect encodes each mapping as container-ID:parent-ID:size. Auto mappings
// cover exactly 65536 container IDs and exclude ID 0 in the parent's rootless
// namespace (the service account). Allocation can span multiple parent ranges.
func validAutoIDMap(mappings []string) bool {
	type span struct{ start, end uint64 }
	var parents []span
	var next uint64
	for _, mapping := range mappings {
		parts := strings.Split(mapping, ":")
		if len(parts) != 3 {
			return false
		}
		var values [3]uint64
		for i, part := range parts {
			value, err := strconv.ParseUint(part, 10, 32)
			if err != nil {
				return false
			}
			values[i] = value
		}
		container, parent, size := values[0], values[1], values[2]
		if container != next || parent == 0 || size == 0 || next+size > 65536 || parent+size > 1<<32 {
			return false
		}
		for _, previous := range parents {
			if parent < previous.end && previous.start < parent+size {
				return false
			}
		}
		parents = append(parents, span{parent, parent + size})
		next += size
	}
	return next == 65536
}
