//go:build linux

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A deterministic engine persists only unlabelled Buildah objects. Ordinary
// Podman inspect/remove are deliberately unsupported, as in the #72 finding.
type storageEngine struct {
	mu                sync.Mutex
	markers           map[string]string
	started, canceled chan struct{}
	finish            chan BackendResult
	late, retain      bool
}

func (f *storageEngine) run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch strings.Join(args, " ") {
	case "info --format=json":
		return []byte(`{"host":{"security":{"rootless":true}}}`), nil
	case "image list --all --no-trunc --format=json", "container list --all --no-trunc --format=json":
		return []byte("[]"), nil
	case "container list --all --external --no-trunc --format=json":
		v := []map[string]string{}
		for id := range f.markers {
			v = append(v, map[string]string{"Id": id})
		}
		return json.Marshal(v)
	}
	return nil, errors.New("unsupported ordinary Podman command")
}

func (f *storageEngine) runBuild(ctx context.Context, _ string, _ string, _, _ io.Writer, args ...string) BackendResult {
	module := strings.TrimPrefix(args[0], "--module=")
	data, err := os.ReadFile(module)
	if err != nil {
		return BackendResult{DefinitelyCompleted, err}
	}
	_, quoted, ok := strings.Cut(strings.TrimSpace(string(data)), "default_mounts_file = ")
	marker, err := strconv.Unquote(quoted)
	if !ok || err != nil {
		return BackendResult{DefinitelyCompleted, ErrNotReady}
	}
	f.mu.Lock()
	f.markers[strings.Repeat("f", 64)] = marker
	f.mu.Unlock()
	close(f.started)
	<-ctx.Done()
	close(f.canceled)
	if f.late {
		return <-f.finish
	}
	return BackendResult{OutcomeUnknown, ErrOutcomeUnknown}
}

type storageCommands struct{ engine *storageEngine }

func (c storageCommands) run(_ context.Context, args ...string) ([]byte, error) {
	f := c.engine
	f.mu.Lock()
	defer f.mu.Unlock()
	id := args[len(args)-1]
	marker, exists := f.markers[id]
	if !exists {
		return nil, ErrOutcomeUnknown
	}
	switch args[0] {
	case "inspect":
		return json.Marshal(map[string]string{"Type": "buildah 0.0.1", "ContainerID": id, "DefaultMountsFilePath": marker})
	case "rm":
		if !f.retain {
			delete(f.markers, id)
		}
		return nil, nil
	}
	return nil, ErrOutcomeUnknown
}

type storageLifecycleBuilder struct {
	*fakeBuilder
	podman *Podman
}

func (f *storageLifecycleBuilder) Initialize(ctx context.Context) error {
	return f.podman.Initialize(ctx)
}
func (f *storageLifecycleBuilder) BuildGenerations(ctx context.Context) ([]Exact, error) {
	return f.podman.BuildGenerations(ctx)
}
func (f *storageLifecycleBuilder) Build(ctx context.Context, e Exact, plan BuildPlan, path string, out, diagnostic io.Writer) BackendResult {
	return f.podman.Build(ctx, e, plan, path, out, diagnostic)
}
func (f *storageLifecycleBuilder) CleanupBuild(ctx context.Context, e Exact, path string) error {
	return f.podman.CleanupBuild(ctx, e, path)
}

func storageLifecycleFixture(t *testing.T) (*storageLifecycleBuilder, *storageEngine, Config, string) {
	t.Helper()
	root := t.TempDir()
	cfg := testConfig(filepath.Join(root, "state"))
	cfg.DataDir = filepath.Join(root, "data")
	f := &storageEngine{markers: map[string]string{}, started: make(chan struct{}), canceled: make(chan struct{}), finish: make(chan BackendResult, 1)}
	p := &Podman{runner: f, buildah: storageCommands{f}, image: testImage, buildConfig: PodmanBuildConfig{DataDir: cfg.DataDir}}
	return &storageLifecycleBuilder{newBuilder(), p}, f, cfg, filepath.Join(root, "lock", "daemon.lock")
}

func waitStorageSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("storage worker signal timed out")
	}
}

func TestExternalStorageCancellationRecoveryAndLateCompletion(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(strconv.FormatBool(late), func(t *testing.T) {
			f, engine, cfg, lock := storageLifecycleFixture(t)
			engine.late = late
			s, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			i := identity(t, "s", "storage-cancel")
			r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch\nRUN true")
			if err != nil {
				t.Fatal(err)
			}
			waitStorageSignal(t, engine.started)
			if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatal(err)
			}
			waitStorageSignal(t, engine.canceled)
			if !late {
				s.buildWG.Wait()
			}
			op := s.PendingBackend()[0]
			if err := s.RecoverBackend(op, DefinitelyCompleted); err != nil {
				t.Fatal(err)
			}
			if late {
				if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal("late worker lost ownership", err)
				}
				engine.finish <- BackendResult{DefinitelyCompleted, injected}
				waitBuildState(t, s, i, Absent)
			} else {
				engine.mu.Lock()
				engine.retain = true
				engine.mu.Unlock()
				if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal("retained storage became ABSENT", err)
				}
				assertState(t, s, i, Failed)
				if _, err := os.Stat(s.buildPath(r.Exact)); err != nil {
					t.Fatal("Data removed before storage absence")
				}
				for _, pending := range s.PendingBackend() {
					if err := s.RecoverBackend(pending, DefinitelyCompleted); err != nil {
						t.Fatal(err)
					}
				}
				engine.mu.Lock()
				engine.retain = false
				engine.mu.Unlock()
				if err := s.Destroy(context.Background(), i); err != nil {
					t.Fatal(err)
				}
				assertState(t, s, i, Absent)
			}
			engine.mu.Lock()
			remaining := len(engine.markers)
			engine.mu.Unlock()
			if remaining != 0 {
				t.Fatal("exact external storage survived ABSENT")
			}
			if _, err := os.Lstat(s.buildPath(r.Exact)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("Data survived ABSENT")
			}
			if err := s.DestroyGeneration(context.Background(), r.Exact); !errors.Is(err, ErrStaleGeneration) {
				t.Fatal(err)
			}
			if f.creates != 0 {
				t.Fatal("canceled candidate published")
			}
		})
	}
}

func TestExternalStorageRestartReconciliationAndOrphan(t *testing.T) {
	f, engine, cfg, lock := storageLifecycleFixture(t)
	s, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal(err)
	}
	i := identity(t, "s", "storage-restart")
	r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch\nRUN true")
	if err != nil {
		t.Fatal(err)
	}
	waitStorageSignal(t, engine.started)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal(err)
	}
	assertState(t, restarted, i, Failed)
	for _, op := range restarted.PendingBackend() {
		if err := restarted.RecoverBackend(op, DefinitelyCompleted); err != nil {
			t.Fatal(err)
		}
	}
	if err := restarted.Destroy(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	assertState(t, restarted, i, Absent)
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	// Same exact external marker without journal authority cannot be adopted.
	engine.mu.Lock()
	engine.markers[strings.Repeat("f", 64)] = filepath.Join(f.podman.candidateBuildPath(r.Exact), buildMountsFile)
	engine.mu.Unlock()
	if orphan, err := open(context.Background(), cfg, func() Runtime { return f }, lock); !errors.Is(err, ErrStaleGeneration) {
		if orphan != nil {
			orphan.Close()
		}
		t.Fatal("orphan external storage escaped reconciliation", err)
	}
}
