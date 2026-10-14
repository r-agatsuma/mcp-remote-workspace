//go:build linux

package workspace

import (
	"bytes"
	"context"
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

type logBuilder struct {
	*fakeBuilder
	build func(context.Context, Exact, string, io.Writer, io.Writer) BackendResult
}

func (f *logBuilder) Build(ctx context.Context, e Exact, _ BuildPlan, path string, out, diagnostic io.Writer) BackendResult {
	return f.build(ctx, e, path, out, diagnostic)
}

func assertLogFile(t *testing.T, path string, size int64) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != size || !ownedByService(info) {
		t.Fatalf("unexpected log state: %s mode=%v size=%d", path, info.Mode(), info.Size())
	}
}

func TestBuildLogHardBoundAndConstantMemory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := t.TempDir()
	logs, err := prepareBuildLogs(path, cancel)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.finish()
	chunk := bytes.Repeat([]byte("a"), 4096)
	for _, log := range []*buildLog{logs.stdout, logs.stderr} {
		assertLogFile(t, log.file.Name(), 0)
		// Cross the limit on a write boundary rather than relying on alignment.
		if _, err := log.Write([]byte("prefix")); err != nil {
			t.Fatal(err)
		}
		for range buildLogBytes/len(chunk) + 2 {
			if n, err := log.Write(chunk); err != nil || n != len(chunk) {
				t.Fatalf("stream was not consumed: %d %v", n, err)
			}
		}
		assertLogFile(t, log.file.Name(), buildLogBytes)
		prefix, err := os.ReadFile(log.file.Name())
		if err != nil || !bytes.HasPrefix(prefix, []byte("prefix")) {
			t.Fatalf("missing retained prefix: %v", err)
		}
		// More than 100 MiB of additional output uses the same input and writer.
		// Any per-output allocation or retained growing buffer fails this check.
		allocations := testing.AllocsPerRun(4, func() {
			for range 8192 {
				n, err := log.Write(chunk)
				if n != len(chunk) || err != nil {
					panic("saturated log stopped consuming")
				}
			}
		})
		if allocations != 0 {
			t.Fatalf("saturated drain allocated %v objects", allocations)
		}
		assertLogFile(t, log.file.Name(), buildLogBytes)
		got, err := os.ReadFile(log.file.Name())
		if err != nil || !bytes.Equal(got, prefix) {
			t.Fatal("saturation changed the retained prefix", err)
		}
	}
	if ctx.Err() != nil {
		t.Fatal("retention saturation canceled the build")
	}
	if err := logs.finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.stdout.Write(chunk); !errors.Is(err, os.ErrClosed) {
		t.Fatal("terminal writer remained usable", err)
	}
}

func TestBuildLogPreparationFailurePreventsSubmission(t *testing.T) {
	for _, name := range []string{"stdout", "stderr"} {
		for _, kind := range []string{"file", "directory", "symlink"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				f := newBuilder()
				s, _, _ := openBuilder(t, f, nil)
				sentinel := filepath.Join(t.TempDir(), "unrelated")
				if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
				s.prepareLogs = func(path string, cancel context.CancelFunc) (*buildLogs, error) {
					var err error
					switch kind {
					case "file":
						err = os.WriteFile(filepath.Join(path, name), []byte("existing"), 0600)
					case "directory":
						err = os.Mkdir(filepath.Join(path, name), 0700)
					case "symlink":
						err = os.Symlink(sentinel, filepath.Join(path, name))
					}
					if err != nil {
						t.Fatal(err)
					}
					return prepareBuildLogs(path, cancel)
				}
				i := identity(t, "s", "log-prepare")
				r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
				if err == nil || r.Status != Failed {
					t.Fatalf("log preparation failure accepted: %v %v", r, err)
				}
				if f.builds != 0 || f.creates != 0 || len(s.PendingBackend()) != 0 {
					t.Fatal("failed log preparation submitted backend work")
				}
				if kind == "symlink" {
					if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
						t.Fatal("ambiguous log symlink authorized cleanup", err)
					}
					assertState(t, s, i, Failed)
					// Model trusted repair of the deliberately injected ambiguity.
					if err := os.Remove(filepath.Join(s.buildPath(r.Exact), name)); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.Destroy(context.Background(), i); err != nil {
					t.Fatal(err)
				}
				assertState(t, s, i, Absent)
				if _, err := os.Lstat(s.buildPath(r.Exact)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("partial log preparation survived ABSENT", err)
				}
				if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
					t.Fatal("unrelated log target changed", err)
				}
			})
		}
	}
}

type countedLogWriter struct {
	io.Writer
	bytes, largest int
}

func (w *countedLogWriter) Write(p []byte) (int, error) {
	w.bytes += len(p)
	w.largest = max(w.largest, len(p))
	return w.Writer.Write(p)
}

func logScript(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "log-stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+contents), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildLogFilesPrecedeSubmissionAndDrainPastCapacity(t *testing.T) {
	script := logScript(t, "head -c 8388608 /dev/zero\nhead -c 8388608 /dev/zero >&2\n")
	finished, release := make(chan BackendResult, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	f := &logBuilder{fakeBuilder: newBuilder()}
	f.build = func(ctx context.Context, _ Exact, path string, out, diagnostic io.Writer) BackendResult {
		for name, writer := range map[string]io.Writer{"stdout": out, "stderr": diagnostic} {
			log, ok := writer.(*buildLog)
			if !ok || log.closed || log.file.Name() != filepath.Join(path, name) {
				t.Error("build received an unprepared log")
				return BackendResult{DefinitelyCompleted, injected}
			}
			info, err := log.file.Stat()
			if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
				t.Error("log descriptor not open before submission", err)
				return BackendResult{DefinitelyCompleted, injected}
			}
		}
		outCount, errCount := &countedLogWriter{Writer: out}, &countedLogWriter{Writer: diagnostic}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		result := (localCLI{script}).runBuild(ctx, "FROM scratch", filepath.Join(path, "tmp"), outCount, errCount, "build")
		for _, counter := range []*countedLogWriter{outCount, errCount} {
			if counter.bytes != 8<<20 || counter.largest > 32<<10 {
				t.Errorf("pipe consumed=%d largest buffer=%d", counter.bytes, counter.largest)
			}
		}
		finished <- result
		<-release
		return result
	}
	s, _, _ := openBuilder(t, f, nil)
	defer unblock()
	i := identity(t, "s", "streaming")
	r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-finished:
		if result.Err != nil || result.Completion != DefinitelyCompleted {
			t.Fatal(result)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("output beyond retention capacity blocked the subprocess")
	}
	assertState(t, s, i, Processing)
	// Data is already present while the build still owns its backend permit.
	for _, name := range []string{"stdout", "stderr"} {
		assertLogFile(t, filepath.Join(s.buildPath(r.Exact), name), buildLogBytes)
	}
	if pending := s.PendingBackend(); len(pending) != 1 || pending[0].Kind != "build" {
		t.Fatal("streaming changed admission", pending)
	}
	unblock()
	waitBuildState(t, s, i, Present)
	if err := s.Destroy(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(s.buildPath(r.Exact)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("logs survived ABSENT", err)
	}
}

type gatedLogWriter struct {
	io.Writer
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *gatedLogWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return w.Writer.Write(p)
}

func waitLogChildExit(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(marker)
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil && parseErr == nil && pid > 0 {
			if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); errors.Is(err, os.ErrNotExist) {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stub subprocess did not exit")
}

func TestBuildPublicationAndCleanupWaitForDrain(t *testing.T) {
	for _, destroy := range []bool{false, true} {
		t.Run(strconv.FormatBool(destroy), func(t *testing.T) {
			// The subprocess exits independently of the service-side drain gate.
			marker := filepath.Join(t.TempDir(), "exited-pid")
			script := logScript(t, "printf diagnostic\nprintf '%s' \"$$\" > \"$3\"\n")
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			f := &logBuilder{fakeBuilder: newBuilder()}
			f.build = func(ctx context.Context, _ Exact, path string, out, diagnostic io.Writer) BackendResult {
				return (localCLI{script}).runBuild(ctx, "", filepath.Join(path, "tmp"), &gatedLogWriter{Writer: out, started: started, release: release}, diagnostic, "build", marker)
			}
			s, _, _ := openBuilder(t, f, nil)
			defer unblock()
			i := identity(t, "s", "drain-gate")
			r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
			if err != nil {
				t.Fatal(err)
			}
			waitStorageSignal(t, started)
			waitLogChildExit(t, marker)
			assertState(t, s, i, Processing)
			if pending := s.PendingBackend(); len(pending) != 1 || pending[0].Kind != "build" {
				t.Fatal("process exit released the unfinished drain", pending)
			}
			if destroy {
				if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal(err)
				}
				// Even trusted backend recovery cannot erase a live local drain.
				if err := s.RecoverBackend(s.PendingBackend()[0], DefinitelyCompleted); err != nil {
					t.Fatal(err)
				}
				if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal("draining generation lost cleanup ownership", err)
				}
				assertLogFile(t, filepath.Join(s.buildPath(r.Exact), "stdout"), 0)
				unblock()
				waitBuildState(t, s, i, Absent)
				if _, err := os.Stat(s.buildPath(r.Exact)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("drain log state survived ABSENT", err)
				}
			} else {
				unblock()
				waitBuildState(t, s, i, Present)
				data, err := os.ReadFile(filepath.Join(s.buildPath(r.Exact), "stdout"))
				if err != nil || string(data) != "diagnostic" {
					t.Fatal("publication preceded retained Data", err)
				}
			}
		})
	}
}

func TestBuildLogFailuresFencePublication(t *testing.T) {
	for _, mode := range []string{"live-write", "ignored-write", "terminal-sync", "panic"} {
		t.Run(mode, func(t *testing.T) {
			// An active local process exercises cancellation on a service write
			// failure. No engine, network or surviving descendant is involved.
			script := logScript(t, "while :; do printf diagnostic; done\n")
			f := &logBuilder{fakeBuilder: newBuilder()}
			var outLog, errLog *buildLog
			f.build = func(ctx context.Context, _ Exact, path string, out, diagnostic io.Writer) BackendResult {
				outLog, errLog = out.(*buildLog), diagnostic.(*buildLog)
				_, _ = diagnostic.Write([]byte("retained failure diagnostics"))
				if mode == "panic" {
					panic("unknown build completion")
				}
				if mode == "terminal-sync" {
					_, _ = out.Write([]byte("output"))
				}
				// Inject an actual descriptor failure, not a fake successful write.
				if err := outLog.file.Close(); err != nil {
					t.Error(err)
				}
				if mode == "live-write" {
					ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
					defer cancel()
					result := (localCLI{script}).runBuild(ctx, "", filepath.Join(path, "tmp"), out, diagnostic, "build")
					if result.Completion != OutcomeUnknown || !errors.Is(result.Err, ErrOutcomeUnknown) || !errors.Is(ctx.Err(), context.Canceled) {
						t.Errorf("live write failure did not cancel: %v %v", result, ctx.Err())
					}
					return result
				}
				if mode == "ignored-write" {
					_, _ = out.Write([]byte("lost output"))
					if !errors.Is(ctx.Err(), context.Canceled) {
						t.Error("write failure left build context live")
					}
				}
				// Definite engine completion cannot hide a failed required log path.
				return BackendResult{Completion: DefinitelyCompleted}
			}
			s, _, _ := openBuilder(t, f, nil)
			i := identity(t, "s", "log-failure")
			r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
			if err != nil {
				t.Fatal(err)
			}
			waitBuildState(t, s, i, Failed)
			s.buildWG.Wait()
			if f.creates != 0 || !outLog.closed || !errLog.closed {
				t.Fatal("failed logging published or retained live descriptors")
			}
			data, err := os.ReadFile(filepath.Join(s.buildPath(r.Exact), "stderr"))
			if err != nil || string(data) != "retained failure diagnostics" {
				t.Fatal("bounded FAILED diagnostics disappeared", err)
			}
			pending := s.PendingBackend()
			unknown := mode == "live-write" || mode == "panic"
			if (len(pending) == 1) != unknown {
				t.Fatalf("unexpected completion accounting: %v", pending)
			}
			if unknown {
				if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal("unknown work cleaned up without recovery", err)
				}
				if err := s.RecoverBackend(pending[0], DefinitelyCompleted); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Destroy(context.Background(), i); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(s.buildPath(r.Exact)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed log state survived ABSENT", err)
			}
		})
	}
}

func TestBuildLogsCancellationRestartAndStaleWriters(t *testing.T) {
	for _, mode := range []string{"destroy", "ttl", "recovery", "restart"} {
		t.Run(mode, func(t *testing.T) {
			started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			f := &logBuilder{fakeBuilder: newBuilder()}
			var oldOut, oldErr io.Writer
			calls := 0
			f.build = func(ctx context.Context, _ Exact, _ string, out, diagnostic io.Writer) BackendResult {
				calls++
				if calls > 1 {
					_, _ = out.Write([]byte("new generation"))
					_, _ = diagnostic.Write([]byte("new generation"))
					return BackendResult{Completion: DefinitelyCompleted}
				}
				oldOut, oldErr = out, diagnostic
				_, _ = out.Write([]byte("before"))
				_, _ = diagnostic.Write([]byte("before"))
				close(started)
				<-ctx.Done()
				close(canceled)
				<-release
				// Deliberately late output after authority has been revoked must
				// remain bound to the original descriptors and cleanup ownership.
				_, _ = out.Write([]byte(" late"))
				_, _ = diagnostic.Write([]byte(" late"))
				if mode == "restart" {
					return BackendResult{OutcomeUnknown, ErrOutcomeUnknown}
				}
				return BackendResult{Completion: DefinitelyCompleted}
			}
			s, cfg, lock := openBuilder(t, f, nil)
			defer unblock()
			i := identity(t, "s", "late-log")
			r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
			if err != nil {
				t.Fatal(err)
			}
			waitStorageSignal(t, started)
			for _, name := range []string{"stdout", "stderr"} {
				assertLogFile(t, filepath.Join(s.buildPath(r.Exact), name), int64(len("before")))
			}
			var closed chan error
			switch mode {
			case "destroy":
				if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal(err)
				}
			case "ttl":
				s.mu.Lock()
				now := s.lifetimes[r.Exact].Created.Add(s.policy.AbsoluteTTL)
				s.now = func() time.Time { return now }
				s.mu.Unlock()
				if err := s.CleanupExpired(context.Background(), r.Exact); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal(err)
				}
			case "recovery":
				if err := s.RecoverBackend(s.PendingBackend()[0], DefinitelyCompleted); err != nil {
					t.Fatal(err)
				}
				if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal(err)
				}
			case "restart":
				closed = make(chan error, 1)
				go func() { closed <- s.Close() }()
			}
			waitStorageSignal(t, canceled)
			if _, err := os.Stat(s.buildPath(r.Exact)); err != nil {
				t.Fatal("live log ownership was lost", err)
			}
			unblock()
			if mode == "restart" {
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
				s, err = open(context.Background(), cfg, func() Runtime { return f }, lock)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				assertState(t, s, i, Failed)
				for _, name := range []string{"stdout", "stderr"} {
					data, err := os.ReadFile(filepath.Join(s.buildPath(r.Exact), name))
					if err != nil || string(data) != "before late" {
						t.Fatal("restart lost already retained output", err)
					}
				}
				for _, op := range s.PendingBackend() {
					if err := s.RecoverBackend(op, DefinitelyCompleted); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.Destroy(context.Background(), i); err != nil {
					t.Fatal(err)
				}
			}
			waitBuildState(t, s, i, Absent)
			if _, err := os.Stat(s.buildPath(r.Exact)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("old logs survived ABSENT", err)
			}
			next, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
			if err != nil || next.Exact == r.Exact {
				t.Fatal("fresh generation missing", err)
			}
			waitBuildState(t, s, i, Present)
			for _, writer := range []io.Writer{oldOut, oldErr} {
				if _, err := writer.Write([]byte("stale overwrite")); !errors.Is(err, os.ErrClosed) {
					t.Fatal("old log writer retained authority", err)
				}
			}
			for _, name := range []string{"stdout", "stderr"} {
				data, err := os.ReadFile(filepath.Join(s.buildPath(next.Exact), name))
				if err != nil || string(data) != "new generation" {
					t.Fatal("late output changed later generation", err)
				}
			}
			if calls != 2 || f.creates != 1 {
				t.Fatal("revoked or restarted build published/replayed")
			}
		})
	}
}
