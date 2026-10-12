//go:build linux

package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var injected = errors.New("injected runtime fault")
var testImage = "example.invalid/workspace@sha256:" + strings.Repeat("a", 64)

type fakeRuntime struct {
	objects                                 map[Exact]Object
	extra                                   []Object
	creates, destroys, lists, initializes   int
	initErr, listErr, createErr, destroyErr error
	omitCreate, retainDestroy               bool
	listHook                                func()
	afterCreate, afterDestroy               func()
}

func fakeObject(e Exact) Object {
	return Object{ID: string(e.Generation), Name: objectName(e), Labels: objectLabels(e, testImage), Image: testImage}
}
func (f *fakeRuntime) Initialize(context.Context) error { f.initializes++; return f.initErr }
func (f *fakeRuntime) List(context.Context) ([]Object, error) {
	f.lists++
	if f.listHook != nil {
		f.listHook()
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	result := append([]Object(nil), f.extra...)
	for _, o := range f.objects {
		result = append(result, o)
	}
	return result, nil
}
func (f *fakeRuntime) Create(ctx context.Context, e Exact, _ string) error {
	f.creates++
	if !f.omitCreate {
		f.objects[e] = fakeObject(e)
	}
	if f.afterCreate != nil {
		f.afterCreate()
	}
	return localRuntimeError(ctx, f.createErr)
}
func (f *fakeRuntime) Destroy(ctx context.Context, e Exact) error {
	f.destroys++
	if !f.retainDestroy {
		delete(f.objects, e)
	}
	if f.afterDestroy != nil {
		f.afterDestroy()
	}
	return localRuntimeError(ctx, f.destroyErr)
}

func testConfig(dir string) Config {
	return Config{StateDir: dir, Image: testImage, PodmanPath: "/stub/podman", Storage: storageFunc(func(context.Context, string) (StorageSample, error) { return StorageSample{1 << 40, 1 << 30}, nil })}
}
func newFake() *fakeRuntime { return &fakeRuntime{objects: make(map[Exact]Object)} }
func openFake(t *testing.T, f *fakeRuntime) (*Service, Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg, lock := testConfig(filepath.Join(dir, "state")), filepath.Join(dir, "lock", "daemon.lock")
	s, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, cfg, lock
}
func assertState(t *testing.T, s *Service, i Identity, want State) {
	t.Helper()
	got, err := s.Status(i)
	if err != nil || got != want {
		t.Fatalf("state=%s error=%v; want %s", got, err, want)
	}
}

func TestLifecycleIdempotencyAndFencing(t *testing.T) {
	f := newFake()
	s, _, _ := openFake(t, f)
	i := identity(t, "s", "c")
	assertState(t, s, i, Absent)
	g1, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	assertState(t, s, i, Present)
	// Even lost/stopped runtime must not trigger repair/restart/replacement.
	delete(f.objects, g1)
	beforeLists := f.lists
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			got, err := s.Create(context.Background(), i)
			if err != nil || got != g1 {
				t.Errorf("idempotent create: %v %v", got, err)
			}
		})
	}
	wg.Wait()
	if f.creates != 1 || f.lists != beforeLists {
		t.Fatal("PRESENT create touched runtime")
	}
	if err := s.Destroy(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	assertState(t, s, i, Absent)
	if err := s.Destroy(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	g2, err := s.Create(context.Background(), i)
	if err != nil || g1.Workspace != g2.Workspace || g1.Generation == g2.Generation {
		t.Fatal("generation not refreshed", err)
	}
	beforeDestroy := f.destroys
	if err := s.DestroyGeneration(context.Background(), g1); !errors.Is(err, ErrStaleGeneration) {
		t.Fatal("stale cleanup accepted", err)
	}
	published := false
	if err := s.WithGeneration(g1, func() error { published = true; return nil }); !errors.Is(err, ErrStaleGeneration) {
		t.Fatal("stale publication accepted", err)
	}
	if published || f.destroys != beforeDestroy {
		t.Fatal("stale callback mutated runtime/publication")
	}
	if err := s.WithGeneration(g2, func() error { published = true; return nil }); err != nil || !published {
		t.Fatal("current publication rejected", err)
	}
	assertState(t, s, i, Present)
}

func TestPartialDestroyRetainsPresentAcrossRestart(t *testing.T) {
	for _, mode := range []string{"error-before", "error-after", "false-success", "unknown-discovery"} {
		t.Run(mode, func(t *testing.T) {
			f := newFake()
			s, cfg, lock := openFake(t, f)
			i := identity(t, "s", "c")
			e, err := s.Create(context.Background(), i)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "error-before":
				f.retainDestroy, f.destroyErr = true, injected
			case "error-after":
				f.destroyErr = injected
			case "false-success":
				f.retainDestroy = true
			case "unknown-discovery":
				f.afterDestroy = func() { f.listErr = injected }
			}
			if err := s.Destroy(context.Background(), i); err == nil {
				t.Fatal("partial destroy succeeded")
			}
			assertState(t, s, i, Present)
			if got, err := s.Create(context.Background(), i); err != nil || got != e || f.creates != 1 {
				t.Fatal("partial destroy repaired", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			f.listErr, f.destroyErr, f.retainDestroy = nil, nil, false
			f.afterDestroy = nil
			restarted, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			assertState(t, restarted, i, Present)
			if err := restarted.Destroy(context.Background(), i); err != nil {
				t.Fatal(err)
			}
			assertState(t, restarted, i, Absent)
		})
	}
}

func TestFailedCreateIntentIsDurable(t *testing.T) {
	for _, mode := range []string{"before", "after", "unconfirmed", "unknown-discovery"} {
		t.Run(mode, func(t *testing.T) {
			f := newFake()
			s, cfg, lock := openFake(t, f)
			i := identity(t, "raw-subject", "raw-session")
			switch mode {
			case "before":
				f.omitCreate, f.createErr = true, injected
			case "after":
				f.createErr = injected
			case "unconfirmed":
				f.omitCreate = true
			case "unknown-discovery":
				f.afterCreate = func() { f.listErr = injected }
			}
			e, err := s.Create(context.Background(), i)
			if err == nil {
				t.Fatal("failed create accepted")
			}
			assertState(t, s, i, Present)
			data, err := os.ReadFile(s.path)
			if err != nil || strings.Contains(string(data), "raw-") {
				t.Fatal("unsafe journal", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			f.listErr = nil
			restarted, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			got, err := restarted.Create(context.Background(), i)
			if err != nil || got != e || f.creates != 1 {
				t.Fatal("restart repaired failed candidate", err)
			}
			if err := restarted.Destroy(context.Background(), i); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPersistenceFaultClosesAdmission(t *testing.T) {
	for _, operation := range []string{"create", "destroy"} {
		t.Run(operation, func(t *testing.T) {
			f := newFake()
			s, cfg, lock := openFake(t, f)
			i := identity(t, "s", "c")
			if operation == "destroy" {
				if _, err := s.Create(context.Background(), i); err != nil {
					t.Fatal(err)
				}
			}
			s.save = func(string, string, map[WorkspaceKey]Exact, map[Exact]Lifetime, bool, BackendState) error {
				return injected
			}
			if operation == "create" {
				if _, err := s.Create(context.Background(), i); !errors.Is(err, injected) {
					t.Fatal(err)
				}
				if f.creates != 0 {
					t.Fatal("runtime mutated before durable intent")
				}
			} else if err := s.Destroy(context.Background(), i); !errors.Is(err, injected) {
				t.Fatal(err)
			}
			if _, err := s.Status(i); !errors.Is(err, ErrNotReady) {
				t.Fatal("admission remained open", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			want := Absent
			if operation == "destroy" {
				want = Present
			}
			assertState(t, restarted, i, want)
		})
	}
}

func TestIdentityAndReadinessFailClosed(t *testing.T) {
	s, _, _ := openFake(t, newFake())
	if _, err := s.Create(context.Background(), Identity{}); err == nil {
		t.Fatal("empty identity accepted")
	}
	if err := s.Destroy(context.Background(), Identity{}); err == nil {
		t.Fatal("empty identity accepted")
	}
	if _, err := s.Status(Identity{}); err == nil {
		t.Fatal("empty identity accepted")
	}
	unready := &Service{}
	i := identity(t, "s", "c")
	if _, err := unready.Create(context.Background(), i); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if err := unready.Destroy(context.Background(), i); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if _, err := unready.Status(i); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), i); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
}

func TestReconciliationAndSingletonGate(t *testing.T) {
	f := newFake()
	entered, release := make(chan struct{}), make(chan struct{})
	f.listHook = func() { close(entered); <-release }
	dir := t.TempDir()
	cfg, lock := testConfig(filepath.Join(dir, "state")), filepath.Join(dir, "lock", "daemon.lock")
	type result struct {
		s   *Service
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
		done <- result{s, err}
	}()
	select {
	case <-entered:
	case r := <-done:
		t.Fatalf("startup returned before discovery: %v", r.err)
	}
	select {
	case <-done:
		t.Fatal("admitted before reconciliation")
	default:
	}
	called := false
	// A different journal directory still conflicts on the account-wide lock.
	other := cfg
	other.StateDir = filepath.Join(dir, "other")
	if _, err := open(context.Background(), other, func() Runtime { called = true; return newFake() }, lock); err == nil || called {
		t.Fatal("singleton did not precede runtime construction")
	}
	close(release)
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if err := r.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.listHook = nil
	reopened, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal("lock not released", err)
	}
	reopened.Close()
}

func TestReconciliationRejectsUnknownAndLookalikeObjects(t *testing.T) {
	i := identity(t, "s", "c")
	g, err := freshGeneration()
	if err != nil {
		t.Fatal(err)
	}
	e := Exact{i.Workspace(), g}
	for _, mode := range []string{"unrelated", "lookalike-name", "lookalike-label", "unrecorded-owned", "wrong-generation", "wrong-image", "duplicate", "discovery-failure", "init-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newFake()
			dir := t.TempDir()
			cfg, lock := testConfig(filepath.Join(dir, "state")), filepath.Join(dir, "lock", "daemon.lock")
			if err := privateDir(cfg.StateDir); err != nil {
				t.Fatal(err)
			}
			entries := map[WorkspaceKey]Exact{}
			switch mode {
			case "unrelated":
				f.extra = []Object{{ID: "unrelated", Name: "someone-else"}}
			case "lookalike-name":
				f.extra = []Object{{Name: "mcpws-v1-lookalike"}}
			case "lookalike-label":
				f.extra = []Object{{Name: "other", Labels: map[string]string{ownerLabel: ownerValue}}}
			case "unrecorded-owned":
				f.objects[e] = fakeObject(e)
			case "wrong-generation":
				entries[e.Workspace] = Exact{e.Workspace, Generation(strings.Repeat("b", 64))}
				f.objects[e] = fakeObject(e)
			case "wrong-image":
				entries[e.Workspace] = e
				o := fakeObject(e)
				o.Image = "different@sha256:" + strings.Repeat("b", 64)
				o.Labels[imageLabel] = o.Image
				f.objects[e] = o
			case "duplicate":
				entries[e.Workspace] = e
				f.objects[e] = fakeObject(e)
				f.extra = []Object{fakeObject(e)}
			case "discovery-failure":
				f.listErr = injected
			case "init-failure":
				f.initErr = injected
			}
			lifetimes := make(map[Exact]Lifetime)
			now := time.Now()
			for _, entry := range entries {
				lifetimes[entry] = Lifetime{entry, now, now}
			}
			if err := writeAdmissionJournal(filepath.Join(cfg.StateDir, "journal.json"), cfg.Image, entries, lifetimes, false, BackendState{Next: 1}); err != nil {
				t.Fatal(err)
			}
			s, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
			if mode == "unrelated" {
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				assertState(t, s, i, Absent)
			} else if err == nil || s != nil {
				t.Fatal("reconciliation admitted invalid state")
			}
			if f.creates != 0 || f.destroys != 0 {
				t.Fatal("startup mutated runtime")
			}
			if mode == "init-failure" && f.lists != 0 {
				t.Fatal("discovery before runtime validation")
			}
		})
	}
}

func TestSingletonAcrossProcess(t *testing.T) {
	if path := os.Getenv("MCP_TEST_LOCK"); path != "" {
		lock, err := acquireLock(path)
		if err == nil {
			lock.Close()
			t.Fatal("child acquired parent's singleton")
		}
		return
	}
	path := filepath.Join(t.TempDir(), "daemon.lock")
	lock, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSingletonAcrossProcess$")
	cmd.Env = append(os.Environ(), "MCP_TEST_LOCK="+path)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, data)
	}
}

func TestDefaultSingletonDirectoryIgnoresHomeOverride(t *testing.T) {
	want, err := DefaultStateDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	got, err := DefaultStateDir()
	if err != nil || got != want {
		t.Fatal("HOME override changed account-wide singleton location", err)
	}
}

func TestDiscoveryFaultPreventsMutation(t *testing.T) {
	f := newFake()
	s, _, _ := openFake(t, f)
	i := identity(t, "s", "c")
	f.listErr = injected
	if _, err := s.Create(context.Background(), i); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if f.creates != 0 {
		t.Fatal("created before validating discovery")
	}
	assertState(t, s, i, Absent)
	f.listErr = nil
	e, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	o := f.objects[e]
	o.Labels[ownerLabel] = "lookalike"
	f.objects[e] = o
	if err := s.Destroy(context.Background(), i); err == nil {
		t.Fatal("destroyed an unrecognized object")
	}
	if f.destroys != 0 {
		t.Fatal("mutation preceded ownership validation")
	}
	assertState(t, s, i, Present)
}
