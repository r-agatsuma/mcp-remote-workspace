//go:build linux

package workspace

import (
	"context"
	"errors"
	"testing"
)

func TestCurrentGenerationLifecycleBinding(t *testing.T) {
	s, f, i, first := openTools(t, Config{})
	ctx := context.Background()
	exact, err := s.CurrentGeneration(ctx, i)
	if err != nil || exact != first {
		t.Fatal(exact, err)
	}
	for _, phase := range []string{buildBuilding, buildFinalizing, buildFailed, buildRecovery} {
		s.mu.Lock()
		life := s.lifetimes[first]
		life.Build = &buildIntent{Phase: phase, Image: "sha256:" + string(first.Generation)}
		s.lifetimes[first] = life
		s.mu.Unlock()
		if e, err := s.CurrentGeneration(ctx, i); !errors.Is(err, ErrNotReady) || e != (Exact{}) {
			t.Fatal(phase, e, err)
		}
	}
	s.mu.Lock()
	life := s.lifetimes[first]
	life.Build = nil
	s.lifetimes[first] = life
	s.mu.Unlock()
	if err := s.Destroy(ctx, i); err != nil {
		t.Fatal(err)
	}
	creates := f.creates
	if e, err := s.CurrentGeneration(ctx, i); !errors.Is(err, ErrStaleGeneration) || e != (Exact{}) {
		t.Fatal(e, err)
	}
	if f.creates != creates {
		t.Fatal("lookup created workspace")
	}
	second, err := s.Create(ctx, i)
	if err != nil || second == first {
		t.Fatal(second, err)
	}
	f.snapshot = safeSnapshot(second, s.policy, 1)
	// All three operations using the old binding must fail at the inherited fence.
	_, err = s.Exec(ctx, i, exact, ExecRequest{Argv: []string{"true"}})
	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatal(err)
	}
	_, err = s.ReadText(ctx, i, exact, ReadTextRequest{Path: "/f"})
	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatal(err)
	}
	_, err = s.WriteText(ctx, i, exact, WriteTextRequest{Path: "/f", Content: "x"})
	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatal(err)
	}
	if f.calls != 0 {
		t.Fatal("stale binding reached replacement")
	}
	if current, err := s.CurrentGeneration(ctx, i); err != nil || current != second {
		t.Fatal(current, err)
	}
}

func TestCurrentGenerationClosedCanceledAndContended(t *testing.T) {
	s, _, i, _ := openTools(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.CurrentGeneration(ctx, i); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s.mu.Lock()
	if _, err := s.CurrentGeneration(context.Background(), i); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	s.ready = false
	s.mu.Unlock()
	if _, err := s.CurrentGeneration(context.Background(), i); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
}

func TestCurrentGenerationRejectsUncertainLifecycle(t *testing.T) {
	f := newFake()
	s, _, _ := openFake(t, f)
	i := identity(t, "uncertain", "session")
	f.createErr = ErrOutcomeUnknown
	if _, err := s.Create(context.Background(), i); err == nil {
		t.Fatal("fixture must fail create")
	}
	if exact, err := s.CurrentGeneration(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) || exact != (Exact{}) {
		t.Fatal("uncertain lifecycle admitted", exact, err)
	}
}
