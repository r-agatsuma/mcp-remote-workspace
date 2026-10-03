package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakePodman struct {
	containers map[string]container
	calls      [][]string
	fail       string
	mutate     func(*container)
}

func (f *fakePodman) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(args))
	if args[0] == f.fail {
		return nil, errors.New("simulated Podman failure")
	}
	switch args[0] {
	case "info":
		return []byte(`{"host":{"security":{"rootless":true},"cgroupVersion":"v2","serviceIsRemote":false}}`), nil
	case "ps":
		var ids []string
		for id, c := range f.containers {
			if c.Config.Labels[managedLabel] == "true" {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		return []byte(strings.Join(ids, "\n")), nil
	case "create":
		if f.containers == nil {
			f.containers = make(map[string]container)
		}
		id := fmt.Sprintf("%064x", len(f.containers)+1)
		c := safeContainer(id)
		for _, arg := range args {
			if label, ok := strings.CutPrefix(arg, "--label="); ok {
				key, value, _ := strings.Cut(label, "=")
				c.Config.Labels[key] = value
			}
		}
		if f.mutate != nil {
			f.mutate(&c)
		}
		f.containers[id] = c
		return []byte(id + "\n"), nil
	case "container":
		c, ok := f.containers[args[2]]
		if !ok {
			return nil, errors.New("container not found")
		}
		return json.Marshal([]container{c})
	case "start":
		return []byte(args[1]), nil
	case "rm":
		delete(f.containers, args[len(args)-1])
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected command %v", args)
	}
}

// Representative Podman inspect data. Nomap appears as "private" in inspect.
func safeContainer(id string) container {
	var c container
	c.ID, c.ImageName = id, Image
	c.Config.Labels = map[string]string{managedLabel: "true", idLabel: "ws_" + strings.Repeat("a", 64), createdLabel: "2026-10-04T00:00:00Z"}
	c.Config.WorkingDir, c.Config.User = "/workspace", "0:0"
	c.Config.Env = []string{"HOME=/root", "PATH=" + containerPath, "container=podman"}
	c.HostConfig.NetworkMode, c.HostConfig.PidMode, c.HostConfig.IpcMode, c.HostConfig.UsernsMode = "slirp4netns", "private", "private", "private"
	c.HostConfig.Memory, c.HostConfig.PidsLimit, c.HostConfig.CpuPeriod, c.HostConfig.CpuQuota = 1073741824, 256, 100000, 200000
	c.HostConfig.SecurityOpt = []string{"no-new-privileges"}
	return c
}

func TestCreateRecoverDestroy(t *testing.T) {
	ctx := context.Background()
	f := &fakePodman{}
	p, err := newPodman(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	w, err := p.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !workspaceIDPattern.MatchString(w.ID) || w.CreatedAt.IsZero() || w.CreatedAt.Location() != time.UTC || len(f.containers) != 1 {
		t.Fatalf("workspace = %#v, containers = %#v", w, f.containers)
	}
	r := p.workspaces[w.ID]
	if w.ID == r.containerID {
		t.Fatal("public ID is a container ID")
	}
	var create, start int
	for _, args := range f.calls {
		if args[0] == "create" {
			create++
			for _, want := range []string{"--pull=never", "--privileged=false", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--userns=nomap", "--user=0:0", "--pid=private", "--ipc=private", "--network=slirp4netns:allow_host_loopback=false", "--cgroups=enabled", "--cpu-period=100000", "--cpu-quota=200000", "--memory=1073741824", "--pids-limit=256", "--image-volume=ignore", "--http-proxy=false", "--unsetenv-all", "--workdir=/workspace", Image} {
				if !slices.Contains(args, want) {
					t.Errorf("missing policy argument %q", want)
				}
			}
			for _, arg := range args {
				for _, forbidden := range []string{"--mount", "--volume", "--device", "--cap-add", "--env-host", "--privileged=true", "--network=host", "--pid=host", "--ipc=host"} {
					if strings.HasPrefix(arg, forbidden) {
						t.Errorf("unsafe argument %q", arg)
					}
				}
			}
		}
		if args[0] == "start" {
			start++
		}
	}
	if create != 1 || start != 1 {
		t.Fatalf("create/start calls: %d/%d", create, start)
	}
	// Restart with no access to the previous in-memory state.
	recovered, err := newPodman(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recovered.workspaces[w.ID], r) {
		t.Fatalf("recovered = %#v, want %#v", recovered.workspaces, r)
	}
	if err := recovered.Destroy(ctx, r.containerID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("raw container ID: %v", err)
	}
	if err := recovered.Destroy(ctx, "--all"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ID: %v", err)
	}
	if err := recovered.Destroy(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.containers) != 0 || len(recovered.workspaces) != 0 {
		t.Fatal("destroy left runtime state")
	}
	before := len(f.calls)
	if err := recovered.Destroy(ctx, w.ID); err != nil {
		t.Fatalf("duplicate destroy: %v", err)
	}
	if len(f.calls) != before {
		t.Fatal("duplicate destroy invoked Podman")
	}
	// A removed workspace is unknown after restart; tombstones are not persisted.
	restarted, err := newPodman(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Destroy(ctx, w.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("destroy after restart: %v", err)
	}
}

func TestCreateIDsAreUnique(t *testing.T) {
	f := &fakePodman{}
	p, err := newPodman(context.Background(), f.run)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for range 20 {
		w, err := p.Create(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if seen[w.ID] {
			t.Fatal("duplicate opaque ID")
		}
		seen[w.ID] = true
	}
}

func TestPodmanFailures(t *testing.T) {
	ctx := context.Background()
	for _, op := range []string{"info", "ps"} {
		t.Run("startup/"+op, func(t *testing.T) {
			f := &fakePodman{fail: op}
			if p, err := newPodman(ctx, f.run); err == nil || p != nil {
				t.Fatalf("startup: %v, %v", p, err)
			}
		})
	}
	for _, op := range []string{"create", "container", "start"} {
		t.Run("create/"+op, func(t *testing.T) {
			f := &fakePodman{}
			p, err := newPodman(ctx, f.run)
			if err != nil {
				t.Fatal(err)
			}
			f.fail = op
			if w, err := p.Create(ctx); err == nil || w.ID != "" {
				t.Fatalf("create: %#v, %v", w, err)
			}
			if len(p.workspaces) != 0 || len(f.containers) != 0 {
				t.Fatal("failed create left runtime state")
			}
			count := 0
			for _, call := range f.calls {
				if call[0] == op {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("failed operation was retried: %d", count)
			}
		})
	}
	t.Run("destroy", func(t *testing.T) {
		f := &fakePodman{}
		p, _ := newPodman(ctx, f.run)
		w, err := p.Create(ctx)
		if err != nil {
			t.Fatal(err)
		}
		f.fail = "rm"
		if err := p.Destroy(ctx, w.ID); err == nil {
			t.Fatal("destroy failure was hidden")
		}
		if len(p.workspaces) != 1 || len(f.containers) != 1 || p.destroyed[w.ID] {
			t.Fatal("failed destroy discarded state")
		}
		f.fail = ""
		if err := p.Destroy(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rollback", func(t *testing.T) {
		f := &fakePodman{mutate: func(c *container) { c.HostConfig.Privileged = true }}
		p, _ := newPodman(ctx, f.run)
		f.fail = "rm"
		if _, err := p.Create(ctx); err == nil || !strings.Contains(err.Error(), "rollback failed") {
			t.Fatalf("rollback error: %v", err)
		}
		if len(p.workspaces) != 1 || len(f.containers) != 1 {
			t.Fatal("lost failed rollback state")
		}
		for id := range p.workspaces {
			f.fail = ""
			if err := p.Destroy(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("externally removed", func(t *testing.T) {
		f := &fakePodman{}
		p, _ := newPodman(ctx, f.run)
		w, err := p.Create(ctx)
		if err != nil {
			t.Fatal(err)
		}
		delete(f.containers, p.workspaces[w.ID].containerID)
		if err := p.Destroy(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestStartupRejectsUnsafeRuntime(t *testing.T) {
	for _, info := range []string{`{}`, `not-json`, `{"host":{"security":{"rootless":false},"cgroupVersion":"v2"}}`, `{"host":{"security":{"rootless":true},"cgroupVersion":"v1"}}`, `{"host":{"security":{"rootless":true},"cgroupVersion":"v2","serviceIsRemote":true}}`} {
		calls := 0
		_, err := newPodman(context.Background(), func(context.Context, ...string) ([]byte, error) { calls++; return []byte(info), nil })
		if err == nil || calls != 1 {
			t.Fatalf("unsafe runtime accepted: %s (%v)", info, err)
		}
	}
}

func TestIsolationRejectedBeforeStartAndOnRecovery(t *testing.T) {
	cases := map[string]func(*container){
		"host home mount": func(c *container) {
			c.Mounts = []json.RawMessage{json.RawMessage(`{"Type":"bind","Source":"/home/user","Destination":"/host"}`)}
		},
		"engine socket bind": func(c *container) {
			c.HostConfig.Binds = []json.RawMessage{json.RawMessage(`"/run/podman/podman.sock:/podman.sock"`)}
		},
		"device":          func(c *container) { c.HostConfig.Devices = []json.RawMessage{json.RawMessage(`{}`)} },
		"privileged":      func(c *container) { c.HostConfig.Privileged = true },
		"host network":    func(c *container) { c.HostConfig.NetworkMode = "host" },
		"host PID":        func(c *container) { c.HostConfig.PidMode = "host" },
		"host IPC":        func(c *container) { c.HostConfig.IpcMode = "host" },
		"user namespace":  func(c *container) { c.HostConfig.UsernsMode = "" },
		"capabilities":    func(c *container) { c.BoundingCaps = []string{"CAP_SYS_ADMIN"} },
		"credentials":     func(c *container) { c.Config.Env = append(c.Config.Env, "GITHUB_TOKEN=secret") },
		"no memory limit": func(c *container) { c.HostConfig.Memory = 0 },
		"no CPU limit":    func(c *container) { c.HostConfig.CpuQuota = 0 },
		"no pids limit":   func(c *container) { c.HostConfig.PidsLimit = 0 },
		"new privileges":  func(c *container) { c.HostConfig.SecurityOpt = nil },
		"image":           func(c *container) { c.ImageName = "arbitrary-image" },
		"ID label":        func(c *container) { c.Config.Labels[idLabel] = "arbitrary" },
		"timestamp":       func(c *container) { c.Config.Labels[createdLabel] = "invalid" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakePodman{mutate: mutate}
			p, err := newPodman(context.Background(), f.run)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Create(context.Background()); err == nil {
				t.Fatal("unsafe create succeeded")
			}
			for _, call := range f.calls {
				if call[0] == "start" {
					t.Fatal("unsafe container was started")
				}
			}
			id := strings.Repeat("b", 64)
			c := safeContainer(id)
			mutate(&c)
			f.containers[id] = c
			if _, err := newPodman(context.Background(), f.run); err == nil {
				t.Fatal("unsafe recovery succeeded")
			}
		})
	}
}

func TestRecoveryFailures(t *testing.T) {
	for _, tc := range []string{"inspect failure", "duplicate workspace IDs", "malformed list", "malformed inspect"} {
		t.Run(tc, func(t *testing.T) {
			id := strings.Repeat("b", 64)
			f := &fakePodman{containers: map[string]container{id: safeContainer(id)}}
			if tc == "inspect failure" {
				f.fail = "container"
			}
			if tc == "duplicate workspace IDs" {
				second := strings.Repeat("c", 64)
				f.containers[second] = safeContainer(second)
			}
			run := func(ctx context.Context, args ...string) ([]byte, error) {
				if tc == "malformed list" && args[0] == "ps" {
					return []byte("--all"), nil
				}
				if tc == "malformed inspect" && args[0] == "container" {
					return []byte("{}"), nil
				}
				return f.run(ctx, args...)
			}
			if p, err := newPodman(context.Background(), run); err == nil || p != nil {
				t.Fatalf("recovery: %v, %v", p, err)
			}
		})
	}
}

func TestCommandBoundary(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
test "$1" = --remote=false || exit 2
test "$2" = --default-mounts-file=/dev/null || exit 3
test -z "$CONTAINER_HOST$CONTAINER_CONNECTION$CONTAINER_SSHKEY$DOCKER_HOST" || exit 4
printf 'result'
printf 'diagnostic' >&2
test "$3" != fail
`
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, key := range []string{"CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINER_SSHKEY", "DOCKER_HOST"} {
		t.Setenv(key, "remote-secret")
	}
	data, err := runPodman(context.Background(), "info")
	if err != nil || string(data) != "result" {
		t.Fatalf("command output = %q, error = %v", data, err)
	}
	if _, err := runPodman(context.Background(), "fail"); err == nil || !strings.Contains(err.Error(), "diagnostic") {
		t.Fatalf("missing command diagnostic: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runPodman(ctx, "info"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled command: %v", err)
	}
}
