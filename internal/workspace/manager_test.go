package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

const testImageID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// A Podman-shaped response fixture, independent of createArgs. Default mounts
// such as /proc and /dev/shm are not user mounts in Podman's inspect API.
const inspectionFixture = `{
 "Id":"", "Image":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
 "EffectiveCaps":[], "BoundingCaps":[], "Mounts":[], "State":{"Status":"created","Running":false},
 "Config":{
   "Labels":{}, "WorkingDir":"/workspace", "User":"0:0",
   "Hostname":"mcp-workspace", "CreateCommand":["/usr/bin/podman","create","--userns=auto:size=65536"],
   "Env":["HOME=/root","LANG=C.UTF-8","PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin","HOSTNAME=mcp-workspace"],
   "Entrypoint":["/usr/bin/sleep"], "Cmd":["infinity"],
   "sdNotifyMode":"ignore", "sdNotifySocket":"", "SystemdMode":false
 },
 "HostConfig":{
   "Privileged":false, "ReadonlyRootfs":false, "Binds":[], "Tmpfs":{}, "Devices":[], "CapAdd":[],
   "SecurityOpt":["no-new-privileges","seccomp=/usr/share/containers/seccomp.json"],
   "PidMode":"private", "IpcMode":"private", "UTSMode":"private", "UsernsMode":"",
   "IDMappings":{"UidMap":["0:1:65536"],"GidMap":["0:1:65536"]},
   "CgroupMode":"private", "Cgroups":"default", "CgroupManager":"systemd", "NetworkMode":"slirp4netns",
   "PortBindings":{}, "CpuPeriod":100000, "CpuQuota":200000, "Memory":2147483648,
   "MemorySwap":2147483648, "PidsLimit":256, "RestartPolicy":{"Name":"no"}
 }
}`

type fakePodman struct {
	containers    map[string]*containerInspection
	calls         [][]string
	contexts      []error
	counts        map[string]int
	fail          func(context.Context, string, int) error
	mutate        func(*containerInspection)
	createOutput  *string
	persist       bool
	info          string
	listOutput    *string
	inspectOutput *string
	next          int
}

func newFake() *fakePodman {
	return &fakePodman{containers: make(map[string]*containerInspection), counts: make(map[string]int), persist: true,
		info: `{"host":{"security":{"rootless":true},"cgroupVersion":"v2","cgroupControllers":["cpu","memory","pids"]}}`}
}

func (f *fakePodman) Run(ctx context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(args))
	f.contexts = append(f.contexts, ctx.Err())
	key := args[0]
	if key == "container" {
		key += " " + args[1]
	}
	f.counts[key]++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var failure error
	if f.fail != nil {
		failure = f.fail(ctx, key, f.counts[key])
	}
	if key != "create" && failure != nil {
		return nil, failure
	}
	marshal := func(value any) ([]byte, error) { return json.Marshal(value) }
	switch key {
	case "info":
		return []byte(f.info), nil
	case "image":
		return []byte(`[{"Id":"` + testImageID + `","Config":{"Labels":{"` + labelPrefix + `image":"v0"}}}]`), nil
	case "ps":
		if f.listOutput != nil {
			return []byte(*f.listOutput), nil
		}
		out := []map[string]string{}
		for id, c := range f.containers {
			match := true
			for _, arg := range args {
				if filter, ok := strings.CutPrefix(arg, "--filter=label="); ok {
					label, value, _ := strings.Cut(filter, "=")
					if c.Config.Labels[label] != value {
						match = false
					}
				}
			}
			if match {
				out = append(out, map[string]string{"Id": id})
			}
		}
		return marshal(out)
	case "create":
		f.next++
		id := fmt.Sprintf("%064x", f.next)
		if f.persist {
			c := fixture()
			c.ID = id
			c.Config.CreateCommand = append([]string{"/usr/bin/podman"}, args...)
			for _, arg := range args {
				if label, ok := strings.CutPrefix(arg, "--label="); ok {
					name, value, _ := strings.Cut(label, "=")
					c.Config.Labels[name] = value
				}
			}
			if f.mutate != nil {
				f.mutate(c)
			}
			f.containers[id] = c
		}
		if f.createOutput != nil {
			return []byte(*f.createOutput), failure
		}
		return []byte(id + "\n"), failure
	case "container inspect":
		if f.inspectOutput != nil {
			return []byte(*f.inspectOutput), nil
		}
		c := f.containers[args[len(args)-1]]
		if c == nil {
			return nil, errors.New("no such container")
		}
		return marshal([]*containerInspection{c})
	case "start":
		c := f.containers[args[1]]
		// Podman 5.4 initializes the auto user namespace and injects HOSTNAME
		// into the runtime spec at start (container_internal_linux.go).
		c.HostConfig.UsernsMode = "private"
		c.State.Status, c.State.Running = "running", true
		c.State.Pid = new(int)
		*c.State.Pid = 123
		if !slices.ContainsFunc(c.Config.Env, func(value string) bool { return strings.HasPrefix(value, "HOSTNAME=") }) {
			c.Config.Env = append(c.Config.Env, "HOSTNAME="+c.Config.Hostname)
		}
		return []byte(args[1]), nil
	case "stop":
		c := f.containers[args[len(args)-1]]
		c.State.Status, c.State.Running, c.State.Pid = "exited", false, new(int)
		return nil, nil
	case "rm":
		delete(f.containers, args[len(args)-1])
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command: %v", args)
}

func fixture() *containerInspection {
	var c containerInspection
	if err := json.Unmarshal([]byte(inspectionFixture), &c); err != nil {
		panic(err)
	}
	return &c
}

func openFake(t *testing.T, f *fakePodman) *Manager {
	t.Helper()
	m, err := New(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCreateDestroyAndRestart(t *testing.T) {
	f := newFake()
	m := openFake(t, f)
	w, err := m.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !workspaceIDPattern.MatchString(w.ID) || len(f.containers) != 1 || w.CreatedAt.Location() != time.UTC {
		t.Fatalf("invalid workspace: %+v", w)
	}
	for id, c := range f.containers {
		if w.ID == id || !c.State.Running || c.Config.Labels[idLabel] != w.ID || c.Config.Labels[createdLabel] != w.CreatedAt.Format(time.RFC3339Nano) {
			t.Fatalf("unexpected container: %+v", c)
		}
		if c.HostConfig.UsernsMode != "private" || c.Config.Hostname != "mcp-workspace" || !slices.Contains(c.Config.Env, "HOSTNAME=mcp-workspace") {
			t.Fatalf("unexpected started profile: %+v", c)
		}
	}
	// New daemon receives the same opaque ID from labels, without touching compute.
	restarted := openFake(t, f)
	if got := restarted.entries[w.ID]; got == nil || !got.CreatedAt.Equal(w.CreatedAt) {
		t.Fatalf("recovery lost identity: %+v", got)
	}
	if f.counts["create"] != 1 || f.counts["start"] != 1 || f.counts["rm"] != 0 {
		t.Fatalf("unexpected lifecycle: %v", f.counts)
	}
	if err := restarted.Destroy(context.Background(), w.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.containers) != 0 {
		t.Fatal("destroy left a container")
	}
	if err := restarted.Destroy(context.Background(), w.ID); err != nil {
		t.Fatal(err)
	}
	if f.counts["rm"] != 1 {
		t.Fatal("idempotent destroy invoked remove again")
	}
	if err := restarted.Destroy(context.Background(), "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if err := openFake(t, f).Destroy(context.Background(), w.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("destroy tombstone survived restart: %v", err)
	}
}

func TestWorkspaceIDsAreUnique(t *testing.T) {
	f := newFake()
	m := openFake(t, f)
	seen := map[string]bool{}
	for range 20 {
		w, err := m.Create(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if seen[w.ID] || !workspaceIDPattern.MatchString(w.ID) {
			t.Fatalf("invalid or reused ID %s", w.ID)
		}
		seen[w.ID] = true
	}
}

func TestRecoveryOfStoppedWorkspace(t *testing.T) {
	f := newFake()
	m := openFake(t, f)
	w, err := m.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.containers {
		c.State.Status, c.State.Running = "exited", false
	}
	restarted := openFake(t, f)
	if restarted.entries[w.ID] == nil || f.counts["start"] != 1 {
		t.Fatal("stopped workspace was lost or restarted")
	}
	if err := restarted.Destroy(context.Background(), w.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCreationFailureRollback(t *testing.T) {
	for _, stage := range []string{"create", "container inspect", "start", "post-start inspect"} {
		t.Run(stage, func(t *testing.T) {
			f := newFake()
			m := openFake(t, f)
			f.fail = func(_ context.Context, op string, n int) error {
				if op == stage && n == 1 || stage == "post-start inspect" && op == "container inspect" && n == 2 {
					return errors.New("injected failure")
				}
				return nil
			}
			if _, err := m.Create(context.Background()); err == nil {
				t.Fatal("expected create failure")
			}
			if len(f.containers) != 0 || len(m.entries) != 0 || f.counts["rm"] != 1 {
				t.Fatalf("rollback failed: containers=%d registry=%d calls=%v", len(f.containers), len(m.entries), f.calls)
			}
		})
	}
	t.Run("failure before persistence", func(t *testing.T) {
		f := newFake()
		m := openFake(t, f)
		f.persist = false
		f.fail = func(_ context.Context, op string, _ int) error {
			if op == "create" {
				return errors.New("failure")
			}
			return nil
		}
		if _, err := m.Create(context.Background()); err == nil {
			t.Fatal("expected failure")
		}
		if len(m.entries) != 0 || f.counts["rm"] != 0 {
			t.Fatal("failed before persistence but retained state")
		}
	})
	t.Run("create error without returned ID", func(t *testing.T) {
		f := newFake()
		m := openFake(t, f)
		output := ""
		f.createOutput = &output
		f.fail = func(_ context.Context, op string, _ int) error {
			if op == "create" {
				return errors.New("lost create response")
			}
			return nil
		}
		if _, err := m.Create(context.Background()); err == nil {
			t.Fatal("expected failure")
		}
		if len(f.containers) != 0 || len(m.entries) != 0 || f.counts["rm"] != 1 {
			t.Fatal("failed to discover container without returned ID")
		}
	})
}

func TestCreateRejectsNonRunningStart(t *testing.T) {
	f := newFake()
	m := openFake(t, f)
	f.fail = func(_ context.Context, op string, count int) error {
		if op == "container inspect" && count == 2 {
			for _, c := range f.containers {
				c.State.Running = false
			}
		}
		return nil
	}
	if _, err := m.Create(context.Background()); err == nil {
		t.Fatal("published stopped compute")
	}
	if len(f.containers) != 0 || len(m.entries) != 0 {
		t.Fatal("did not roll back stopped compute")
	}
}

func TestUnusableCreateOutputDiscoveredByLabels(t *testing.T) {
	for _, output := range []string{"", "bad", "--all", strings.Repeat("f", 12), strings.Repeat("f", 64) + "\nnoise", strings.Repeat("b", 64)} {
		t.Run(output, func(t *testing.T) {
			f := newFake()
			m := openFake(t, f)
			// Unrelated user containers must survive rollback.
			unrelated := fixture()
			unrelated.ID = strings.Repeat("b", 64)
			f.containers[unrelated.ID] = unrelated
			f.createOutput = &output
			if _, err := m.Create(context.Background()); err == nil {
				t.Fatal("invalid container ID accepted")
			}
			if len(f.containers) != 1 || f.containers[unrelated.ID] == nil || len(m.entries) != 0 {
				t.Fatal("label discovery cleaned up the wrong state")
			}
		})
	}
}

func TestCancelledCreationUsesIndependentCleanupContext(t *testing.T) {
	for _, stage := range []string{"create", "container inspect", "start", "after start"} {
		t.Run(stage, func(t *testing.T) {
			f := newFake()
			m := openFake(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.fail = func(_ context.Context, op string, n int) error {
				if op == stage && n == 1 || stage == "after start" && op == "container inspect" && n == 2 {
					cancel()
					return context.Canceled
				}
				return nil
			}
			if _, err := m.Create(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel error: %v", err)
			}
			if len(f.containers) != 0 || len(m.entries) != 0 {
				t.Fatal("cancelled create left unreachable compute")
			}
			for i, call := range f.calls {
				if call[0] == "rm" && f.contexts[i] != nil {
					t.Fatal("cleanup inherited cancellation")
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := newFake()
	m := openFake(t, f)
	if _, err := m.Create(ctx); !errors.Is(err, context.Canceled) || f.counts["create"] != 0 {
		t.Fatalf("pre-cancelled request: %v", err)
	}
}

func TestCleanupFailuresPreserveExplicitDestroyPath(t *testing.T) {
	for _, failedOp := range []string{"rm", "ps", "container inspect"} {
		t.Run(failedOp, func(t *testing.T) {
			f := newFake()
			m := openFake(t, f)
			output := ""
			f.createOutput = &output
			f.fail = func(_ context.Context, op string, _ int) error {
				if op == failedOp {
					return errors.New("cleanup unavailable")
				}
				return nil
			}
			_, err := m.Create(context.Background())
			if err == nil || len(m.entries) != 1 || len(f.containers) != 1 {
				t.Fatalf("forgot failed cleanup: %v", err)
			}
			var id string
			for key := range m.entries {
				id = key
			}
			if !strings.Contains(err.Error(), "workspace_id="+id) {
				t.Fatalf("no public recovery handle: %v", err)
			}
			f.fail = nil
			if err := m.Destroy(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if len(f.containers) != 0 {
				t.Fatal("explicit destroy did not recover")
			}
		})
	}
}

func TestRemoveFailureRetainsRegistry(t *testing.T) {
	f := newFake()
	m := openFake(t, f)
	w, err := m.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.fail = func(_ context.Context, op string, _ int) error {
		if op == "rm" {
			return errors.New("remove failure")
		}
		return nil
	}
	if err := m.Destroy(context.Background(), w.ID); err == nil || m.entries[w.ID] == nil {
		t.Fatalf("forgot retry state: %v", err)
	}
	f.fail = nil
	if err := m.Destroy(context.Background(), w.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRejectsLabelsAndDuplicates(t *testing.T) {
	cases := map[string]func(*containerInspection){
		"missing ID":        func(c *containerInspection) { delete(c.Config.Labels, idLabel) },
		"invalid ID":        func(c *containerInspection) { c.Config.Labels[idLabel] = "ws_bad" },
		"raw container ID":  func(c *containerInspection) { c.Config.Labels[idLabel] = c.ID },
		"missing timestamp": func(c *containerInspection) { delete(c.Config.Labels, createdLabel) },
		"invalid timestamp": func(c *containerInspection) { c.Config.Labels[createdLabel] = "tomorrow" },
		"non UTC timestamp": func(c *containerInspection) { c.Config.Labels[createdLabel] = "2026-10-04T09:00:00+09:00" },
		"unknown profile":   func(c *containerInspection) { c.Config.Labels[profileLabel] = "v1" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			m := openFake(t, f)
			if _, err := m.Create(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, c := range f.containers {
				mutate(c)
			}
			if _, err := New(context.Background(), f); err == nil {
				t.Fatal("bad label accepted")
			}
			if f.counts["rm"] != 0 {
				t.Fatal("recovery mutated rejected container")
			}
		})
	}
	t.Run("duplicate workspace ID", func(t *testing.T) {
		f := newFake()
		m := openFake(t, f)
		w, err := m.Create(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		duplicate := fixture()
		duplicate.ID = strings.Repeat("b", 64)
		duplicate.Config.Labels = map[string]string{managedLabel: "true", idLabel: w.ID, createdLabel: w.CreatedAt.Format(time.RFC3339Nano), profileLabel: "v0"}
		f.containers[duplicate.ID] = duplicate
		if _, err := New(context.Background(), f); err == nil || !strings.Contains(err.Error(), "duplicate workspace ID") {
			t.Fatalf("duplicate accepted: %v", err)
		}
	})
}

func TestRecoveryOfInterruptedTransaction(t *testing.T) {
	f := newFake()
	m := openFake(t, f)
	f.fail = func(_ context.Context, op string, _ int) error {
		if op == "start" || op == "rm" {
			return errors.New("unavailable")
		}
		return nil
	}
	if _, err := m.Create(context.Background()); err == nil {
		t.Fatal("expected partial failure")
	}
	var id string
	for key := range m.entries {
		id = key
	}
	for _, c := range f.containers {
		if c.HostConfig.UsernsMode != "" || c.State.Status != "created" {
			t.Fatal("fixture did not preserve the pre-runtime auto namespace state")
		}
	}
	f.fail = nil
	restarted := openFake(t, f)
	if restarted.entries[id] == nil {
		t.Fatal("lost interrupted transaction on restart")
	}
	if err := restarted.Destroy(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidPodmanResponses(t *testing.T) {
	for _, output := range []string{"not JSON", `[{}]`, `[{"Id":"short"}]`, `[{"Id":"` + strings.Repeat("b", 64) + `"},{"Id":"` + strings.Repeat("b", 64) + `"}]`} {
		f := newFake()
		f.listOutput = &output
		if _, err := New(context.Background(), f); err == nil {
			t.Fatalf("bad discovery response accepted: %s", output)
		}
	}
	for _, output := range []string{"not JSON", `[]`, `[{}]`} {
		f := newFake()
		m := openFake(t, f)
		f.inspectOutput = &output
		if _, err := m.Create(context.Background()); err == nil {
			t.Fatalf("bad inspection response accepted: %s", output)
		}
		// Discovery cannot inspect ownership in this failure mode, so preserve
		// the handle rather than removing unverified user containers.
		if len(m.entries) != 1 {
			t.Fatal("forgot recovery state")
		}
	}
}

func TestPreparedHostRequired(t *testing.T) {
	for _, info := range []string{
		`{"host":{"security":{"rootless":false},"cgroupVersion":"v2","cgroupControllers":["cpu","memory","pids"]}}`,
		`{"host":{"security":{"rootless":true},"cgroupVersion":"v1","cgroupControllers":["cpu","memory","pids"]}}`,
		`{"host":{"security":{"rootless":true},"cgroupVersion":"v2","cgroupControllers":["memory","pids"]}}`,
		`{}`, `bad JSON`,
	} {
		f := newFake()
		f.info = info
		if _, err := New(context.Background(), f); err == nil {
			t.Fatalf("unsafe host accepted: %s", info)
		}
		if f.counts["create"] != 0 {
			t.Fatal("provisioned an unprepared host")
		}
	}
}
