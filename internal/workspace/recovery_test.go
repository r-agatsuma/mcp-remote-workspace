//go:build linux

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const previousBoot = "11111111-1111-4111-8111-111111111111"
const currentBoot = "22222222-2222-4222-8222-222222222222"

func TestKernelBootEpoch(t *testing.T) {
	boot, err := kernelBootID()
	if err != nil || !validBootID(boot) {
		t.Fatal(boot, err)
	}
	for _, bad := range []string{"", strings.Repeat("x", 36), strings.Repeat("a", 4096), "00000000-0000-0000-0000-000000000000", currentBoot + "\n", currentBoot + "\n" + previousBoot, " " + currentBoot, strings.Replace(currentBoot, "-", "/", 1)} {
		if validBootID(bad) {
			t.Fatal("invalid epoch accepted", bad)
		}
	}
}

func TestRebootRecoveryEveryPersistedKindAndRestart(t *testing.T) {
	for _, kind := range []string{"create", "destroy", "injection", "build-control", "build", "arbitrary-host-kind"} {
		t.Run(kind, func(t *testing.T) {
			f := newFake()
			s, cfg, lock := openFake(t, f)
			e, err := s.Create(context.Background(), identity(t, "s", "c"))
			if err != nil {
				t.Fatal(err)
			}
			s.backend.bootID = previousBoot
			permit, err := s.backend.Acquire(e, kind)
			if err != nil {
				t.Fatal(err)
			}
			op := permit.Operation()
			if err := s.commit(clone(s.entries)); err != nil {
				t.Fatal(err)
			}
			s.Close()
			r, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
			if err != nil {
				t.Fatal(err)
			}
			r.bootID = func() (string, error) { return currentBoot, nil }
			if len(r.PendingBackend()) != 1 || r.PendingBackend()[0] != op {
				t.Fatal("startup released or changed operation")
			}
			snapshot, err := r.RecoverySnapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			selection := snapshot.Operations[0].Selection
			if snapshot.Operations[0].Evidence != "prior_boot_terminal" {
				t.Fatal(snapshot)
			}
			if err := r.RecoverPendingBackend(context.Background(), selection); err != nil {
				t.Fatal(err)
			}
			if r.entries[e.Workspace] != e || len(f.objects) != 1 || f.destroys != 0 || f.creates != 1 {
				t.Fatal("reboot evidence implied absence or replay")
			}
			if r.injections[e] != nil || len(r.PendingBackend()) != 0 {
				t.Fatal("reservation retained after evidence")
			}
			if err := r.RecoverPendingBackend(context.Background(), selection); !errors.Is(err, ErrStaleGeneration) {
				t.Fatal("stale selection accepted", err)
			}
			r.Close()
			r, err = open(context.Background(), cfg, func() Runtime { return f }, lock)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if len(r.PendingBackend()) != 0 || r.entries[e.Workspace] != e {
				t.Fatal("recovery not durable")
			}
			snapshot, err = r.RecoverySnapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			cleanup := snapshot.Generations[0].Selection
			if err := r.CleanupRecoveryGeneration(context.Background(), cleanup); err != nil {
				t.Fatal(err)
			}
			if len(r.entries) != 0 || len(f.objects) != 0 {
				t.Fatal("exact cleanup did not finish")
			}
			if err := r.CleanupRecoveryGeneration(context.Background(), cleanup); !errors.Is(err, ErrStaleGeneration) {
				t.Fatal("stale cleanup accepted", err)
			}
		})
	}
}

func TestRebootRecoveryUnknownEpochAndMismatchedOperation(t *testing.T) {
	for _, epoch := range []string{"", currentBoot, previousBoot} {
		t.Run(epoch, func(t *testing.T) {
			s, _, _ := openFake(t, newFake())
			e, err := s.Create(context.Background(), identity(t, "s", "c"))
			if err != nil {
				t.Fatal(err)
			}
			s.backend.bootID = epoch
			permit, _ := s.backend.Acquire(e, "injection")
			op := permit.Operation()
			s.injections[e] = permit
			if err := s.commit(clone(s.entries)); err != nil {
				t.Fatal(err)
			}
			s.bootID = func() (string, error) { return currentBoot, nil }
			for _, field := range []string{"id", "workspace", "generation", "kind", "boot"} {
				wrong := op
				switch field {
				case "id":
					wrong.ID++
				case "workspace":
					wrong.Exact.Workspace = WorkspaceKey(strings.Repeat("e", 64))
				case "generation":
					wrong.Exact.Generation = Generation(strings.Repeat("f", 64))
				case "kind":
					wrong.Kind = "destroy"
				case "boot":
					wrong.BootID = "33333333-3333-4333-8333-333333333333"
				}
				if err := s.RecoverBackendAfterReboot(context.Background(), wrong); !errors.Is(err, ErrStaleGeneration) {
					t.Fatal(field, err)
				}
			}
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if err := s.RecoverBackendAfterReboot(canceled, op); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if epoch == previousBoot {
				s.bootID = func() (string, error) { return "", injected }
				if err := s.RecoverBackendAfterReboot(context.Background(), op); !errors.Is(err, ErrNotReady) {
					t.Fatal(err)
				}
			} else if err := s.RecoverBackendAfterReboot(context.Background(), op); !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatal(err)
			}
			if len(s.PendingBackend()) != 1 || s.injections[e] != permit {
				t.Fatal("unknown evidence released ownership")
			}
		})
	}
}

func TestRebootRecoveryReleasesOnlyExactOperation(t *testing.T) {
	s, _, _ := openFake(t, newFake())
	e, _ := s.Create(context.Background(), identity(t, "one", "c"))
	other, _ := s.Create(context.Background(), identity(t, "two", "c"))
	s.backend.bootID = previousBoot
	chosen, _ := s.backend.Acquire(e, "create")
	s.backend.bootID = currentBoot
	sibling, _ := s.backend.Acquire(e, "destroy")
	s.backend.bootID = previousBoot
	neighbor, _ := s.backend.Acquire(other, "create")
	s.bootID = func() (string, error) { return currentBoot, nil }
	if err := s.RecoverBackendAfterReboot(context.Background(), chosen.Operation()); err != nil {
		t.Fatal(err)
	}
	pending := s.PendingBackend()
	if len(pending) != 2 {
		t.Fatal(pending)
	}
	for _, want := range []BackendOperation{sibling.Operation(), neighbor.Operation()} {
		found := false
		for _, got := range pending {
			found = found || got == want
		}
		if !found {
			t.Fatal("released another operation", want)
		}
	}
	// The standalone ledger also fences a later generation for the same W.
	ledger, err := NewBackendLedger(4, 0)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := ledger.Acquire(e, "create")
	later := e
	later.Generation = Generation(strings.Repeat("f", 64))
	future, _ := ledger.Acquire(later, "create")
	if err := ledger.Recover(old.Operation(), DefinitelyCompleted); err != nil {
		t.Fatal(err)
	}
	remaining := ledger.Pending()
	if len(remaining) != 1 || remaining[0] != future.Operation() {
		t.Fatal("later generation permit released", remaining)
	}

}

func TestRebootRecoveryCommitFailureClosesAdmission(t *testing.T) {
	for _, storage := range []bool{false, true} {
		t.Run(map[bool]string{false: "backend", true: "storage"}[storage], func(t *testing.T) {
			f := newFake()
			s, cfg, lock := openFake(t, f)
			e, _ := s.Create(context.Background(), identity(t, "s", "c"))
			s.backend.bootID = previousBoot
			permit, _ := s.backend.Acquire(e, "injection")
			s.injections[e] = permit
			s.storageBlocked = storage
			if err := s.commit(clone(s.entries)); err != nil {
				t.Fatal(err)
			}
			s.bootID = func() (string, error) { return currentBoot, nil }
			s.save = func(string, string, map[WorkspaceKey]Exact, map[Exact]Lifetime, bool, BackendState) error {
				return injected
			}
			var err error
			if storage {
				err = s.RecoverStorage(context.Background())
			} else {
				err = s.RecoverBackendAfterReboot(context.Background(), permit.Operation())
			}
			if !errors.Is(err, injected) || s.ready {
				t.Fatal("failed commit reopened admission", err)
			}
			if _, err := s.Create(context.Background(), identity(t, "new", "c")); !errors.Is(err, ErrNotReady) {
				t.Fatal(err)
			}
			s.Close()
			r, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if len(r.PendingBackend()) != 1 || r.storageBlocked != storage {
				t.Fatal("old durable reservation/latch lost")
			}
		})
	}
}

func TestOperatorStorageRecoveryPersistedAndNoDeletion(t *testing.T) {
	f := newFake()
	sample := StorageSample{1, 1}
	s, cfg, lock := openControlled(t, f, Config{Storage: storageFunc(func(context.Context, string) (StorageSample, error) { return sample, nil })})
	marker := filepath.Join(cfg.StateDir, "operator-owned-file")
	if err := os.WriteFile(marker, []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	i := identity(t, "s", "c")
	if _, err := s.Create(context.Background(), i); !errors.Is(err, ErrStorageRecovery) {
		t.Fatal(err)
	}
	s.Close()
	sample = StorageSample{1 << 40, 1 << 30}
	r, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(context.Background(), i); !errors.Is(err, ErrStorageRecovery) {
		t.Fatal("safe restart cleared latch", err)
	}
	for _, unsafe := range []StorageSample{{0, 1 << 30}, {1 << 40, 0}} {
		sample = unsafe
		if err := r.RecoverStorage(context.Background()); !errors.Is(err, ErrStorageRecovery) {
			t.Fatal(err)
		}
		if !r.storageBlocked {
			t.Fatal("unsafe recovery cleared latch")
		}
	}
	sample = StorageSample{1 << 40, 1 << 30}
	if err := r.RecoverStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Close()
	r, err = open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.storageBlocked {
		t.Fatal("recovery not durable")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "retain" {
		t.Fatal("recovery deleted file", err)
	}
	if _, err := r.Create(context.Background(), i); err != nil {
		t.Fatal(err)
	}
}

func TestStorageRecoveryCanceledSampleRetainsLatch(t *testing.T) {
	for _, duringSample := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-sample", true: "during-sample"}[duringSample], func(t *testing.T) {
			s, _, _ := openFake(t, newFake())
			s.storageBlocked = true
			if err := s.commit(clone(s.entries)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if !duringSample {
				cancel()
			}
			s.storage = storageFunc(func(context.Context, string) (StorageSample, error) {
				if !duringSample {
					t.Fatal("sampled after cancellation")
				}
				cancel()
				return StorageSample{1 << 40, 1 << 30}, nil
			})
			if err := s.RecoverStorage(ctx); !errors.Is(err, context.Canceled) || !s.storageBlocked {
				t.Fatal("canceled recovery cleared latch", err)
			}
			_, _, blocked, _, err := readAdmissionJournal(s.path, s.image)
			if err != nil || !blocked {
				t.Fatal("durable latch changed", err)
			}
		})
	}
}

func TestBootEpochIsDurableBeforeSubmissionAndHistoricalMissingRemainsUnknown(t *testing.T) {
	f := newFake()
	s, cfg, lock := openFake(t, f)
	f.afterCreate = func() {
		_, _, _, state, err := readAdmissionJournal(s.path, s.image)
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Operations) != 1 || state.Operations[0].BootID != s.backend.bootID || !validBootID(state.Operations[0].BootID) {
			t.Fatal("boot identity not durable before submission", state)
		}
	}
	e, err := s.Create(context.Background(), identity(t, "s", "c"))
	if err != nil {
		t.Fatal(err)
	}
	s.backend.bootID = ""
	permit, _ := s.backend.Acquire(e, "create") // historical format fixture
	if err := s.commit(clone(s.entries)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	r, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.bootID = func() (string, error) { return currentBoot, nil }
	if err := r.RecoverBackendAfterReboot(context.Background(), permit.Operation()); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal("missing historical boot became proof", err)
	}
	if len(r.PendingBackend()) != 1 {
		t.Fatal("historical permit lost")
	}
}

func TestRecoveryUncertainCommitAfterWriteClosesAdmission(t *testing.T) {
	f := newFake()
	s, cfg, lock := openFake(t, f)
	e, _ := s.Create(context.Background(), identity(t, "s", "c"))
	s.backend.bootID = previousBoot
	permit, _ := s.backend.Acquire(e, "create")
	sibling, _ := s.backend.Acquire(e, "destroy")
	if err := s.commit(clone(s.entries)); err != nil {
		t.Fatal(err)
	}
	s.bootID = func() (string, error) { return currentBoot, nil }
	s.save = func(path, image string, entries map[WorkspaceKey]Exact, lives map[Exact]Lifetime, blocked bool, state BackendState) error {
		if err := writeAdmissionJournal(path, image, entries, lives, blocked, state); err != nil {
			return err
		}
		return injected
	}
	if err := s.RecoverBackendAfterReboot(context.Background(), permit.Operation()); !errors.Is(err, injected) || s.ready {
		t.Fatal("uncertain commit reopened admission", err)
	}
	s.Close()
	r, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ops := r.PendingBackend()
	if len(ops) != 1 || ops[0] != sibling.Operation() || r.entries[e.Workspace] != e {
		t.Fatal("exact committed recovery was not preserved", ops)
	}
}
