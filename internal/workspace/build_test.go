//go:build linux

package workspace

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A small lifecycle adapter, not an engine emulator. Channels hold only the
// transitions whose serialization/publication behavior is under test.
type asyncRuntime struct {
	mu               sync.Mutex
	objects          map[Exact]Object
	started          chan Exact
	finish           chan error
	finalStarted     chan Exact
	finalFinish      chan error
	builds, cleanups int
	text             string
	cleanupErr       error
}

func newAsyncRuntime() *asyncRuntime {
	return &asyncRuntime{objects: map[Exact]Object{}, started: make(chan Exact, 4), finish: make(chan error, 4)}
}
func (*asyncRuntime) Initialize(context.Context) error { return nil }
func (r *asyncRuntime) List(context.Context) ([]Object, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []Object
	for _, o := range r.objects {
		result = append(result, o)
	}
	return result, nil
}
func (r *asyncRuntime) Build(ctx context.Context, e Exact, text, _ string) (string, error) {
	r.mu.Lock()
	r.builds++
	r.text = text
	r.mu.Unlock()
	r.started <- e
	select {
	case err := <-r.finish:
		return "sha256:" + string(e.Generation), err
	case <-ctx.Done():
		return "", CompletedBackendError(ctx.Err())
	}
}
func (r *asyncRuntime) Create(_ context.Context, e Exact, image string) error {
	if r.finalStarted != nil {
		r.finalStarted <- e
		if err := <-r.finalFinish; err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.objects[e] = Object{ID: string(e.Generation), Name: objectName(e), Image: image, Labels: objectLabels(e, image)}
	return nil
}
func (r *asyncRuntime) Destroy(_ context.Context, e Exact) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.objects, e)
	return nil
}
func (r *asyncRuntime) CleanupBuild(context.Context, Exact, string, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanups++
	return r.cleanupErr
}
func (*asyncRuntime) StartExec(context.Context, ProcessSnapshot, ExecRequest, io.Writer, io.Writer) (<-chan ExecOutcome, error) {
	return nil, errors.New("candidate must never reach exec")
}

func openAsync(t *testing.T, r *asyncRuntime) (*Service, Config, string, Identity) {
	t.Helper()
	dir := t.TempDir()
	cfg, lock := testConfig(filepath.Join(dir, "state")), filepath.Join(dir, "lock", "daemon.lock")
	s, err := open(context.Background(), cfg, func() Runtime { return r }, lock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, cfg, lock, identity(t, "subject", "conversation")
}

func awaitWorker(t *testing.T, s *Service) {
	t.Helper()
	done := make(chan struct{})
	go func() { s.workerDone.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("candidate worker did not terminate")
	}
}

func receiveExact(t *testing.T, ch <-chan Exact) Exact {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("transition was not reached")
		return Exact{}
	}
}

func TestAsyncCandidatePollFinalizationAndPresent(t *testing.T) {
	r := newAsyncRuntime()
	r.finalStarted, r.finalFinish = make(chan Exact, 1), make(chan error, 1)
	s, _, _, i := openAsync(t, r)
	text := "FROM busybox\nRUN true\n"
	first, err := s.CreateWorkspace(context.Background(), i, &text)
	if err != nil || first.Status != "PROCESSING" {
		t.Fatal(first, err)
	}
	if receiveExact(t, r.started) != first.Exact {
		t.Fatal("different candidate")
	}
	before := s.lifetimes[first.Exact].Activity
	invalid := ""
	for range 3 {
		poll, err := s.CreateWorkspace(context.Background(), i, &invalid)
		if err != nil || poll != first {
			t.Fatal("poll replaced candidate", poll, err)
		}
	}
	if len(s.PendingBackend()) != 2 {
		t.Fatal("build lost accounting")
	}
	s.mu.Lock()
	s.policy.MaxGenerations = 1
	s.mu.Unlock()
	if _, err := s.CreateWorkspace(context.Background(), identity(t, "other", "conversation"), &text); !errors.Is(err, ErrGenerationLimit) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, execErr := s.Exec(ctx, i, first.Exact, ExecRequest{Argv: []string{"true"}})
	_, readErr := s.ReadText(ctx, i, first.Exact, ReadTextRequest{Path: "/f"})
	_, writeErr := s.WriteText(ctx, i, first.Exact, WriteTextRequest{Path: "/f"})
	for _, err := range []error{execErr, readErr, writeErr} {
		if !errors.Is(err, ErrNotReady) {
			t.Fatal(err)
		}
	}
	r.finish <- nil
	receiveExact(t, r.finalStarted)
	poll, err := s.CreateWorkspace(ctx, i, &invalid)
	if err != nil || poll != first {
		t.Fatal("finalization was published early", poll, err)
	}
	r.finalFinish <- nil
	awaitWorker(t, s)
	present, err := s.CreateWorkspace(ctx, i, &invalid)
	if err != nil || present.Status != "PRESENT" || present.Exact != first.Exact {
		t.Fatal(present, err)
	}
	if r.builds != 1 || r.text != text || len(s.PendingBackend()) != 0 || s.lifetimes[first.Exact].Activity.Before(before) {
		t.Fatal("candidate accounting/idempotency")
	}
}

func TestTerminalFailureObservationCleansBeforeFreshGeneration(t *testing.T) {
	for _, stage := range []string{"build", "finalization"} {
		t.Run(stage, func(t *testing.T) {
			r := newAsyncRuntime()
			if stage == "finalization" {
				r.finalStarted, r.finalFinish = make(chan Exact, 1), make(chan error, 1)
			}
			s, cfg, lock, i := openAsync(t, r)
			text := "FROM busybox"
			first, err := s.CreateWorkspace(context.Background(), i, &text)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "build" {
				r.finish <- CompletedBackendError(injected)
			} else {
				r.finish <- nil
				receiveExact(t, r.finalStarted)
				r.finalFinish <- CompletedBackendError(injected)
			}
			awaitWorker(t, s)
			if len(s.PendingBackend()) != 0 || len(s.entries) != 1 {
				t.Fatal("diagnostics retained a live permit or lost generation")
			}
			if stage == "build" {
				s.Close()
				s, err = open(context.Background(), cfg, func() Runtime { return r }, lock)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
			}
			corrected := "FROM alpine"
			observed, err := s.CreateWorkspace(context.Background(), i, &corrected)
			if !errors.Is(err, ErrBuildFailed) || observed.Status != "" || observed.Exact != first.Exact {
				t.Fatal(observed, err)
			}
			assertState(t, s, i, Absent)
			if r.cleanups != 1 {
				t.Fatal("failed candidate not cleaned")
			}
			later, err := s.CreateWorkspace(context.Background(), i, &corrected)
			if err != nil || later.Exact.Generation == first.Exact.Generation || later.Status != "PROCESSING" {
				t.Fatal(later, err)
			}
			// Finish before test cleanup so a held finalization never stalls Close.
			r.finish <- CompletedBackendError(injected)
			awaitWorker(t, s)
		})
	}
}

func TestUnknownBuildAndCleanupRetainExactOwnershipAcrossRestart(t *testing.T) {
	for _, uncertain := range []string{"build", "cleanup"} {
		t.Run(uncertain, func(t *testing.T) {
			r := newAsyncRuntime()
			s, cfg, lock, i := openAsync(t, r)
			text := "FROM busybox"
			first, err := s.CreateWorkspace(context.Background(), i, &text)
			if err != nil {
				t.Fatal(err)
			}
			if uncertain == "build" {
				r.finish <- injected
			} else {
				r.finish <- CompletedBackendError(injected)
			}
			awaitWorker(t, s)
			if uncertain == "cleanup" {
				r.cleanupErr = injected
				if _, err := s.CreateWorkspace(context.Background(), i, &text); !errors.Is(err, ErrBuildFailed) {
					t.Fatal(err)
				}
			}
			wantPermits := 1
			if uncertain == "build" {
				wantPermits = 2
			}
			if len(s.PendingBackend()) != wantPermits {
				t.Fatal("unknown operation lost permit")
			}
			s.Close()
			restarted, err := open(context.Background(), cfg, func() Runtime { return r }, lock)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if result, err := restarted.CreateWorkspace(context.Background(), i, &text); !errors.Is(err, ErrOutcomeUnknown) || result.Exact != first.Exact || r.builds != 1 {
				t.Fatal("restart replayed/replaced", result, err)
			}
			op := restarted.PendingBackend()[0]
			wrong := op
			wrong.ID++
			if err := restarted.RecoverBackend(wrong, DefinitelyCompleted); !errors.Is(err, ErrStaleGeneration) {
				t.Fatal(err)
			}
			for _, op := range restarted.PendingBackend() {
				if err := restarted.RecoverBackend(op, DefinitelyCompleted); err != nil {
					t.Fatal(err)
				}
			}
			r.cleanupErr = nil
			if err := restarted.DestroyGeneration(context.Background(), first.Exact); err != nil {
				t.Fatal(err)
			}
			assertState(t, restarted, i, Absent)
		})
	}
}

func TestDestroyRevokesLateFinalizationAndFencesReplacement(t *testing.T) {
	r := newAsyncRuntime()
	r.finalStarted, r.finalFinish = make(chan Exact, 1), make(chan error, 1)
	s, _, _, i := openAsync(t, r)
	text := "FROM busybox"
	first, err := s.CreateWorkspace(context.Background(), i, &text)
	if err != nil {
		t.Fatal(err)
	}
	r.finish <- nil
	receiveExact(t, r.finalStarted)
	if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	if _, err := s.CreateWorkspace(context.Background(), i, &text); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal("replacement admitted before terminal evidence", err)
	}
	// Simulate finalization that had already started and completes after revoke.
	r.finalFinish <- nil
	awaitWorker(t, s)
	assertState(t, s, i, Absent)
	later, err := s.CreateWorkspace(context.Background(), i, &text)
	if err != nil || later.Exact == first.Exact {
		t.Fatal(later, err)
	}
	if err := s.DestroyGeneration(context.Background(), first.Exact); !errors.Is(err, ErrStaleGeneration) {
		t.Fatal(err)
	}
	r.finish <- CompletedBackendError(injected)
	awaitWorker(t, s)
}

func TestBuildProgressDoesNotRenewTTLOrOverrideDurability(t *testing.T) {
	for _, mode := range []string{"idle-expired", "publication-sync-error"} {
		t.Run(mode, func(t *testing.T) {
			r := newAsyncRuntime()
			s, _, _, i := openAsync(t, r)
			text := "FROM busybox"
			first, err := s.CreateWorkspace(context.Background(), i, &text)
			if err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			life := s.lifetimes[first.Exact]
			if mode == "idle-expired" {
				s.now = func() time.Time { return life.Activity.Add(s.policy.IdleTTL) }
			} else {
				s.save = func(path, image string, entries map[WorkspaceKey]Exact, lives map[Exact]Lifetime, blocked bool, backend BackendState) error {
					if lives[first.Exact].Build.Phase == buildPresent {
						return injected
					}
					return writeAdmissionJournal(path, image, entries, lives, blocked, backend)
				}
			}
			s.mu.Unlock()
			r.finish <- nil
			awaitWorker(t, s)
			if mode == "idle-expired" {
				assertState(t, s, i, Absent)
			} else if _, err := s.Status(i); !errors.Is(err, ErrNotReady) {
				t.Fatal("published despite failed durability", err)
			}
		})
	}
}

func TestAbsentEmptyDockerfileDoesNotSelectCanonicalCreate(t *testing.T) {
	s, _, _, i := openAsync(t, newAsyncRuntime())
	for _, text := range []string{"", strings.Repeat(" ", 4), "not a dockerfile"} {
		if _, err := s.CreateWorkspace(context.Background(), i, &text); !errors.Is(err, ErrDockerfile) {
			t.Fatal(err)
		}
		assertState(t, s, i, Absent)
	}
}
