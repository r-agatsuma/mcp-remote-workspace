package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestProfileRejectsDriftAtCreateAndRecovery(t *testing.T) {
	trueValue := true
	cases := map[string]func(*containerInspection){
		"replacement image":        func(c *containerInspection) { c.Image = strings.Repeat("b", 64) },
		"readonly rootfs":          func(c *containerInspection) { c.HostConfig.ReadonlyRootfs = &trueValue },
		"missing rootfs assertion": func(c *containerInspection) { c.HostConfig.ReadonlyRootfs = nil },
		"wrong cwd":                func(c *containerInspection) { c.Config.WorkingDir = "/" },
		"privileged":               func(c *containerInspection) { c.HostConfig.Privileged = &trueValue },
		"effective capabilities":   func(c *containerInspection) { c.EffectiveCaps = []string{"CAP_SYS_ADMIN"} },
		"bounding capabilities":    func(c *containerInspection) { c.BoundingCaps = []string{"CAP_CHOWN"} },
		"cap add":                  func(c *containerInspection) { c.HostConfig.CapAdd = []string{"CAP_NET_RAW"} },
		"host home mount": func(c *containerInspection) {
			c.Mounts = []json.RawMessage{json.RawMessage(`{"Type":"bind","Source":"/home/service","Destination":"/host"}`)}
		},
		"socket bind":     func(c *containerInspection) { c.HostConfig.Binds = []string{"/run/podman/podman.sock:/engine.sock"} },
		"workspace tmpfs": func(c *containerInspection) { c.HostConfig.Tmpfs = map[string]string{"/workspace": "ro"} },
		"host device": func(c *containerInspection) {
			c.HostConfig.Devices = []json.RawMessage{json.RawMessage(`{"PathOnHost":"/dev/sda"}`)}
		},
		"credential secret": func(c *containerInspection) {
			c.Config.Secrets = []json.RawMessage{json.RawMessage(`{"Name":"token"}`)}
		},
		"new privileges": func(c *containerInspection) {
			c.HostConfig.SecurityOpt = []string{"seccomp=/usr/share/containers/seccomp.json"}
		},
		"unconfined seccomp": func(c *containerInspection) {
			c.HostConfig.SecurityOpt = []string{"no-new-privileges", "seccomp=unconfined"}
		},
		"host PID":             func(c *containerInspection) { c.HostConfig.PidMode = "host" },
		"shared IPC":           func(c *containerInspection) { c.HostConfig.IpcMode = "shareable" },
		"host user namespace":  func(c *containerInspection) { c.HostConfig.UsernsMode = "host" },
		"missing ID mappings":  func(c *containerInspection) { c.HostConfig.IDMappings = nil },
		"missing UID map":      func(c *containerInspection) { c.HostConfig.IDMappings.UIDMap = nil },
		"missing GID map":      func(c *containerInspection) { c.HostConfig.IDMappings.GIDMap = nil },
		"service UID mapped":   func(c *containerInspection) { c.HostConfig.IDMappings.UIDMap = []string{"0:0:65536"} },
		"service GID mapped":   func(c *containerInspection) { c.HostConfig.IDMappings.GIDMap = []string{"0:0:65536"} },
		"wrong namespace size": func(c *containerInspection) { c.HostConfig.IDMappings.UIDMap = []string{"0:1:65535"} },
		"missing auto option":  func(c *containerInspection) { c.Config.CreateCommand = nil },
		"conflicting userns": func(c *containerInspection) {
			c.Config.CreateCommand = append(c.Config.CreateCommand, "--userns=host")
		},
		"host UTS":              func(c *containerInspection) { c.HostConfig.UTSMode = "host" },
		"host cgroup namespace": func(c *containerInspection) { c.HostConfig.CgroupMode = "host" },
		"host network":          func(c *containerInspection) { c.HostConfig.NetworkMode = "host" },
		"network disabled":      func(c *containerInspection) { c.HostConfig.NetworkMode = "none" },
		"published ports": func(c *containerInspection) {
			c.HostConfig.PortBindings = map[string]json.RawMessage{"80/tcp": json.RawMessage(`[]`)}
		},
		"cgroups disabled":          func(c *containerInspection) { c.HostConfig.Cgroups = "disabled" },
		"unlimited CPU":             func(c *containerInspection) { c.HostConfig.CpuQuota = -1 },
		"changed CPU period":        func(c *containerInspection) { c.HostConfig.CpuPeriod = 200000 },
		"unlimited memory":          func(c *containerInspection) { c.HostConfig.Memory = 0 },
		"unlimited swap":            func(c *containerInspection) { c.HostConfig.MemorySwap = -1 },
		"unlimited pids":            func(c *containerInspection) { c.HostConfig.PidsLimit = -1 },
		"user override":             func(c *containerInspection) { c.Config.User = "1000" },
		"entrypoint override":       func(c *containerInspection) { c.Config.Entrypoint = []string{"/bin/sh"} },
		"notify socket":             func(c *containerInspection) { c.Config.SdNotifySocket = "/run/host/notify" },
		"sdnotify forwarding":       func(c *containerInspection) { c.Config.SdNotifyMode = "container" },
		"NOTIFY_SOCKET environment": func(c *containerInspection) { c.Config.Env = append(c.Config.Env, "NOTIFY_SOCKET=/host/notify") },
		"LISTEN_FDS environment":    func(c *containerInspection) { c.Config.Env = append(c.Config.Env, "LISTEN_FDS=3") },
		"proxy environment":         func(c *containerInspection) { c.Config.Env = append(c.Config.Env, "HTTP_PROXY=http://host") },
		"hostname override":         func(c *containerInspection) { c.Config.Hostname = "host-name" },
		"HOSTNAME override":         func(c *containerInspection) { c.Config.Env = append(c.Config.Env, "HOSTNAME=host-name") },
		"auto removal":              func(c *containerInspection) { c.HostConfig.AutoRemove = true },
		"systemd mode":              func(c *containerInspection) { c.Config.SystemdMode = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			m := openFake(t, f)
			f.mutate = mutate
			if _, err := m.Create(context.Background()); err == nil {
				t.Fatal("unsafe profile accepted during create")
			}
			if f.counts["start"] != 0 || len(f.containers) != 0 {
				t.Fatal("unsafe profile started or rollback failed")
			}
			f.mutate = nil
			if _, err := m.Create(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, c := range f.containers {
				mutate(c)
			}
			if _, err := New(context.Background(), f); err == nil {
				t.Fatal("unsafe profile recovered after restart")
			}
		})
	}
}

func TestFixedCreationOptions(t *testing.T) {
	f := newFake()
	m := openFake(t, f)
	if _, err := m.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	var args []string
	for _, call := range f.calls {
		if call[0] == "create" {
			args = call
		}
	}
	for _, option := range []string{
		"--pull=never", "--cap-drop=ALL", "--privileged=false", "--security-opt=no-new-privileges",
		"--pid=private", "--ipc=private", "--userns=auto:size=65536", "--uts=private", "--cgroupns=private",
		"--network=slirp4netns:allow_host_loopback=false", "--http-proxy=false", "--read-only=false",
		"--workdir=/workspace", "--image-volume=ignore", "--cpu-period=100000", "--cpu-quota=200000",
		"--memory=2147483648", "--memory-swap=2147483648", "--pids-limit=256", "--sdnotify=ignore", "--unsetenv-all",
		"--hostname=mcp-workspace", "--env=HOSTNAME=mcp-workspace",
	} {
		if !slices.Contains(args, option) {
			t.Errorf("missing required option %s", option)
		}
	}
	for _, arg := range args {
		for _, forbidden := range []string{"--volume", "--mount", "--device", "--cap-add", "--env-host", "--env-file", "--pod="} {
			if strings.HasPrefix(arg, forbidden) {
				t.Errorf("unsafe option: %s", arg)
			}
		}
	}
	if args[len(args)-2] != testImageID || slices.Contains(args, Image) {
		t.Fatal("create did not pin the local image ID")
	}
}

func TestAutoUserNamespaceInspectionStates(t *testing.T) {
	for _, tc := range []struct {
		name, status, mode string
		running, valid     bool
	}{
		{"allocated before start", "created", "", false, true},
		{"initialized runtime", "initialized", "private", false, true},
		{"running runtime", "running", "private", true, true},
		{"stopped runtime", "stopped", "private", false, true},
		{"exited runtime", "exited", "private", false, true},
		{"missing initialized namespace", "initialized", "", false, false},
		{"missing running namespace", "running", "", true, false},
		{"missing stopped namespace", "stopped", "", false, false},
		{"missing exited namespace", "exited", "", false, false},
		{"inconsistent running state", "created", "", true, false},
		{"unknown state", "", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture()
			c.State.Status, c.State.Running = tc.status, tc.running
			c.HostConfig.UsernsMode = tc.mode
			if err := c.verify(testImageID); (err == nil) != tc.valid {
				t.Fatalf("profile valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

func TestAutoIDMappings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mappings []string
		valid    bool
	}{
		{"allocated", []string{"0:1:65536"}, true},
		{"multiple ranges", []string{"0:100001:32768", "32768:1:32768"}, true},
		{"missing", nil, false},
		{"service account", []string{"0:0:65536"}, false},
		{"malformed", []string{"0:1"}, false},
		{"negative", []string{"0:-1:65536"}, false},
		{"invalid number", []string{"0:x:65536"}, false},
		{"zero size", []string{"0:1:0"}, false},
		{"wrong size", []string{"0:1:65535"}, false},
		{"excessive size", []string{"0:1:65537"}, false},
		{"container gap", []string{"1:1:65536"}, false},
		{"container overlap", []string{"0:1:32768", "0:100001:32768"}, false},
		{"parent overlap", []string{"0:1:32768", "32768:32768:32768"}, false},
		{"parent overflow", []string{"0:4294967295:65536"}, false},
		{"integer overflow", []string{"0:4294967296:65536"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validAutoIDMap(tc.mappings); got != tc.valid {
				t.Fatalf("valid=%v, want %v", got, tc.valid)
			}
		})
	}
}

func TestStartedProfileDriftRollsBack(t *testing.T) {
	for name, mutate := range map[string]func(*containerInspection){
		"missing runtime user namespace": func(c *containerInspection) { c.HostConfig.UsernsMode = "" },
		"changed hostname":               func(c *containerInspection) { c.Config.Hostname = "host-name" },
		"unexpected HOSTNAME": func(c *containerInspection) {
			c.Config.Env = slices.DeleteFunc(c.Config.Env, func(v string) bool { return strings.HasPrefix(v, "HOSTNAME=") })
			c.Config.Env = append(c.Config.Env, "HOSTNAME=host-name")
		},
		"unexpected host environment": func(c *containerInspection) { c.Config.Env = append(c.Config.Env, "NOTIFY_SOCKET=/host/socket") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			m := openFake(t, f)
			f.fail = func(_ context.Context, op string, count int) error {
				if op == "container inspect" && count == 2 {
					for _, c := range f.containers {
						mutate(c)
					}
				}
				return nil
			}
			if _, err := m.Create(context.Background()); err == nil {
				t.Fatal("published a workspace with post-start profile drift")
			}
			if f.counts["start"] != 1 || f.counts["rm"] != 1 || len(f.containers) != 0 || len(m.entries) != 0 {
				t.Fatal("started workspace was not rolled back")
			}
		})
	}
}

func TestDeterministicProcessEnvironmentAndConfiguration(t *testing.T) {
	dir := t.TempDir()
	hostConfig := filepath.Join(dir, "host.conf")
	if err := os.WriteFile(hostConfig, []byte(`[containers]
devices = ["/dev/sda"]
read_only = true
mounts = ["type=bind,source=/home,destination=/home"]
env_host = true
[engine]
remote = true
active_service = "host-remote"
hooks_dir = ["/host/hooks"]
`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"NOTIFY_SOCKET", "LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES",
		"CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINER_SSHKEY", "DOCKER_HOST", "DOCKER_CONTEXT",
		"CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE", "CONTAINERS_STORAGE_CONF", "PODMAN_USERNS",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy",
		"XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "HOME", "PATH", "TMPDIR",
		"SSH_AUTH_SOCK", "GITHUB_TOKEN", "OPENAI_API_KEY", "LD_PRELOAD",
	} {
		t.Setenv(name, hostConfig)
	}
	env := podmanEnvironment("/home/dedicated-service", "1001", "/private/config")
	want := []string{
		"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "HOME=/home/dedicated-service",
		"XDG_RUNTIME_DIR=/run/user/1001", "XDG_CONFIG_HOME=/private/config",
		"CONTAINERS_CONF=/private/config/containers.conf", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1001/bus",
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("inherited host environment: %v", env)
	}
	p := &podman{dir: "/private/config", env: env}
	cmd := p.command(context.Background(), "info", "--format=json")
	if cmd.Path != "/usr/bin/podman" || cmd.Dir != "/" || !reflect.DeepEqual(cmd.Env, want) {
		t.Fatalf("nonlocal command: %+v", cmd)
	}
	for _, option := range []string{"--remote=false", "--hooks-dir=/private/config/hooks", "--default-mounts-file=/dev/null"} {
		if !slices.Contains(cmd.Args, option) {
			t.Fatalf("missing deterministic boundary %s", option)
		}
	}
	config := string(containersConfig)
	for _, setting := range []string{
		"devices = []", "mounts = []", "volumes = []", "hooks_dir = []", "cdi_spec_dirs = []",
		"read_only = false", "privileged = false", "default_capabilities = []", "env_host = false", "http_proxy = false",
		"remote = false", `active_service = ""`, "env = []", "network_cmd_options = []",
	} {
		if !strings.Contains(config, setting) {
			t.Fatalf("missing fixed config setting: %s", setting)
		}
	}
}

func TestProductionRejectsRoot(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to verify refusal")
	}
	if _, err := newPodman(); err == nil {
		t.Fatal("rootful production runner accepted")
	}
}

func TestProjectConfigurationPersistsAndRejectsOverrides(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "project")
	if err := prepareConfiguration(dir); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "containers.conf")
	first, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareConfiguration(dir); err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(first, second) || !first.ModTime().Equal(second.ModTime()) {
		t.Fatal("startup rewrote existing project configuration")
	}
	hostile := []byte("[containers]\ndevices = [\"/dev/sda\"]\nread_only = true\n[engine]\nremote = true\n")
	if err := os.WriteFile(configPath, hostile, 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareConfiguration(dir); err == nil {
		t.Fatal("override accepted")
	}
	got, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(got, hostile) {
		t.Fatal("conflicting file was overwritten")
	}
	// Only the test repairs its own fixture. The service refuses to repair it.
	if err := os.WriteFile(configPath, containersConfig, 0600); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(dir, "hooks", "host-hook.json")
	if err := os.WriteFile(hookPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareConfiguration(dir); err == nil {
		t.Fatal("nonempty OCI hook directory accepted")
	}
	if _, err := os.Stat(hookPath); err != nil {
		t.Fatal("service removed a conflicting hook")
	}
}

func TestProjectConfigurationRejectsSymlinks(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "project")
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	if err := prepareConfiguration(dir); err == nil {
		t.Fatal("configuration directory symlink accepted")
	}
	actual := filepath.Join(parent, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	targetFile := filepath.Join(parent, "host.conf")
	if err := os.WriteFile(targetFile, containersConfig, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetFile, filepath.Join(actual, "containers.conf")); err != nil {
		t.Fatal(err)
	}
	if err := prepareConfiguration(actual); err == nil {
		t.Fatal("configuration file symlink accepted")
	}
}
