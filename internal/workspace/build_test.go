//go:build linux

package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBuilder struct {
	mu                        sync.Mutex
	objects                   map[Exact]Object
	images                    map[Exact]bool
	builds, creates, cleanups int
	started                   chan Exact
	finish                    chan BackendResult
	createStarted             chan Exact
	createFinish              chan error
	known                     []string
	cleanupErr                error
	retainImage               bool
	cancelObserved            chan struct{}
}

func newBuilder() *fakeBuilder {
	return &fakeBuilder{objects: map[Exact]Object{}, images: map[Exact]bool{}, started: make(chan Exact, 4), finish: make(chan BackendResult, 4)}
}
func (f *fakeBuilder) Initialize(context.Context) error { return nil }
func (f *fakeBuilder) List(context.Context) ([]Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := []Object{}
	for _, o := range f.objects {
		v = append(v, o)
	}
	return v, nil
}
func (f *fakeBuilder) Create(ctx context.Context, e Exact, image string) error {
	f.mu.Lock()
	f.creates++
	start, finish := f.createStarted, f.createFinish
	f.mu.Unlock()
	var err error
	if start != nil {
		start <- e
		err = <-finish
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Deliberately permit a side effect after cancellation to exercise fencing.
	if err == nil {
		f.objects[e] = Object{ID: string(e.Generation), Name: objectName(e), Labels: objectLabels(e, image), Image: image}
	}
	return CompletedBackendError(err)
}
func (f *fakeBuilder) Destroy(_ context.Context, e Exact) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, e)
	return nil
}
func (f *fakeBuilder) KnownBuildImages(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.known...), nil
}
func (f *fakeBuilder) BuildGenerations(context.Context) ([]Exact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := []Exact{}
	for e := range f.images {
		v = append(v, e)
	}
	return v, nil
}
func (f *fakeBuilder) Build(ctx context.Context, e Exact, plan BuildPlan, path string, out, diagnostic io.Writer) BackendResult {
	f.mu.Lock()
	f.builds++
	f.mu.Unlock()
	f.started <- e
	_, _ = out.Write([]byte(strings.Repeat("x", 3*MaxStreamBytes)))
	_, _ = diagnostic.Write([]byte("private build diagnostics"))
	var result BackendResult
	select {
	case result = <-f.finish:
	case <-ctx.Done():
		if f.cancelObserved != nil {
			close(f.cancelObserved)
			result = <-f.finish
		} else {
			return BackendResult{OutcomeUnknown, ErrOutcomeUnknown}
		}
	}
	f.mu.Lock()
	f.images[e] = true
	f.mu.Unlock()
	return result
}
func (f *fakeBuilder) CleanupBuild(_ context.Context, e Exact, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanups++
	if f.cleanupErr != nil {
		return CompletedBackendError(f.cleanupErr)
	}
	if f.retainImage {
		return ErrOutcomeUnknown
	}
	delete(f.images, e)
	return nil
}
func openBuilder(t *testing.T, f Runtime, p *Policy) (*Service, Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := testConfig(filepath.Join(dir, "state"))
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Policy = p
	lock := filepath.Join(dir, "lock", "daemon.lock")
	s, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, cfg, lock
}
func waitBuildState(t *testing.T, s *Service, i Identity, want State) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := s.Status(i)
		if err != nil {
			t.Fatal(err)
		}
		if state == want {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("state=%s want %s", state, want)
		}
	}
}
func receiveExact(t *testing.T, ch <-chan Exact) Exact {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
		return Exact{}
	}
}
func TestAsyncBuildPollingPublicationAndContext(t *testing.T) {
	f := newBuilder()
	s, _, _ := openBuilder(t, f, nil)
	i := identity(t, "s", "build")
	ctx, cancel := context.WithCancel(context.Background())
	r, err := s.CreateDockerfile(ctx, i, "FROM scratch\nCOPY . /src\n")
	if err != nil || r.Status != Processing {
		t.Fatalf("create=%v %v", r, err)
	}
	cancel()
	if receiveExact(t, f.started) != r.Exact {
		t.Fatal("wrong worker generation")
	}
	// Only Dockerfile is caller material; logs and tmp cannot enter COPY context.
	entries, err := os.ReadDir(filepath.Join(s.buildPath(r.Exact), "context"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "Dockerfile" {
		t.Fatalf("context=%v %v", entries, err)
	}
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.CreateDockerfile(context.Background(), i, "FROM unsafe")
			if err != nil || got != r {
				t.Errorf("duplicate=%v %v", got, err)
			}
		}()
	}
	wg.Wait()
	if _, err := s.Create(context.Background(), i); !errors.Is(err, ErrNotReady) {
		t.Fatalf("canonical create escaped candidate: %v", err)
	}
	for _, call := range []func() error{
		func() error {
			_, err := s.Exec(context.Background(), i, r.Exact, ExecRequest{Argv: []string{"true"}})
			return err
		},
		func() error {
			_, err := s.ReadText(context.Background(), i, r.Exact, ReadTextRequest{Path: "/x"})
			return err
		},
		func() error {
			_, err := s.WriteText(context.Background(), i, r.Exact, WriteTextRequest{Path: "/x", Content: "x"})
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrNotReady) {
			t.Fatalf("not ready=%v", err)
		}
	}
	f.finish <- BackendResult{Completion: DefinitelyCompleted}
	waitBuildState(t, s, i, Present)
	got, err := s.CreateDockerfile(context.Background(), i, "invalid")
	if err != nil || got.Status != Present || got.Exact != r.Exact {
		t.Fatalf("present=%v %v", got, err)
	}
	f.mu.Lock()
	if f.builds != 1 || f.creates != 1 {
		t.Fatalf("builds=%d creates=%d", f.builds, f.creates)
	}
	f.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(s.buildPath(r.Exact), "stdout"))
	if err != nil || len(data) != MaxStreamBytes {
		t.Fatalf("bounded log size=%d %v", len(data), err)
	}
	if len(s.PendingBackend()) != 0 {
		t.Fatal("successful build retained backend capacity")
	}
	if err := s.Destroy(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	assertState(t, s, i, Absent)
	if _, err := os.Lstat(s.buildPath(r.Exact)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("build state survived ABSENT")
	}
}
func TestBuildFailureUnknownRecoveryAndNoReplay(t *testing.T) {
	for _, result := range []BackendResult{{DefinitelyCompleted, injected}, {OutcomeUnknown, injected}} {
		t.Run(result.Err.Error()+string(rune(result.Completion)), func(t *testing.T) {
			f := newBuilder()
			s, _, _ := openBuilder(t, f, nil)
			i := identity(t, "s", "failure")
			r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
			if err != nil {
				t.Fatal(err)
			}
			receiveExact(t, f.started)
			f.finish <- result
			waitBuildState(t, s, i, Failed)
			got, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
			if err != nil || got.Status != Failed || got.Exact != r.Exact {
				t.Fatalf("failed polling=%v %v", got, err)
			}
			f.mu.Lock()
			if f.creates != 0 || f.builds != 1 {
				t.Fatal("failure published or replayed")
			}
			f.mu.Unlock()
			if result.Completion == OutcomeUnknown {
				if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal(err)
				}
				pending := s.PendingBackend()
				if len(pending) != 1 {
					t.Fatal(pending)
				}
				if err := s.RecoverBackend(pending[0], DefinitelyCompleted); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Destroy(context.Background(), i); err != nil {
				t.Fatal(err)
			}
			assertState(t, s, i, Absent)
		})
	}
}
func TestBuildCancellationLateSuccessAndNewGeneration(t *testing.T) {
	f := newBuilder()
	f.cancelObserved = make(chan struct{})
	s, _, _ := openBuilder(t, f, nil)
	i := identity(t, "s", "cancel")
	r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
	if err != nil {
		t.Fatal(err)
	}
	receiveExact(t, f.started)
	if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	<-f.cancelObserved
	if err := s.WithGeneration(r.Exact, func() error { t.Error("revoked callback ran"); return nil }); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	f.finish <- BackendResult{Completion: DefinitelyCompleted}
	waitBuildState(t, s, i, Absent)
	f.mu.Lock()
	if f.creates != 0 {
		t.Fatal("late success published")
	}
	f.cancelObserved = nil
	f.mu.Unlock()
	next, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
	if err != nil || next.Exact == r.Exact {
		t.Fatalf("replacement=%v %v", next, err)
	}
	receiveExact(t, f.started)
	if err := s.DestroyGeneration(context.Background(), r.Exact); !errors.Is(err, ErrStaleGeneration) {
		t.Fatal(err)
	}
	f.finish <- BackendResult{Completion: DefinitelyCompleted}
	waitBuildState(t, s, i, Present)
}
func TestFinalizationPollingCancellationAndLateContainer(t *testing.T) {
	f := newBuilder()
	f.createStarted = make(chan Exact, 1)
	f.createFinish = make(chan error, 1)
	s, _, _ := openBuilder(t, f, nil)
	i := identity(t, "s", "final")
	r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
	if err != nil {
		t.Fatal(err)
	}
	receiveExact(t, f.started)
	f.finish <- BackendResult{Completion: DefinitelyCompleted}
	receiveExact(t, f.createStarted)
	got, err := s.CreateDockerfile(context.Background(), i, "invalid")
	if err != nil || got != r {
		t.Fatalf("finalizing polling blocked/replaced: %v %v", got, err)
	}
	if _, err := s.BeginBackend(r.Exact, "exec"); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	f.createFinish <- nil
	waitBuildState(t, s, i, Absent)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.objects) != 0 || len(f.images) != 0 {
		t.Fatal("late container/image survived cleanup")
	}
}
func TestBuildRestartFencesUnfinishedCandidate(t *testing.T) {
	f := newBuilder()
	s, cfg, lock := openBuilder(t, f, nil)
	i := identity(t, "s", "restart")
	r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
	if err != nil {
		t.Fatal(err)
	}
	receiveExact(t, f.started)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	assertState(t, restarted, i, Failed)
	got, err := restarted.CreateDockerfile(context.Background(), i, "FROM scratch")
	if err != nil || got.Exact != r.Exact || got.Status != Failed {
		t.Fatalf("restart replay=%v %v", got, err)
	}
	pending := restarted.PendingBackend()
	if len(pending) != 1 {
		t.Fatalf("lost uncertain operation: %v", pending)
	}
	if err := restarted.RecoverBackend(pending[0], DefinitelyCompleted); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Destroy(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.builds != 1 || f.creates != 0 {
		t.Fatal("restart built or published")
	}
}
func TestCandidateAdmissionCleanupOwnershipAndTTL(t *testing.T) {
	p := ReferencePolicy()
	p.MaxGenerations = 1
	p.MaxBackendOperations = 1
	f := newBuilder()
	s, _, _ := openBuilder(t, f, &p)
	i := identity(t, "s", "one")
	other := identity(t, "s", "two")
	if _, err := s.CreateDockerfile(context.Background(), i, "FROM alpine"); !errors.Is(err, ErrDockerfilePolicy) {
		t.Fatal(err)
	}
	assertState(t, s, i, Absent)
	r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
	if err != nil {
		t.Fatal(err)
	}
	receiveExact(t, f.started)
	if _, err := s.CreateDockerfile(context.Background(), other, "FROM scratch"); !errors.Is(err, ErrGenerationLimit) {
		t.Fatal(err)
	}
	if _, err := s.backend.Acquire(Exact{other.Workspace(), r.Exact.Generation}, "test"); !errors.Is(err, ErrBackendLimit) {
		t.Fatal(err)
	}
	f.finish <- BackendResult{DefinitelyCompleted, injected}
	waitBuildState(t, s, i, Failed)
	sentinel := filepath.Join(s.dataDir, "unrelated")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.cleanupErr = injected
	f.mu.Unlock()
	if err := s.Destroy(context.Background(), i); err == nil {
		t.Fatal("cleanup failure became absent")
	}
	assertState(t, s, i, Failed)
	f.mu.Lock()
	f.cleanupErr = nil
	f.mu.Unlock()
	// Expired candidates count toward admission and are reclaimed only under TTL.
	s.mu.Lock()
	now := s.lifetimes[r.Exact].Created.Add(p.AbsoluteTTL)
	s.now = func() time.Time { return now }
	s.mu.Unlock()
	if err := s.CleanupExpired(context.Background(), r.Exact); err != nil {
		t.Fatal(err)
	}
	assertState(t, s, i, Absent)
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatal("unrelated Data removed")
	}
}

func TestExpiredBuildingCandidateCannotPublish(t *testing.T) {
	f := newBuilder()
	f.cancelObserved = make(chan struct{})
	s, _, _ := openBuilder(t, f, nil)
	i := identity(t, "s", "expired-build")
	r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
	if err != nil {
		t.Fatal(err)
	}
	receiveExact(t, f.started)
	s.mu.Lock()
	now := s.lifetimes[r.Exact].Created.Add(s.policy.AbsoluteTTL)
	s.now = func() time.Time { return now }
	s.mu.Unlock()
	if err := s.CleanupExpired(context.Background(), r.Exact); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	<-f.cancelObserved
	f.finish <- BackendResult{Completion: DefinitelyCompleted}
	waitBuildState(t, s, i, Absent)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 0 {
		t.Fatal("TTL-revoked candidate published")
	}
}
func TestRestartFencesFinalizingWithoutPublishing(t *testing.T) {
	f := newBuilder()
	f.createStarted = make(chan Exact, 1)
	f.createFinish = make(chan error, 1)
	s, cfg, lock := openBuilder(t, f, nil)
	i := identity(t, "s", "restart-final")
	r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
	if err != nil {
		t.Fatal(err)
	}
	receiveExact(t, f.started)
	f.finish <- BackendResult{Completion: DefinitelyCompleted}
	receiveExact(t, f.createStarted)
	// Close revokes admission before the trusted adapter returns a late container.
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	for {
		s.mu.Lock()
		ready := s.ready
		s.mu.Unlock()
		if !ready {
			break
		}
		time.Sleep(time.Millisecond)
	}
	f.createFinish <- nil
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	restarted, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	assertState(t, restarted, i, Failed)
	pending := restarted.PendingBackend()
	if len(pending) != 1 || pending[0].Kind != "create" {
		t.Fatalf("lost finalizing uncertainty: %v", pending)
	}
	if err := restarted.RecoverBackend(pending[0], DefinitelyCompleted); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Destroy(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	assertState(t, restarted, i, Absent)
	if err := restarted.WithGeneration(r.Exact, func() error { t.Error("late callback ran"); return nil }); !errors.Is(err, ErrStaleGeneration) {
		t.Fatal(err)
	}
}
func TestBuildJournalFailureClosesPublication(t *testing.T) {
	f := newBuilder()
	s, _, _ := openBuilder(t, f, nil)
	i := identity(t, "s", "journal-fail")
	r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
	if err != nil {
		t.Fatal(err)
	}
	receiveExact(t, f.started)
	s.mu.Lock()
	s.save = func(string, string, map[WorkspaceKey]Exact, map[Exact]Lifetime, bool, BackendState, map[Exact]GenerationRecord) error {
		return injected
	}
	s.mu.Unlock()
	f.finish <- BackendResult{Completion: DefinitelyCompleted}
	s.buildWG.Wait()
	if _, err := s.Status(i); !errors.Is(err, ErrNotReady) {
		t.Fatal("journal failure left admission open")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 0 {
		t.Fatal("failed FINALIZING intent published")
	}
	if !f.images[r.Exact] {
		t.Fatal("uncertain residue was silently erased")
	}
}
func TestToolDeadlineWhileLifecycleLockIsHeld(t *testing.T) {
	f := newBuilder()
	s, _, _ := openBuilder(t, f, nil)
	i := identity(t, "s", "deadline")
	// This trusted exact is deliberately absent; only deadline behavior is tested.
	e := Exact{i.Workspace(), Generation(strings.Repeat("f", 64))}
	s.mu.Lock()
	s.toolTimeout = 10 * time.Millisecond
	done := make(chan error, 1)
	go func() { _, err := s.Exec(context.Background(), i, e, ExecRequest{Argv: []string{"true"}}); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("tool blocked beyond deadline")
	}
	s.mu.Unlock()
}

func TestRecoveryRevokesPublicationAndRetainsWorkerOwnership(t *testing.T) {
	f := newBuilder()
	f.cancelObserved = make(chan struct{})
	s, _, _ := openBuilder(t, f, nil)
	i := identity(t, "s", "recover-active")
	r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
	if err != nil {
		t.Fatal(err)
	}
	receiveExact(t, f.started)
	op := s.PendingBackend()[0]
	wrong := op
	wrong.ID++
	if err := s.RecoverBackend(wrong, DefinitelyCompleted); !errors.Is(err, ErrStaleGeneration) {
		t.Fatal(err)
	}
	assertState(t, s, i, Processing)
	if err := s.RecoverBackend(op, DefinitelyCompleted); err != nil {
		t.Fatal(err)
	}
	assertState(t, s, i, Failed)
	<-f.cancelObserved
	if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("live worker ownership lost: %v", err)
	}
	if _, err := os.Lstat(s.buildPath(r.Exact)); err != nil {
		t.Fatal("live worker Data disappeared")
	}
	f.finish <- BackendResult{Completion: DefinitelyCompleted}
	waitBuildState(t, s, i, Absent)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 0 || len(f.images) != 0 {
		t.Fatal("recovered worker republished or left residue")
	}
}

func TestBuildOrphanImageAndDataFailReconciliation(t *testing.T) {
	for _, mode := range []string{"image", "data", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			f := newBuilder()
			dir := t.TempDir()
			cfg := testConfig(filepath.Join(dir, "state"))
			cfg.DataDir = filepath.Join(dir, "data")
			lock := filepath.Join(dir, "lock", "daemon.lock")
			e := Exact{identity(t, "s", "orphan").Workspace(), Generation(strings.Repeat("b", 64))}
			if mode == "image" {
				f.images[e] = true
			} else {
				if err := os.MkdirAll(filepath.Join(cfg.DataDir, "builds"), 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(cfg.DataDir, "builds", objectName(e))
				if mode == "symlink" {
					if err := os.Symlink(dir, path); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			s, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
			if err == nil {
				s.Close()
				t.Fatal("orphan generation adopted")
			}
		})
	}
}
