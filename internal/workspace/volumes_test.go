package workspace

import (
	"context"
	"errors"
	"testing"
)

func TestVolumeFailuresRetainDestroyPath(t *testing.T) {
	for _, stage := range []string{"volume create", "volume inspect", "volume rm"} {
		t.Run(stage, func(t *testing.T) {
			f := newFake()
			m := openFake(t, f)
			f.fail = func(_ context.Context, key string, _ int) error {
				if key == stage || (stage == "volume rm" && key == "create") {
					return errors.New("injected volume failure")
				}
				return nil
			}
			if _, err := m.Create(context.Background()); err == nil {
				t.Fatal("failure returned success")
			}
			if stage == "volume rm" || stage == "volume inspect" {
				if len(m.entries) != 1 || len(f.volumes) != 1 {
					t.Fatal("volume cleanup failure lost identity")
				}
				f.fail = nil
				for id := range m.entries {
					if err := m.Destroy(context.Background(), id); err != nil {
						t.Fatal(err)
					}
				}
			}
			if len(f.volumes) != 0 || len(f.containers) != 0 {
				t.Fatal("cleanup left workspace storage or compute")
			}
		})
	}
}

func TestOrphanVolumeRecoveryAndOwnership(t *testing.T) {
	f := newFake()
	m := openFake(t, f)
	w, err := m.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	delete(f.containers, m.entries[w.ID].container) // Interrupted transaction fixture.
	restarted := openFake(t, f)
	if e := restarted.entries[w.ID]; e == nil || !e.blocked || e.container != "" {
		t.Fatal("orphan volume not recovered as blocked")
	}
	if err := restarted.Destroy(context.Background(), w.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.volumes) != 0 {
		t.Fatal("destroy left orphan data")
	}
}

func TestVolumeProfileDriftFailsBeforeRecoveryMutation(t *testing.T) {
	for _, mutation := range []func(*volumeInspection){
		func(v *volumeInspection) { v.Driver = "nfs" },
		func(v *volumeInspection) { v.Options = map[string]string{"device": "/host"} },
		func(v *volumeInspection) { v.Labels[createdLabel] = "2026-01-01T00:00:00Z" },
		func(v *volumeInspection) { v.Labels[idLabel] = "ws_bad" },
	} {
		f := newFake()
		m := openFake(t, f)
		if _, err := m.Create(context.Background()); err != nil {
			t.Fatal(err)
		}
		for name, v := range f.volumes {
			mutation(&v)
			f.volumes[name] = v
		}
		if _, err := New(context.Background(), f); err == nil {
			t.Fatal("drifted volume accepted")
		}
		if f.counts["stop"] != 0 {
			t.Fatal("recovery mutated before validating volume ownership")
		}
	}
}
