//go:build linux

package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type storageFunc func(context.Context, string) (StorageSample, error)

func (f storageFunc) Sample(ctx context.Context, path string) (StorageSample, error) {
	return f(ctx, path)
}

type processRuntime struct {
	*fakeRuntime
	snapshot ProcessSnapshot
	probeErr error
	probes   int
	hook     func()
}

func (f *processRuntime) ProbeProcess(_ context.Context, e Exact) (ProcessSnapshot, error) {
	f.probes++
	if f.hook != nil {
		f.hook()
	}
	return f.snapshot, CompletedBackendError(f.probeErr)
}
func safeSnapshot(e Exact, p Policy, tasks uint64) ProcessSnapshot {
	return ProcessSnapshot{Exact: e, ObjectID: string(e.Generation), PID: 123, PIDStart: 456, CgroupPath: "/service/exact", CgroupID: 789, Tasks: tasks,
		MemoryBytes: p.MemoryBytes, SwapBytes: p.SwapBytes, CPUQuota: p.CPUQuota, CPUWeight: p.CPUWeight, PIDsMax: p.PIDsMax, NoFileSoft: p.NoFile, NoFileHard: p.NoFile}
}
func openControlled(t *testing.T, runtime Runtime, cfg Config) (*Service, Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.Image, cfg.PodmanPath = testImage, "/stub/podman"
	if cfg.Storage == nil {
		cfg.Storage = storageFunc(func(context.Context, string) (StorageSample, error) { return StorageSample{1 << 40, 1 << 30}, nil })
	}
	lock := filepath.Join(dir, "lock", "daemon.lock")
	s, err := open(context.Background(), cfg, func() Runtime { return runtime }, lock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, cfg, lock
}

func TestReferencePolicyAndOperatorValidation(t *testing.T) {
	p := ReferencePolicy()
	if p.MemoryBytes != 4<<30 || p.MaxGenerations != 2 || p.SwapBytes != 0 || p.CPUQuota != -1 || p.CPUWeight != 100 || p.PIDsMax != 512 || p.MigrationTasksMax != 513 || p.NoFile != 1024 || p.MaxBackendOperations != 4 {
		t.Fatal(p)
	}
	for _, change := range []func(*Policy){func(p *Policy) { p.IdleTTL = 0 }, func(p *Policy) { p.AbsoluteTTL = -1 }, func(p *Policy) { p.MinFreeInodes = 0 }, func(p *Policy) { p.MinAvailableBytes = 0 }, func(p *Policy) { p.MigrationTasksMax = 514 }, func(p *Policy) { p.MaxBackendOperations = 0 }} {
		bad := p
		change(&bad)
		if bad.Validate() == nil {
			t.Fatal("invalid policy admitted", bad)
		}
	}
}

func TestGenerationAdmissionConcurrentAndUncertain(t *testing.T) {
	p := ReferencePolicy()
	p.MaxGenerations = 2
	f := newFake()
	f.createErr = ErrOutcomeUnknown
	s, _, _ := openControlled(t, f, Config{Policy: &p})
	start := make(chan struct{})
	var wg sync.WaitGroup
	var admitted atomic.Int32
	for n := range 20 {
		i := identity(t, "subject", fmt.Sprint(n))
		wg.Go(func() {
			<-start
			e, err := s.Create(context.Background(), i)
			if e.Generation != "" {
				admitted.Add(1)
				if !errors.Is(err, ErrOutcomeUnknown) {
					t.Error(err)
				}
			} else if !errors.Is(err, ErrGenerationLimit) {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	if admitted.Load() != 2 || len(s.entries) != 2 || f.creates != 2 || len(s.PendingBackend()) != 2 {
		t.Fatal("generation ceiling raced", admitted.Load(), f.creates)
	}
	for _, op := range s.PendingBackend() {
		if err := s.DestroyGeneration(context.Background(), op.Exact); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatal(err)
		}
		if err := s.RecoverBackend(op, DefinitelyCompleted); err != nil {
			t.Fatal(err)
		}
		if err := s.DestroyGeneration(context.Background(), op.Exact); err != nil {
			t.Fatal(err)
		}
	}
	f.createErr = nil
	if _, err := s.Create(context.Background(), identity(t, "new", "candidate")); err != nil {
		t.Fatal(err)
	}
}

func TestBackendLedgerUnknownCompletedDiagnosticsAndExactRecovery(t *testing.T) {
	b, err := NewBackendLedger(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	e := Exact{identity(t, "s", "c").Workspace(), Generation("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}
	p1, _ := b.Acquire(e, "build")
	p2, _ := b.Acquire(e, "exec")
	if err := p1.Resolve(e, OutcomeUnknown); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	if _, err := b.Acquire(e, "next"); !errors.Is(err, ErrBackendLimit) {
		t.Fatal(err)
	}
	other := e
	other.Generation = Generation("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err := p1.Resolve(other, DefinitelyCompleted); !errors.Is(err, ErrStaleGeneration) {
		t.Fatal(err)
	}
	if err := p1.Resolve(e, DefinitelyCompleted); err != nil {
		t.Fatal(err)
	}
	p3, err := b.Acquire(e, "next")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Diagnostics()) != 1 || len(b.Pending()) != 2 {
		t.Fatal("diagnostics consumed live permit")
	}
	_ = p2.Resolve(e, DefinitelyCompleted)
	_ = p3.Resolve(e, DefinitelyCompleted)
	if len(b.Pending()) != 0 || len(b.Diagnostics()) != 1 {
		t.Fatal("unbounded diagnostics or leaked permits")
	}
}

func TestBackendLedgerConcurrentCeiling(t *testing.T) {
	b, _ := NewBackendLedger(4, 0)
	e := Exact{identity(t, "s", "c").Workspace(), Generation("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var count atomic.Int32
	for range 32 {
		wg.Go(func() {
			<-start
			if _, err := b.Acquire(e, "work"); err == nil {
				count.Add(1)
			} else if !errors.Is(err, ErrBackendLimit) {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	if count.Load() != 4 {
		t.Fatal(count.Load())
	}
}

func TestInjectionFreshExactGateAndTaskEnvelope(t *testing.T) {
	p := ReferencePolicy()
	f := &processRuntime{fakeRuntime: newFake()}
	s, _, _ := openControlled(t, f, Config{Policy: &p})
	i := identity(t, "s", "c")
	e, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*ProcessSnapshot)
		want   error
	}{
		{"below-ceiling", func(v *ProcessSnapshot) { v.Tasks = 511 }, nil},
		{"full-ceiling", func(v *ProcessSnapshot) { v.Tasks = 512 }, ErrInjectionBusy},
		{"bounded-race", func(v *ProcessSnapshot) { v.Tasks = 513 }, ErrInjectionBusy},
		{"beyond-race", func(v *ProcessSnapshot) { v.Tasks = 514 }, ErrProcessUnsafe},
		{"unknown-pid", func(v *ProcessSnapshot) { v.PID = 0 }, ErrProcessUnsafe},
		{"reused-pid-unknown-start", func(v *ProcessSnapshot) { v.PIDStart = 0 }, ErrProcessUnsafe},
		{"unknown-cgroup", func(v *ProcessSnapshot) { v.CgroupID = 0 }, ErrProcessUnsafe},
		{"wrong-exact", func(v *ProcessSnapshot) {
			v.Exact.Generation = Generation("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		}, ErrProcessUnsafe},
		{"wrong-effective-limit", func(v *ProcessSnapshot) { v.SwapBytes = 1 }, ErrProcessUnsafe},
		{"wrong-nofile", func(v *ProcessSnapshot) { v.NoFileHard++ }, ErrProcessUnsafe},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.snapshot = safeSnapshot(e, p, 1)
			tc.change(&f.snapshot)
			called := false
			before := f.probes
			_, err := s.Inject(context.Background(), i, e, func(_ context.Context, v ProcessSnapshot) BackendResult {
				called = true
				if v != f.snapshot {
					t.Error("cached target")
				}
				return BackendResult{Completion: DefinitelyCompleted}
			})
			if tc.want == nil {
				if err != nil || !called {
					t.Fatal(err)
				}
			} else if !errors.Is(err, tc.want) || called {
				t.Fatal("unsafe injection", err)
			}
			if f.probes != before+1 || len(s.PendingBackend()) != 0 {
				t.Fatal("probe skipped or permit leaked")
			}
		})
	}
	// A fallen task count reopens injection without abandoning the exact binding.
	f.snapshot = safeSnapshot(e, p, 1)
	f.probeErr = injected
	called := false
	_, err = s.Inject(context.Background(), i, e, func(context.Context, ProcessSnapshot) BackendResult {
		called = true
		return BackendResult{Completion: DefinitelyCompleted}
	})
	if !errors.Is(err, ErrProcessUnsafe) || called {
		t.Fatal(err)
	}
	f.probeErr = nil
	if err := s.Destroy(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	e2, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	before := f.probes
	if _, err := s.Inject(context.Background(), i, e, func(context.Context, ProcessSnapshot) BackendResult {
		t.Error("stale callback")
		return BackendResult{}
	}); !errors.Is(err, ErrStaleGeneration) {
		t.Fatal(err)
	}
	if f.probes != before {
		t.Fatal("stale generation probed")
	}
	// A stale trusted probe result also cannot authorize the replacement.
	if _, err := s.Inject(context.Background(), i, e2, func(context.Context, ProcessSnapshot) BackendResult {
		t.Error("stale snapshot accepted")
		return BackendResult{}
	}); !errors.Is(err, ErrProcessUnsafe) {
		t.Fatal(err)
	}
}

func TestInjectionCancellationSinglePendingAndGlobalBudget(t *testing.T) {
	p := ReferencePolicy()
	p.MaxBackendOperations = 2
	f := &processRuntime{fakeRuntime: newFake()}
	s, _, _ := openControlled(t, f, Config{Policy: &p})
	i := identity(t, "s", "c")
	e, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	f.snapshot = safeSnapshot(e, p, 1)
	ctx, cancel := context.WithCancel(context.Background())
	op, err := s.Inject(ctx, i, e, func(context.Context, ProcessSnapshot) BackendResult {
		cancel()
		return BackendResult{Err: context.Canceled}
	})
	if !errors.Is(err, ErrOutcomeUnknown) || len(s.PendingBackend()) != 1 {
		t.Fatal(err)
	}
	before := f.probes
	if _, err := s.Inject(context.Background(), i, e, func(context.Context, ProcessSnapshot) BackendResult {
		t.Error("second injection")
		return BackendResult{}
	}); !errors.Is(err, ErrInjectionBusy) {
		t.Fatal(err)
	}
	if f.probes != before {
		t.Fatal("pending injection probed")
	}
	build, err := s.BeginBackend(e, "build")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginBackend(e, "extra"); !errors.Is(err, ErrBackendLimit) {
		t.Fatal(err)
	}
	wrong := op
	wrong.Exact.Generation = Generation("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err := s.RecoverBackend(wrong, DefinitelyCompleted); !errors.Is(err, ErrStaleGeneration) {
		t.Fatal(err)
	}
	if err := s.RecoverBackend(op, OutcomeUnknown); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	if err := s.RecoverBackend(op, DefinitelyCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Inject(context.Background(), i, e, func(context.Context, ProcessSnapshot) BackendResult {
		return BackendResult{Completion: DefinitelyCompleted, Err: injected}
	}); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if len(s.PendingBackend()) != 1 {
		t.Fatal("completed failure retained live permit")
	}
	if err := s.RecoverBackend(build, DefinitelyCompleted); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentInjectionSubmissionAndCleanup(t *testing.T) {
	f := &processRuntime{fakeRuntime: newFake()}
	s, _, _ := openControlled(t, f, Config{})
	i := identity(t, "s", "c")
	e, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	f.snapshot = safeSnapshot(e, ReferencePolicy(), 1)
	entered, release := make(chan struct{}), make(chan struct{})
	submitted := make(chan BackendOperation, 1)
	go func() {
		op, err := s.Inject(context.Background(), i, e, func(context.Context, ProcessSnapshot) BackendResult {
			close(entered)
			<-release
			return BackendResult{}
		})
		if !errors.Is(err, ErrOutcomeUnknown) {
			t.Error(err)
		}
		submitted <- op
	}()
	<-entered
	cleanupStarted, cleanupDone := make(chan struct{}), make(chan error, 1)
	go func() { close(cleanupStarted); cleanupDone <- s.DestroyGeneration(context.Background(), e) }()
	<-cleanupStarted
	select {
	case err := <-cleanupDone:
		t.Fatal("cleanup escaped submission lock", err)
	default:
	}
	close(release)
	op := <-submitted
	if err := <-cleanupDone; !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	if f.destroys != 0 {
		t.Fatal("removed generation with pending injection")
	}
	if err := s.RecoverBackend(op, DefinitelyCompleted); err != nil {
		t.Fatal(err)
	}
	if err := s.DestroyGeneration(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func TestTTLRevalidatesHintAndExactGeneration(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := ReferencePolicy()
	p.IdleTTL = time.Minute
	p.AbsoluteTTL = 10 * time.Minute
	f := newFake()
	s, _, _ := openControlled(t, f, Config{Policy: &p, Clock: func() time.Time { return now }})
	i := identity(t, "s", "c")
	e, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	hints := s.ExpiredGenerations()
	if len(hints) != 1 || hints[0] != e {
		t.Fatal(hints)
	}
	if err := s.AdmitActivity(i, e); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupExpired(context.Background(), hints[0]); !errors.Is(err, ErrNotExpired) {
		t.Fatal("stale idle hint cleaned active generation", err)
	}
	if f.destroys != 0 {
		t.Fatal("active workspace removed")
	}
	now = now.Add(time.Minute)
	if err := s.CleanupExpired(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	e2, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupExpired(context.Background(), e); !errors.Is(err, ErrStaleGeneration) {
		t.Fatal(err)
	}
	if len(s.entries) != 1 || s.entries[e2.Workspace] != e2 {
		t.Fatal("replacement removed")
	}
}

func TestBeginBackendTTLAdmission(t *testing.T) {
	for _, tc := range []struct {
		name       string
		elapsed    time.Duration
		activityAt time.Duration
		want       error
	}{
		{"before-idle-deadline", time.Minute - time.Nanosecond, 0, nil},
		{"at-idle-deadline", time.Minute, 0, ErrIdleTTL},
		{"after-idle-deadline", time.Minute + time.Nanosecond, 0, ErrIdleTTL},
		{"admitted-activity-refreshed-idle", time.Minute, time.Minute / 2, nil},
		{"at-refreshed-idle-deadline", 90 * time.Second, time.Minute / 2, ErrIdleTTL},
		{"absolute-deadline-with-recent-activity", 3 * time.Minute, 150 * time.Second, ErrAbsoluteTTL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			now := start
			p := ReferencePolicy()
			p.IdleTTL, p.AbsoluteTTL = time.Minute, 3*time.Minute
			s, _, _ := openControlled(t, newFake(), Config{Policy: &p, Clock: func() time.Time { return now }})
			i := identity(t, "s", "c")
			e, err := s.Create(context.Background(), i)
			if err != nil {
				t.Fatal(err)
			}
			if tc.activityAt != 0 {
				now = start.Add(tc.activityAt)
				if err := s.AdmitActivity(i, e); err != nil {
					t.Fatal(err)
				}
			}
			life, counter := s.lifetimes[e], s.backend.state().Next
			now = start.Add(tc.elapsed)
			op, err := s.BeginBackend(e, "build")
			if tc.want == nil {
				if err != nil || op.Exact != e || len(s.PendingBackend()) != 1 {
					t.Fatalf("unexpired backend work rejected: operation=%+v err=%v", op, err)
				}
			} else if !errors.Is(err, tc.want) || op != (BackendOperation{}) || len(s.PendingBackend()) != 0 || s.backend.state().Next != counter {
				t.Fatalf("expired backend admission changed accounting: operation=%+v err=%v pending=%v", op, err, s.PendingBackend())
			}
			if s.lifetimes[e] != life {
				t.Fatal("backend admission refreshed generation lifetime")
			}
		})
	}
}

func TestBeginBackendIdleExpiryRetainsPendingWork(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := ReferencePolicy()
	p.IdleTTL = time.Minute
	s, _, _ := openControlled(t, newFake(), Config{Policy: &p, Clock: func() time.Time { return now }})
	i := identity(t, "s", "c")
	e, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.BeginBackend(e, "detached")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(p.IdleTTL)
	if _, err := s.BeginBackend(e, "build"); !errors.Is(err, ErrIdleTTL) {
		t.Fatal("idle-expired generation admitted new work", err)
	}
	if pending := s.PendingBackend(); len(pending) != 1 || pending[0] != op {
		t.Fatal("idle expiry released pending work", pending)
	}
	if err := s.CleanupExpired(context.Background(), e); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal("cleanup ignored pending work", err)
	}
	if err := s.RecoverBackend(op, DefinitelyCompleted); err != nil {
		t.Fatal("expired backend work could not be recovered", err)
	}
	if err := s.CleanupExpired(context.Background(), e); err != nil {
		t.Fatal("backend recovery prevented TTL cleanup", err)
	}
}

func TestTTLAbsoluteDetachedAndRestart(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := ReferencePolicy()
	p.IdleTTL = 2 * time.Minute
	p.AbsoluteTTL = 3 * time.Minute
	f := newFake()
	s, cfg, lock := openControlled(t, f, Config{Policy: &p, Clock: func() time.Time { return now }})
	i := identity(t, "s", "c")
	e, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.BeginBackend(e, "detached")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := s.RecoverBackend(op, DefinitelyCompleted); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if len(s.ExpiredGenerations()) != 1 {
		t.Fatal("detached completion refreshed idle TTL")
	}
	if err := s.AdmitActivity(i, e); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	now = now.Add(time.Minute)
	if len(restarted.ExpiredGenerations()) != 1 {
		t.Fatal("absolute TTL refreshed on activity or restart")
	}
	if err := restarted.AdmitActivity(i, e); !errors.Is(err, ErrAbsoluteTTL) {
		t.Fatal(err)
	}
	if err := restarted.CleanupExpired(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func TestStorageByteAndInodeMarginsLatchWithoutDestroy(t *testing.T) {
	for _, margin := range []string{"bytes", "inodes", "zero", "probe-error"} {
		t.Run(margin, func(t *testing.T) {
			p := ReferencePolicy()
			p.MinAvailableBytes = 100
			p.MinFreeInodes = 10
			sample := StorageSample{100, 10}
			var probeErr error
			f := newFake()
			s, cfg, lock := openControlled(t, f, Config{Policy: &p, Storage: storageFunc(func(context.Context, string) (StorageSample, error) { return sample, probeErr })})
			i := identity(t, "active", "c")
			e, err := s.Create(context.Background(), i)
			if err != nil {
				t.Fatal(err)
			}
			switch margin {
			case "bytes":
				sample.AvailableBytes = 99
			case "inodes":
				sample.FreeInodes = 9
			case "zero":
				sample.AvailableBytes = 0
			case "probe-error":
				probeErr = injected
			}
			if _, err := s.Create(context.Background(), identity(t, "fresh", "c")); !errors.Is(err, ErrStorageRecovery) {
				t.Fatal(err)
			}
			if f.creates != 1 || f.destroys != 0 || s.entries[e.Workspace] != e {
				t.Fatal("pressure destroyed active workspace or created candidate")
			}
			if err := s.RecoverStorage(context.Background()); !errors.Is(err, ErrStorageRecovery) {
				t.Fatal(err)
			}
			sample = StorageSample{100, 10}
			probeErr = nil
			if _, err := s.Create(context.Background(), identity(t, "fresh", "c")); !errors.Is(err, ErrStorageRecovery) {
				t.Fatal("safe sample bypassed operator recovery", err)
			}
			s.Close()
			restarted, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if _, err := restarted.Create(context.Background(), identity(t, "fresh", "c")); !errors.Is(err, ErrStorageRecovery) {
				t.Fatal("restart bypassed recovery latch", err)
			}
			if err := restarted.RecoverStorage(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.Create(context.Background(), identity(t, "fresh", "c")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPressureCleanupOnlyIndependentTTLAndZeroIsOperatorOnly(t *testing.T) {
	for _, zero := range []bool{false, true} {
		t.Run(fmt.Sprint(zero), func(t *testing.T) {
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			p := ReferencePolicy()
			p.MinAvailableBytes = 100
			p.MinFreeInodes = 10
			p.IdleTTL = time.Minute
			sample := StorageSample{100, 10}
			f := newFake()
			s, _, _ := openControlled(t, f, Config{Policy: &p, Clock: func() time.Time { return now }, Storage: storageFunc(func(context.Context, string) (StorageSample, error) { return sample, nil })})
			old := identity(t, "old", "c")
			_, err := s.Create(context.Background(), old)
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute / 2)
			active := identity(t, "active", "c")
			live, err := s.Create(context.Background(), active)
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute / 2)
			sample.AvailableBytes = 99
			if zero {
				sample.AvailableBytes = 0
			}
			f.afterDestroy = func() { sample.AvailableBytes = 100 }
			_, err = s.Create(context.Background(), identity(t, "fresh", "c"))
			if zero {
				if !errors.Is(err, ErrStorageRecovery) || f.destroys != 0 {
					t.Fatal("zero-block recovery auto-deleted", err)
				}
				hints := s.ExpiredGenerations()
				if len(hints) != 1 {
					t.Fatal(hints)
				}
				if err := s.CleanupExpired(context.Background(), hints[0]); !errors.Is(err, ErrStorageRecovery) {
					t.Fatal(err)
				}
			} else if err != nil || f.destroys != 1 {
				t.Fatal("TTL-eligible pressure recovery failed", err)
			}
			if s.entries[live.Workspace] != live {
				t.Fatal("pressure removed active generation")
			}
		})
	}
}

func TestUnknownBackendAndInjectionSurviveRestart(t *testing.T) {
	p := ReferencePolicy()
	p.MaxBackendOperations = 1
	f := &processRuntime{fakeRuntime: newFake()}
	s, cfg, lock := openControlled(t, f, Config{Policy: &p})
	i := identity(t, "s", "c")
	e, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	f.snapshot = safeSnapshot(e, p, 1)
	op, err := s.Inject(context.Background(), i, e, func(context.Context, ProcessSnapshot) BackendResult { return BackendResult{} })
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	s.Close()
	restarted, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if len(restarted.PendingBackend()) != 1 {
		t.Fatal("unknown backend permit lost on restart")
	}
	if _, err := restarted.BeginBackend(e, "build"); !errors.Is(err, ErrBackendLimit) {
		t.Fatal(err)
	}
	if _, err := restarted.Inject(context.Background(), i, e, func(context.Context, ProcessSnapshot) BackendResult {
		t.Error("duplicate injection after restart")
		return BackendResult{}
	}); !errors.Is(err, ErrInjectionBusy) {
		t.Fatal(err)
	}
	if err := restarted.RecoverBackend(op, DefinitelyCompleted); err != nil {
		t.Fatal(err)
	}
	next, err := restarted.BeginBackend(e, "build")
	if err != nil {
		t.Fatal(err)
	}
	if next.ID <= op.ID {
		t.Fatal("backend operation identity reused")
	}
	if err := restarted.RecoverBackend(op, DefinitelyCompleted); !errors.Is(err, ErrStaleGeneration) {
		t.Fatal("old completion released newer operation", err)
	}
}

type resultRuntime struct {
	*fakeRuntime
	result error
}

func (f *resultRuntime) Create(ctx context.Context, e Exact, image string) error {
	_ = f.fakeRuntime.Create(ctx, e, image)
	return f.result
}

func TestRuntimeUnknownVersusAttestedTerminalFailure(t *testing.T) {
	for _, tc := range []struct {
		err     error
		pending int
	}{
		{injected, 1}, {context.Canceled, 1}, {context.DeadlineExceeded, 1}, {ErrOutcomeUnknown, 1}, {CompletedBackendError(injected), 0},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			f := &resultRuntime{newFake(), tc.err}
			s, _, _ := openControlled(t, f, Config{})
			i := identity(t, "s", "c")
			e, err := s.Create(context.Background(), i)
			if !errors.Is(err, tc.err) || e.Generation == "" {
				t.Fatal("runtime failure not retained", err)
			}
			if len(s.PendingBackend()) != tc.pending {
				t.Fatal("completion accounting", s.PendingBackend())
			}
			assertState(t, s, i, Present)
		})
	}
}

func TestCanceledRuntimeMutationRetainsPermitAcrossRestart(t *testing.T) {
	f := newFake()
	s, cfg, lock := openControlled(t, f, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	f.afterCreate = cancel
	f.createErr = context.Canceled
	i := identity(t, "s", "c")
	e, err := s.Create(ctx, i)
	if !errors.Is(err, context.Canceled) || len(s.PendingBackend()) != 1 {
		t.Fatal("caller cancellation released uncertain backend", err)
	}
	s.Close()
	f.afterCreate = nil
	f.createErr = nil
	restarted, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.DestroyGeneration(context.Background(), e); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal("uncertain generation released after restart", err)
	}
	ops := restarted.PendingBackend()
	if len(ops) != 1 || ops[0].Exact != e {
		t.Fatal(ops)
	}
	if err := restarted.RecoverBackend(ops[0], DefinitelyCompleted); err != nil {
		t.Fatal(err)
	}
	if err := restarted.DestroyGeneration(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func TestProbeUnknownAndSubmissionPanicKeepInjectionReservation(t *testing.T) {
	for _, mode := range []string{"unknown-probe", "panic-submit"} {
		t.Run(mode, func(t *testing.T) {
			f := &processRuntime{fakeRuntime: newFake()}
			s, _, _ := openControlled(t, f, Config{})
			i := identity(t, "s", "c")
			e, err := s.Create(context.Background(), i)
			if err != nil {
				t.Fatal(err)
			}
			f.snapshot = safeSnapshot(e, ReferencePolicy(), 1)
			if mode == "unknown-probe" {
				f.probeErr = ErrOutcomeUnknown
				_, err = s.Inject(context.Background(), i, e, func(context.Context, ProcessSnapshot) BackendResult {
					t.Error("injected after uncertain probe")
					return BackendResult{}
				})
				if !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal(err)
				}
			} else {
				func() {
					defer func() {
						if recover() == nil {
							t.Error("submission did not panic")
						}
					}()
					_, _ = s.Inject(context.Background(), i, e, func(context.Context, ProcessSnapshot) BackendResult { panic("submission interrupted") })
				}()
			}
			if len(s.PendingBackend()) != 1 {
				t.Fatal("uncertain backend permit lost")
			}
			if _, err := s.Inject(context.Background(), i, e, func(context.Context, ProcessSnapshot) BackendResult {
				t.Error("duplicate uncertain injection")
				return BackendResult{}
			}); !errors.Is(err, ErrInjectionBusy) {
				t.Fatal(err)
			}
			if err := s.RecoverBackend(s.PendingBackend()[0], DefinitelyCompleted); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMissingProbeAndExpiredDuringProbeFailClosed(t *testing.T) {
	s, _, _ := openControlled(t, newFake(), Config{})
	i := identity(t, "s", "c")
	e, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Inject(context.Background(), i, e, func(context.Context, ProcessSnapshot) BackendResult {
		t.Error("missing probe injected")
		return BackendResult{}
	}); !errors.Is(err, ErrProcessUnsafe) {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := ReferencePolicy()
	p.AbsoluteTTL = time.Minute
	f := &processRuntime{fakeRuntime: newFake()}
	controlled, _, _ := openControlled(t, f, Config{Policy: &p, Clock: func() time.Time { return now }})
	current, err := controlled.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	f.snapshot = safeSnapshot(current, p, 1)
	f.hook = func() { now = now.Add(time.Minute) }
	if _, err := controlled.Inject(context.Background(), i, current, func(context.Context, ProcessSnapshot) BackendResult {
		t.Error("injected after absolute expiry")
		return BackendResult{}
	}); !errors.Is(err, ErrAbsoluteTTL) {
		t.Fatal(err)
	}
	if len(controlled.PendingBackend()) != 0 {
		t.Fatal("rejected unsubmitted injection leaked permit")
	}
}

func TestPolicyCopiedAndOldChronologyFailsClosed(t *testing.T) {
	p := ReferencePolicy()
	p.MaxGenerations = 1
	f := newFake()
	s, _, _ := openControlled(t, f, Config{Policy: &p})
	p.MaxGenerations = 99
	if _, err := s.Create(context.Background(), identity(t, "first", "c")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), identity(t, "second", "c")); !errors.Is(err, ErrGenerationLimit) {
		t.Fatal("mutable operator pointer altered admitted policy", err)
	}
	dir := t.TempDir()
	cfg := testConfig(filepath.Join(dir, "state"))
	if err := privateDir(cfg.StateDir); err != nil {
		t.Fatal(err)
	}
	old := Exact{identity(t, "legacy", "c").Workspace(), Generation("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}
	if err := writeJournal(filepath.Join(cfg.StateDir, "journal.json"), testImage, map[WorkspaceKey]Exact{old.Workspace: old}); err != nil {
		t.Fatal(err)
	}
	legacy := newFake()
	legacy.objects[old] = fakeObject(old)
	if _, err := open(context.Background(), cfg, func() Runtime { return legacy }, filepath.Join(dir, "lock", "daemon.lock")); err == nil {
		t.Fatal("missing old chronology silently renewed")
	}
	if legacy.initializes != 0 || legacy.destroys != 0 {
		t.Fatal("missing chronology caused runtime access")
	}
}

func TestUncertainLifecycleCannotAuthorizeBackendOrInjection(t *testing.T) {
	f := &processRuntime{fakeRuntime: newFake()}
	f.createErr = ErrOutcomeUnknown
	s, _, _ := openControlled(t, f, Config{})
	i := identity(t, "s", "c")
	e, err := s.Create(context.Background(), i)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	f.snapshot = safeSnapshot(e, ReferencePolicy(), 1)
	if _, err := s.Inject(context.Background(), i, e, func(context.Context, ProcessSnapshot) BackendResult {
		t.Error("uncertain lifecycle injected")
		return BackendResult{}
	}); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	if _, err := s.BeginBackend(e, "build"); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	if f.probes != 0 {
		t.Fatal("uncertain lifecycle probed for injection")
	}
	if err := s.RecoverBackend(s.PendingBackend()[0], DefinitelyCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Inject(context.Background(), i, e, func(context.Context, ProcessSnapshot) BackendResult {
		return BackendResult{Completion: DefinitelyCompleted}
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStorageLatchClosesCallerMutationButKeepsStatus(t *testing.T) {
	p := ReferencePolicy()
	p.MinAvailableBytes = 100
	p.MinFreeInodes = 10
	sample := StorageSample{100, 10}
	f := newFake()
	s, _, _ := openControlled(t, f, Config{Policy: &p, Storage: storageFunc(func(context.Context, string) (StorageSample, error) { return sample, nil })})
	i := identity(t, "active", "c")
	e, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	sample.AvailableBytes = 0
	if _, err := s.Create(context.Background(), identity(t, "fresh", "c")); !errors.Is(err, ErrStorageRecovery) {
		t.Fatal(err)
	}
	assertState(t, s, i, Present)
	if _, err := s.Create(context.Background(), i); !errors.Is(err, ErrStorageRecovery) {
		t.Fatal(err)
	}
	if err := s.AdmitActivity(i, e); !errors.Is(err, ErrStorageRecovery) {
		t.Fatal(err)
	}
	if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrStorageRecovery) {
		t.Fatal(err)
	}
	if _, err := s.BeginBackend(e, "build"); !errors.Is(err, ErrStorageRecovery) {
		t.Fatal(err)
	}
	if f.destroys != 0 {
		t.Fatal("caller mutation bypassed operator recovery")
	}
}

func TestAdmissionJournalRejectsCorruptAuthorityBeforeRuntime(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := Exact{identity(t, "s", "c").Workspace(), Generation("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}
	other := e
	other.Generation = Generation("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	for _, tc := range []struct {
		name   string
		change func(*journal)
	}{
		{"missing-lifetime", func(j *journal) { j.Lifetimes = nil }},
		{"invalid-lifetime-exact", func(j *journal) { j.Lifetimes[0].Exact = Exact{} }},
		{"duplicate-lifetime", func(j *journal) { j.Lifetimes = append(j.Lifetimes, j.Lifetimes[0]) }},
		{"future-activity", func(j *journal) { j.Lifetimes[0].Activity = now.Add(time.Minute) }},
		{"missing-counter", func(j *journal) { j.Backend.Next = 0 }},
		{"stale-backend", func(j *journal) { j.Backend.Operations = []BackendOperation{{1, other, "create"}} }},
		{"duplicate-operation", func(j *journal) { j.Backend.Operations = []BackendOperation{{1, e, "build"}, {1, e, "build"}} }},
		{"duplicate-injection", func(j *journal) {
			j.Backend.Next = 2
			j.Backend.Operations = []BackendOperation{{1, e, "injection"}, {2, e, "injection"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig(filepath.Join(dir, "state"))
			cfg.Clock = func() time.Time { return now }
			if err := privateDir(cfg.StateDir); err != nil {
				t.Fatal(err)
			}
			state := journal{Version: 2, Image: testImage, Entries: []Exact{e}, Lifetimes: []Lifetime{{e, now, now}}, Backend: BackendState{Next: 1}}
			tc.change(&state)
			if err := os.WriteFile(filepath.Join(cfg.StateDir, "journal.json"), []byte(jsonText(t, state)), 0600); err != nil {
				t.Fatal(err)
			}
			f := newFake()
			if _, err := open(context.Background(), cfg, func() Runtime { return f }, filepath.Join(dir, "lock", "daemon.lock")); err == nil {
				t.Fatal("invalid durable admission accepted")
			}
			if f.initializes != 0 || f.lists != 0 || f.destroys != 0 {
				t.Fatal("invalid authority reached runtime")
			}
		})
	}
}
