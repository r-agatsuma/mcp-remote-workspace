package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/r-agatsuma/mcp-remote-workspace/internal/control"
)

type operationFake struct {
	*fakePodman
	fn        func(context.Context, control.Request) ([]byte, error)
	opCalls   int
	leftovers bool
	blocked   map[string]bool
	lastArgs  []string
}

func newOperationFake() *operationFake {
	return &operationFake{fakePodman: newFake(), blocked: map[string]bool{}}
}

func (f *operationFake) Blocked(id string) (bool, error)  { return f.blocked[id], nil }
func (f *operationFake) Block(id string) error            { f.blocked[id] = true; return nil }
func (f *operationFake) UnblockDestroyed(id string) error { delete(f.blocked, id); return nil }

func (f *operationFake) Run(ctx context.Context, args ...string) ([]byte, error) {
	data, err := f.fakePodman.Run(ctx, args...)
	if err == nil && args[0] == "stop" {
		f.leftovers = false
	}
	return data, err
}

func (f *operationFake) Operate(ctx context.Context, input []byte, limit int64, args ...string) ([]byte, error) {
	f.opCalls++
	f.lastArgs = append([]string(nil), args...)
	if f.leftovers {
		return nil, errors.New("previous execution still active")
	}
	f.leftovers = true // Includes hypothetical daemonized children and dead helpers.
	var req control.Request
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}
	if int64(len(input)) > limit {
		return nil, errors.New("bad transport limit")
	}
	if f.fn != nil {
		return f.fn(ctx, req)
	}
	zero := 0
	return json.Marshal(control.Response{Exec: &control.ExecResult{ExitCode: &zero, Stdout: "hello"}})
}

func operationWorkspace(t *testing.T) (*Manager, *operationFake, Workspace) {
	t.Helper()
	f := newOperationFake()
	m, err := New(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	w, err := m.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m, f, w
}

func TestSuccessAlwaysResetsBeforeReturning(t *testing.T) {
	m, f, w := operationWorkspace(t)
	id := m.entries[w.ID].container
	r, err := m.Exec(context.Background(), w.ID, ExecOptions{Argv: []string{"printf", "hello"}})
	if err != nil || r.Stdout != "hello" || f.leftovers || f.counts["stop"] != 1 || f.counts["start"] != 2 {
		t.Fatalf("exec: %+v %v calls=%v", r, err, f.calls)
	}
	if m.entries[w.ID].container != id || f.counts["create"] != 1 || len(f.volumes) != 1 {
		t.Fatal("reset replaced identity/rootfs/volume")
	}
	if want := fmt.Sprint([]string{"exec", "--interactive", "--user=0:0", "--workdir=/", id, controlContainerPath, "operate"}); fmt.Sprint(f.lastArgs) != want {
		t.Fatalf("control boundary: %v", f.lastArgs)
	}
}

func TestOperationFailuresAlwaysReset(t *testing.T) {
	for _, operation := range []string{"exec", "read_text", "write_text"} {
		for _, failure := range []string{"transport", "cancel", "deadline", "helper", "malformed", "missing-field", "extra-response", "oversized"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				m, f, w := operationWorkspace(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				f.fn = func(_ context.Context, req control.Request) ([]byte, error) {
					switch failure {
					case "transport":
						return nil, errors.New("podman exec died")
					case "cancel":
						cancel()
						return nil, context.Canceled
					case "deadline":
						return nil, context.DeadlineExceeded
					case "helper":
						return []byte(`{"error":{"code":"backend_error","message":"helper failed"}}`), nil
					case "malformed":
						return []byte(`not JSON`), nil
					case "missing-field":
						return []byte(`{"exec":{"exit_code":0}}`), nil
					case "extra-response":
						return []byte(`{} {}`), nil
					case "oversized":
						zero := 0
						return json.Marshal(control.Response{Exec: &control.ExecResult{ExitCode: &zero, Stdout: strings.Repeat("x", control.StreamLimit+1)}})
					}
					panic("unknown failure")
				}
				var err error
				switch operation {
				case "exec":
					_, err = m.Exec(ctx, w.ID, ExecOptions{Argv: []string{"printf", "hello"}})
				case "read_text":
					_, err = m.ReadText(ctx, w.ID, "file")
				case "write_text":
					_, err = m.WriteText(ctx, w.ID, "file", "hello")
				}
				if err == nil || f.leftovers || m.entries[w.ID].blocked || f.counts["stop"] != 1 {
					t.Fatalf("failure cleanup: %v, calls=%v", err, f.calls)
				}
				for i, call := range f.calls {
					if call[0] == "stop" && f.contexts[i] != nil {
						t.Fatal("reset inherited cancellation")
					}
				}
				f.fn = nil
				if _, err := m.Exec(context.Background(), w.ID, ExecOptions{Argv: []string{"printf", "hello"}}); err != nil {
					t.Fatal("successful cleanup did not preserve usability", err)
				}
			})
		}
	}
}

func TestTimedOutResultRequiresSuccessfulReset(t *testing.T) {
	m, f, w := operationWorkspace(t)
	f.fn = func(context.Context, control.Request) ([]byte, error) {
		return json.Marshal(control.Response{Exec: &control.ExecResult{TimedOut: true}})
	}
	r, err := m.Exec(context.Background(), w.ID, ExecOptions{Argv: []string{"sleep", "30"}, TimeoutSeconds: 1})
	if err != nil || !r.TimedOut || f.leftovers {
		t.Fatalf("timeout: %+v, %v", r, err)
	}
}

func TestFailedResetBlocksUntilDestroyAcrossRestart(t *testing.T) {
	for _, stage := range []string{"stop", "stopped-inspect", "start", "running-inspect", "still-running", "not-running"} {
		t.Run(stage, func(t *testing.T) {
			m, f, w := operationWorkspace(t)
			f.fail = func(_ context.Context, key string, count int) error {
				if key == "stop" && stage == "stop" || key == "start" && stage == "start" || key == "container inspect" && ((count == 3 && stage == "stopped-inspect") || (count == 4 && stage == "running-inspect")) {
					return errors.New("reset unavailable")
				}
				if key == "container inspect" && ((count == 3 && stage == "still-running") || (count == 4 && stage == "not-running")) {
					for _, c := range f.containers {
						c.State.Running = stage == "still-running"
					}
				}
				return nil
			}
			_, err := m.Exec(context.Background(), w.ID, ExecOptions{Argv: []string{"printf", "hello"}})
			if !errors.Is(err, ErrBlocked) || !m.entries[w.ID].blocked || !f.blocked[w.ID] {
				t.Fatalf("unsafe success: %v", err)
			}
			calls := f.opCalls
			for _, op := range []func() error{
				func() error {
					_, err := m.Exec(context.Background(), w.ID, ExecOptions{Argv: []string{"true"}})
					return err
				},
				func() error { _, err := m.ReadText(context.Background(), w.ID, "file"); return err },
				func() error { _, err := m.WriteText(context.Background(), w.ID, "file", "x"); return err },
			} {
				if !errors.Is(op(), ErrBlocked) {
					t.Fatal("blocked workspace accepted operation")
				}
			}
			if f.opCalls != calls {
				t.Fatal("blocked workspace executed helper")
			}
			f.fail = nil
			// Repair the test's injected State mismatch only, without running an
			// operation on the blocked workspace. The daemon never repairs it.
			for _, c := range f.containers {
				if c.State.Running {
					c.State.Status = "running"
				} else {
					c.State.Status = "exited"
				}
			}
			restarted, err := New(context.Background(), f)
			if err != nil {
				t.Fatal(err)
			}
			if !restarted.entries[w.ID].blocked {
				t.Fatal("daemon restart cleared blocked state")
			}
			if err := restarted.Destroy(context.Background(), w.ID); err != nil {
				t.Fatal(err)
			}
			if len(f.containers) != 0 || len(f.volumes) != 0 || f.blocked[w.ID] {
				t.Fatal("destroy did not remove blocked workspace/data")
			}
		})
	}
}

func TestLockIncludesCancelledFileCleanup(t *testing.T) {
	m, f, w := operationWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, cleaning, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.fn = func(ctx context.Context, _ control.Request) ([]byte, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.fail = func(_ context.Context, key string, _ int) error {
		if key == "stop" {
			close(cleaning)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { _, err := m.ReadText(ctx, w.ID, "file"); done <- err }()
	<-entered
	cancel()
	<-cleaning
	if m.entries[w.ID].op.TryLock() {
		m.entries[w.ID].op.Unlock()
		t.Fatal("lock released before failure cleanup")
	}
	// Destruction also waits behind the same operation lock.
	destroyed := make(chan error, 1)
	go func() { destroyed <- m.Destroy(context.Background(), w.ID) }()
	select {
	case err := <-destroyed:
		t.Fatalf("destroy overtook cleanup: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-destroyed; err != nil {
		t.Fatal(err)
	}
	if f.leftovers {
		t.Fatal("left process active when lock was released")
	}
}

func TestRestartResetsOldProcessesOrBlocks(t *testing.T) {
	m, f, w := operationWorkspace(t)
	f.leftovers = true
	restarted, err := New(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if f.leftovers || restarted.entries[w.ID].blocked {
		t.Fatal("recovery trusted previous daemon's execution state")
	}
	f.fail = func(_ context.Context, key string, _ int) error {
		if key == "stop" {
			return errors.New("unavailable")
		}
		return nil
	}
	restarted, err = New(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.entries[w.ID].blocked {
		t.Fatal("failed startup cleanup accepted workspace")
	}
	_ = m
}

func TestBoundedTransportWriter(t *testing.T) {
	b := &limitedBuffer{limit: 4}
	if n, err := b.Write([]byte("123456")); n != 6 || err != nil {
		t.Fatal("Writer contract violated")
	}
	b.Write([]byte("789"))
	if b.String() != "1234" || !b.overflow {
		t.Fatal("transport not bounded")
	}
}
