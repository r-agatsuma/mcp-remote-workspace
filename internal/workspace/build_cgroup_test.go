//go:build linux

package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// No host systemd or cgroup mutation: the real adapter runs against this user
// manager and independent kernel observation fixture.
type fakeUserManager struct {
	mu                                         sync.Mutex
	units                                      map[string]map[string]string
	groups                                     map[string]buildGroupSample
	calls                                      [][]string
	createReplyLost                            bool
	createErr, stopErr                         error
	wrongLimit, liveTasks, retain, badIdentity bool
}

func cgroupFixture(root string) (*systemdBuildCgroups, *fakeUserManager) {
	f := &fakeUserManager{units: map[string]map[string]string{}, groups: map[string]buildGroupSample{}}
	return &systemdBuildCgroups{dataDir: root, bus: f, control: f, fs: f}, f
}

func (f *fakeUserManager) run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(args) > 1 && args[1] == "call" {
		if f.createErr != nil {
			return nil, f.createErr
		}
		if len(args) != 20 || !reflect.DeepEqual(args[:8], []string{"--user", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))", args[7]}) || args[8] != "fail" || args[9] != "3" || !reflect.DeepEqual(args[10:12], []string{"Description", "s"}) || !reflect.DeepEqual(args[13:15], []string{"TasksMax", "t"}) || !reflect.DeepEqual(args[16:], []string{"CollectMode", "s", "inactive-or-failed", "0"}) {
			return nil, fmt.Errorf("unexpected transient slice request: %v", args)
		}
		unit := args[7]
		if f.units[unit] != nil {
			return nil, injected
		}
		limit, err := strconv.ParseUint(args[15], 10, 64)
		if err != nil {
			return nil, err
		}
		f.units[unit] = map[string]string{"Id": unit, "LoadState": "loaded", "ActiveState": "active", "Transient": "yes", "Description": args[12], "ControlGroup": buildGroupPath(unit), "TasksMax": args[15]}
		f.groups[unit] = buildGroupSample{Exists: true, Limit: limit, Device: 1, Inode: 2}
		if f.wrongLimit {
			s := f.groups[unit]
			s.Limit++
			f.groups[unit] = s
		}
		if f.badIdentity {
			f.units[unit]["Description"] = "stale unit"
		}
		if f.createReplyLost {
			return nil, injected
		}
		return []byte(`o "/org/freedesktop/systemd1/job/1"`), nil
	}
	if slices.Contains(args, "list-units") {
		var out strings.Builder
		for unit := range f.units {
			fmt.Fprintf(&out, "%s loaded active active exact build\n", unit)
		}
		return []byte(out.String()), nil
	}
	if slices.Contains(args, "show") {
		unit := args[len(args)-1]
		v := f.units[unit]
		if v == nil {
			return nil, injected
		}
		var out strings.Builder
		for _, k := range []string{"Id", "LoadState", "ActiveState", "Transient", "Description", "ControlGroup", "TasksMax"} {
			fmt.Fprintf(&out, "%s=%s\n", k, v[k])
		}
		return []byte(out.String()), nil
	}
	if slices.Contains(args, "stop") {
		if !reflect.DeepEqual(args[:len(args)-1], []string{"--user", "--no-pager", "--no-ask-password", "stop", "--"}) {
			return nil, injected
		}
		if f.stopErr != nil {
			return nil, f.stopErr
		}
		unit := args[len(args)-1]
		if !f.retain {
			delete(f.units, unit)
			delete(f.groups, unit)
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected user-manager command: %v", args)
}
func (f *fakeUserManager) Sample(_ context.Context, unit string) (buildGroupSample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.groups[unit]
	if f.liveTasks && s.Exists {
		s.Tasks = 1
		s.Populated = true
	}
	return s, nil
}
func (f *fakeUserManager) Units(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := []string{}
	for unit := range f.groups {
		v = append(v, unit)
	}
	return v, nil
}
func (f *fakeUserManager) stops() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if slices.Contains(call, "stop") {
			n++
		}
	}
	return n
}

func TestBuildPIDParentTrustedPolicyAndInvocation(t *testing.T) {
	if ReferencePolicy().PIDsMax != 512 {
		t.Fatal("reference ceiling changed")
	}
	for _, limit := range []uint64{512, 64} {
		t.Run(strconv.FormatUint(limit, 10), func(t *testing.T) {
			e := Exact{identity(t, "s", "pids").Workspace(), Generation(strings.Repeat("d", 64))}
			plan, err := compileDockerfile("FROM scratch\nARG CGROUP_PARENT=evil.slice\nENV TasksMax=99999\nRUN echo --cgroup-parent=evil.slice:foo:bar\n", nil)
			if err != nil {
				t.Fatal(err)
			}
			p, stub := podmanStub(t, infoStep(), imageListStep(t))
			cli := &buildCLIStub{stubCLI: stub, result: BackendResult{DefinitelyCompleted, injected}}
			p.runner = cli
			path := configureBuildFixture(t, p, e, plan)
			m, f := cgroupFixture(p.buildConfig.DataDir)
			p.buildCgroups = m
			policy := ReferencePolicy()
			policy.PIDsMax = limit
			policy.MigrationTasksMax = limit + 1
			p.policy = &policy
			if err := p.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			result := p.Build(context.Background(), e, plan, path, io.Discard, io.Discard)
			if !errors.Is(result.Err, injected) || cli.builds != 1 {
				t.Fatal(result)
			}
			var parents []string
			for _, arg := range cli.args {
				if strings.HasPrefix(arg, "--cgroup-parent=") {
					parents = append(parents, arg)
				}
			}
			if !reflect.DeepEqual(parents, []string{"--cgroup-parent=" + buildCgroupParent(e)}) || !slices.Contains(cli.args, "--cgroup-manager=systemd") {
				t.Fatal(cli.args)
			}
			r, err := m.record(e)
			if err != nil || r.Limit != limit {
				t.Fatal(r, err)
			}
			if s := f.groups[buildSlice(e)]; s.Limit != limit {
				t.Fatal("effective parent not verified", s)
			}
			if f.stops() != 0 {
				t.Fatal("failed build released parent before backend cleanup")
			}
			if err := m.Check(context.Background(), e); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBuildPIDSetupFailureNeverSubmits(t *testing.T) {
	for _, mode := range []string{"manager-failure", "wrong-limit", "wrong-identity", "live-tasks", "stale-unit", "existing-record"} {
		t.Run(mode, func(t *testing.T) {
			e := Exact{identity(t, "s", "failure").Workspace(), Generation(strings.Repeat("a", 64))}
			plan, _ := compileDockerfile("FROM scratch", nil)
			p, stub := podmanStub(t, infoStep(), imageListStep(t))
			cli := &buildCLIStub{stubCLI: stub}
			p.runner = cli
			path := configureBuildFixture(t, p, e, plan)
			m, f := cgroupFixture(p.buildConfig.DataDir)
			p.buildCgroups = m
			switch mode {
			case "manager-failure":
				f.createErr = injected
			case "wrong-limit":
				f.wrongLimit = true
			case "wrong-identity":
				f.badIdentity = true
			case "live-tasks":
				f.liveTasks = true
			case "stale-unit":
				f.units[buildSlice(e)] = map[string]string{}
			case "existing-record":
				if err := os.WriteFile(m.recordPath(e), []byte("stale"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			if result := p.Build(context.Background(), e, plan, path, io.Discard, io.Discard); result.Err == nil || cli.builds != 0 {
				t.Fatal("unbounded submission", result)
			}
			if f.stops() != 0 {
				t.Fatal("setup failure guessed cleanup ownership")
			}
		})
	}
}

func TestBuildPIDExactReleaseAndAmbiguity(t *testing.T) {
	for _, mode := range []string{"success", "tasks", "kernel-limit", "description", "foreign-path", "persistent-unit", "stop-failure", "retained", "missing-record", "symlink-record", "orphan-cgroup"} {
		t.Run(mode, func(t *testing.T) {
			s, e := cleanupFixture(t)
			if err := os.Chmod(s.dataDir, 0700); err != nil {
				t.Fatal(err)
			}
			m, f := cgroupFixture(s.dataDir)
			if _, err := m.Prepare(context.Background(), e, 512); err != nil {
				t.Fatal(err)
			}
			other := Exact{e.Workspace, Generation(strings.Repeat("b", 64))}
			if err := os.Mkdir(s.buildPath(other), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Prepare(context.Background(), other, 64); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "tasks":
				f.liveTasks = true
			case "kernel-limit":
				v := f.groups[buildSlice(e)]
				v.Limit = 513
				f.groups[buildSlice(e)] = v
			case "description":
				f.units[buildSlice(e)]["Description"] = "stale"
			case "foreign-path":
				f.units[buildSlice(e)]["ControlGroup"] = buildGroupPath(buildSlice(other))
			case "persistent-unit":
				f.units[buildSlice(e)]["Transient"] = "no"
			case "stop-failure":
				f.stopErr = injected
			case "retained":
				f.retain = true
			case "missing-record":
				if err := os.Remove(m.recordPath(e)); err != nil {
					t.Fatal(err)
				}
			case "symlink-record":
				if err := os.Rename(m.recordPath(e), m.recordPath(e)+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(m.recordPath(e)+"-saved", m.recordPath(e)); err != nil {
					t.Fatal(err)
				}
			case "orphan-cgroup":
				delete(f.units, buildSlice(e))
			}
			err := m.Release(context.Background(), e)
			if mode == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("ambiguous/live cgroup released")
			}
			if _, ok := f.groups[buildSlice(other)]; !ok {
				t.Fatal("cross-generation cleanup")
			}
			if mode != "success" && mode != "stop-failure" && mode != "retained" && f.stops() != 0 {
				t.Fatal("ambiguous state authorized stop")
			}
		})
	}
}

func TestBuildPIDRecordsDoNotAuthorizePublicationOrReplay(t *testing.T) {
	s, e := cleanupFixture(t)
	if err := os.Chmod(s.dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	m, f := cgroupFixture(s.dataDir)
	if _, err := m.Prepare(context.Background(), e, 512); err != nil {
		t.Fatal(err)
	}
	// New adapter after restart discovers the exact ownership, never prepares it.
	reopened := &systemdBuildCgroups{dataDir: s.dataDir, bus: f, control: f, fs: f}
	got, err := reopened.Generations(context.Background())
	if err != nil || !reflect.DeepEqual(got, []Exact{e}) {
		t.Fatal(got, err)
	}
	if _, err := reopened.Prepare(context.Background(), e, 512); err == nil {
		t.Fatal("replayed stale slice")
	}
	f.units[buildSlice(e)]["Description"] = "another invocation"
	if _, err := reopened.Generations(context.Background()); err == nil {
		t.Fatal("ambiguous slice adopted")
	}
	if f.stops() != 0 {
		t.Fatal("discovery mutated user manager")
	}
}

func TestBuildPIDKernelObservationFailsClosed(t *testing.T) {
	for _, mode := range []string{"ok", "unbounded", "wrong-tasks", "populated", "frozen", "threaded", "symlink", "control-plane-inside", "malformed-membership", "missing-membership", "oversized", "missing"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			proc := filepath.Join(root, "proc")
			groups := filepath.Join(root, "cgroup")
			e := Exact{identity(t, "s", "kernel").Workspace(), Generation(strings.Repeat("a", 64))}
			path := filepath.Join(groups, strings.TrimPrefix(buildGroupPath(buildSlice(e)), "/"))
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(proc, "self"), 0700); err != nil {
				t.Fatal(err)
			}
			write := func(path, value string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(proc, "self", "cgroup"), "0::"+buildGroupPath("app.slice/mcp-workspaced.service")+"\n")
			for k, v := range map[string]string{"cgroup.type": "domain\n", "pids.max": "512\n", "pids.current": "0\n", "cgroup.events": "populated 0\nfrozen 0\n"} {
				write(filepath.Join(path, k), v)
			}
			switch mode {
			case "unbounded":
				write(filepath.Join(path, "pids.max"), "max")
			case "wrong-tasks":
				write(filepath.Join(path, "pids.current"), "invalid")
			case "populated":
				write(filepath.Join(path, "cgroup.events"), "populated 1\nfrozen 0\n")
			case "frozen":
				write(filepath.Join(path, "cgroup.events"), "populated 0\nfrozen 1\n")
			case "threaded":
				write(filepath.Join(path, "cgroup.type"), "threaded")
			case "symlink":
				if err := os.Remove(filepath.Join(path, "pids.max")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/dev/null", filepath.Join(path, "pids.max")); err != nil {
					t.Fatal(err)
				}
			case "control-plane-inside":
				write(filepath.Join(proc, "self", "cgroup"), "0::"+buildGroupPath(buildSlice(e))+"/daemon\n")
			case "malformed-membership":
				write(filepath.Join(proc, "self", "cgroup"), "0::/x\n0::/y\n")
			case "missing-membership":
				if err := os.Remove(filepath.Join(proc, "self", "cgroup")); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				write(filepath.Join(path, "cgroup.events"), strings.Repeat("0", 1025))
			case "missing":
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
			}
			fs := hostBuildGroupFS{openRoots: func() (*os.Root, *os.Root, error) {
				a, err := os.OpenRoot(proc)
				if err != nil {
					return nil, nil, err
				}
				b, err := os.OpenRoot(groups)
				if err != nil {
					a.Close()
					return nil, nil, err
				}
				return a, b, nil
			}}
			v, err := fs.Sample(context.Background(), buildSlice(e))
			switch mode {
			case "ok":
				if err != nil || !v.Exists || v.Limit != 512 || v.Tasks != 0 || v.Populated {
					t.Fatal(v, err)
				}
			case "populated":
				if err != nil || !v.Populated {
					t.Fatal(v, err)
				}
			case "missing":
				if err != nil || v.Exists {
					t.Fatal(v, err)
				}
			default:
				if err == nil {
					t.Fatal("unsafe kernel observation accepted", v)
				}
			}
		})
	}
}

func TestBuildPIDTerminalTasksFenceSuccess(t *testing.T) {
	e := Exact{identity(t, "s", "survivor").Workspace(), Generation(strings.Repeat("a", 64))}
	plan, _ := compileDockerfile("FROM scratch", nil)
	p, stub := podmanStub(t, infoStep(), imageListStep(t))
	path := configureBuildFixture(t, p, e, plan)
	m, f := cgroupFixture(p.buildConfig.DataDir)
	p.buildCgroups = m
	cli := &terminalTaskCLI{buildCLIStub: buildCLIStub{stubCLI: stub, result: BackendResult{Completion: DefinitelyCompleted}}, manager: f}
	p.runner = cli
	if err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	result := p.Build(context.Background(), e, plan, path, io.Discard, io.Discard)
	if !errors.Is(result.Err, ErrOutcomeUnknown) || cli.builds != 1 || f.stops() != 0 {
		t.Fatal("live RUN tasks allowed success", result)
	}
}

type terminalTaskCLI struct {
	buildCLIStub
	manager *fakeUserManager
}

func (f *terminalTaskCLI) runBuild(ctx context.Context, input, temp string, out, diagnostic io.Writer, args ...string) BackendResult {
	result := f.buildCLIStub.runBuild(ctx, input, temp, out, diagnostic, args...)
	f.manager.liveTasks = true
	return result
}

func TestBuildPIDLifecycleKeepsOwnershipUntilExactCleanup(t *testing.T) {
	for _, mode := range []string{"destroy", "ttl", "restart", "late", "failed", "setup-unknown"} {
		t.Run(mode, func(t *testing.T) {
			builder, engine, cfg, lock := storageLifecycleFixture(t)
			manager, host := cgroupFixture(cfg.DataDir)
			builder.podman.buildCgroups = manager
			engine.late = mode == "late" || mode == "failed"
			if mode == "setup-unknown" {
				host.createReplyLost = true
			}
			s, err := open(context.Background(), cfg, func() Runtime { return builder }, lock)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close() }()
			i := identity(t, "s", "cgroup-lifecycle")
			r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch\nRUN true")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "setup-unknown" {
				waitBuildState(t, s, i, Failed)
			} else {
				waitStorageSignal(t, engine.started)
			}
			host.mu.Lock()
			group, exists := host.groups[buildSlice(r.Exact)]
			host.mu.Unlock()
			if !exists || group.Limit != 512 {
				t.Fatal("build did not own reference parent", group)
			}
			switch mode {
			case "restart":
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = open(context.Background(), cfg, func() Runtime { return builder }, lock)
				if err != nil {
					t.Fatal(err)
				}
				assertState(t, s, i, Failed)
			case "ttl":
				s.mu.Lock()
				now := s.lifetimes[r.Exact].Created.Add(s.policy.AbsoluteTTL)
				s.now = func() time.Time { return now }
				s.mu.Unlock()
				if err := s.CleanupExpired(context.Background(), r.Exact); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal(err)
				}
			case "failed":
				// A terminal failed build retains the parent for backend cleanup.
				s.mu.Lock()
				s.builds[r.Exact].cancel()
				s.mu.Unlock()
				waitStorageSignal(t, engine.canceled)
				engine.finish <- BackendResult{DefinitelyCompleted, injected}
				waitBuildState(t, s, i, Failed)
			case "setup-unknown":
			default:
				if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal(err)
				}
			}
			if mode != "setup-unknown" {
				waitStorageSignal(t, engine.canceled)
			}
			if host.stops() != 0 {
				t.Fatal("cancellation/restart released cgroup before backend cleanup")
			}
			if mode != "late" {
				s.buildWG.Wait()
			}
			for _, op := range s.PendingBackend() {
				if err := s.RecoverBackend(op, DefinitelyCompleted); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "late" {
				if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal("active worker lost cgroup ownership", err)
				}
				if host.stops() != 0 {
					t.Fatal("recovery released live worker's parent")
				}
				engine.finish <- BackendResult{DefinitelyCompleted, injected}
				waitBuildState(t, s, i, Absent)
			} else {
				// Even trusted backend recovery cannot erase observed live tasks.
				host.mu.Lock()
				host.liveTasks = true
				host.mu.Unlock()
				if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal("live tasks allowed ABSENT", err)
				}
				assertState(t, s, i, Failed)
				if _, err := os.Stat(s.buildPath(r.Exact)); err != nil {
					t.Fatal("Data removed before cgroup absence")
				}
				if host.stops() != 0 {
					t.Fatal("populated parent stopped")
				}
				for _, op := range s.PendingBackend() {
					if err := s.RecoverBackend(op, DefinitelyCompleted); err != nil {
						t.Fatal(err)
					}
				}
				host.mu.Lock()
				host.liveTasks = false
				host.mu.Unlock()
				if err := s.Destroy(context.Background(), i); err != nil {
					t.Fatal(err)
				}
				assertState(t, s, i, Absent)
			}
			host.mu.Lock()
			remaining := len(host.groups) + len(host.units)
			host.mu.Unlock()
			if remaining != 0 || host.stops() != 1 {
				t.Fatal("ABSENT retained cgroup state", remaining)
			}
			if _, err := os.Lstat(s.buildPath(r.Exact)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("ABSENT retained cgroup/log metadata", err)
			}
			if builder.creates != 0 {
				t.Fatal("revoked/failed cgroup published")
			}
			if err := s.DestroyGeneration(context.Background(), r.Exact); !errors.Is(err, ErrStaleGeneration) {
				t.Fatal(err)
			}
			if host.stops() != 1 {
				t.Fatal("stale generation authorized cgroup cleanup")
			}
		})
	}
}

func TestBuildPIDOrphanSliceRejectsStartup(t *testing.T) {
	builder, _, cfg, lock := storageLifecycleFixture(t)
	if err := privateDir(cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	manager, host := cgroupFixture(cfg.DataDir)
	builder.podman.buildCgroups = manager
	e := Exact{identity(t, "s", "orphan").Workspace(), Generation(strings.Repeat("a", 64))}
	if err := os.MkdirAll(filepath.Dir(manager.recordPath(e)), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), e, 512); err != nil {
		t.Fatal(err)
	}
	if s, err := open(context.Background(), cfg, func() Runtime { return builder }, lock); !errors.Is(err, ErrStaleGeneration) {
		if s != nil {
			s.Close()
		}
		t.Fatal("orphan slice adopted", err)
	}
	if host.stops() != 0 {
		t.Fatal("orphan slice was deleted")
	}
}
