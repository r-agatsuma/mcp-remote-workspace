//go:build linux

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func diagnosticLogs(t *testing.T, directory string) (*buildLog, *buildLog) {
	t.Helper()
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	open := func(name string) *buildLog {
		f, err := os.OpenFile(filepath.Join(directory, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		return &buildLog{file: f}
	}
	return open("stdout.log"), open("stderr.log")
}

func TestBuildSeparateStreamsDrainAndRetainAggregateBound(t *testing.T) {
	for _, failure := range []string{"none", "stdout", "stderr"} {
		t.Run(failure, func(t *testing.T) {
			directory := t.TempDir()
			out, diagnostic := diagnosticLogs(t, directory)
			if failure == "stdout" {
				out.file.Close()
			}
			if failure == "stderr" {
				diagnostic.file.Close()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Both actual OS pipes carry 8 MiB. Concurrent writers exercise independent
			// drains, including when one retained-file writer has failed.
			command := hostCommand(ctx, "/bin/sh", "-c", "(printf stdout-prefix; head -c 8388608 /dev/zero) & (printf stderr-prefix; head -c 8388608 /dev/zero) >&2 & wait")
			command.Stdout, command.Stderr = out, diagnostic
			if err := command.Run(); err != nil {
				t.Fatal("drain stalled", err)
			}
			if out.total != 8388608+13 || diagnostic.total != 8388608+13 {
				t.Fatal("output stopped draining", out.total, diagnostic.total)
			}
			e := Exact{WorkspaceKey(strings.Repeat("a", 64)), Generation(strings.Repeat("b", 64))}
			code := 7
			err := finishBuildLogs(directory, e, out, diagnostic, &code)
			if (err != nil) != (failure != "none") {
				t.Fatal("write failure hidden", err)
			}
			var total int64
			for _, name := range []string{"stdout.log", "stderr.log"} {
				info, err := os.Stat(filepath.Join(directory, name))
				if err != nil {
					t.Fatal(err)
				}
				if info.Size() > retainedBuildBytes {
					t.Fatal("unbounded file", info.Size())
				}
				total += info.Size()
				if !strings.HasPrefix(name, failure) {
					data, err := os.ReadFile(filepath.Join(directory, name))
					if err != nil || !strings.HasPrefix(string(data), strings.TrimSuffix(name, ".log")+"-prefix") {
						t.Fatal("stream crossed or lost", err)
					}
				}
			}
			if total > 1<<20 || out.retained > retainedBuildBytes || diagnostic.retained > retainedBuildBytes {
				t.Fatal("aggregate bound exceeded")
			}
		})
	}
}

type diagnosticRuntime struct {
	*asyncRuntime
	t            *testing.T
	cleanupCheck func(Exact)
}

func (r *diagnosticRuntime) Build(ctx context.Context, e Exact, text, data string) (string, error) {
	out, diagnostic := diagnosticLogs(r.t, buildDirectory(data, e))
	out.Write([]byte("stdout for " + string(e.Generation) + "\n" + data + "/private-file\n" + strings.Repeat("x", retainedBuildBytes*2)))
	diagnostic.Write([]byte("stderr for " + string(e.Generation) + "\nfailed instruction\nopen \"/host-storage-outside-data/engine/private\": permission denied\n--runroot=/run/user/123/containers\n"))
	code := 17
	if err := finishBuildLogs(buildDirectory(data, e), e, out, diagnostic, &code); err != nil {
		r.t.Fatal(err)
	}
	return r.asyncRuntime.Build(ctx, e, text, data)
}
func (r *diagnosticRuntime) CleanupBuild(ctx context.Context, e Exact, data, image string) error {
	if r.cleanupCheck != nil {
		r.cleanupCheck(e)
	}
	if err := r.asyncRuntime.CleanupBuild(ctx, e, data, image); err != nil {
		return err
	}
	return removeBuildDirectory(data, e)
}

func TestBuildFailureDiagnosticObservationRestartCleanupAndFreshGeneration(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleanup", true: "uncertain-cleanup"}[cleanupFails], func(t *testing.T) {
			async := newAsyncRuntime()
			s, cfg, lock, i := openAsync(t, async)
			runtime := &diagnosticRuntime{asyncRuntime: async, t: t}
			s.runtime = runtime
			text := "FROM busybox"
			first, err := s.CreateWorkspace(context.Background(), i, &text)
			if err != nil {
				t.Fatal(err)
			}
			receiveExact(t, async.started)
			async.finish <- CompletedBackendError(injected)
			awaitWorker(t, s)
			if s.lifetimes[first.Exact].Build.Phase != buildFailed || len(s.PendingBackend()) != 0 {
				t.Fatal("terminal failure did not release only backend work")
			}
			s.Close()
			s, err = open(context.Background(), cfg, func() Runtime { return runtime }, lock)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if cleanupFails {
				runtime.cleanupErr = injected
			}
			runtime.cleanupCheck = func(e Exact) {
				if e != first.Exact {
					t.Fatal("wrong cleanup")
				}
				if _, err := os.Stat(filepath.Join(buildDirectory(s.dataDir, e), "stderr.log")); err != nil {
					t.Fatal("diagnostics removed before observation", err)
				}
			}
			corrected := "FROM alpine"
			observed, err := s.CreateWorkspace(context.Background(), i, &corrected)
			tool, ok := mapToolError(err).(*ToolError)
			if !ok || tool.Code != "build_failed" || tool.BuildDiagnostic == nil || observed.Exact != first.Exact {
				t.Fatal("missing exact diagnostic", observed, err)
			}
			d := tool.BuildDiagnostic
			if !d.Available || d.ExitStatus == nil || *d.ExitStatus != 17 || !strings.Contains(d.Stdout.Text, "stdout for "+string(first.Exact.Generation)) || !strings.Contains(d.Stderr.Text, "stderr for "+string(first.Exact.Generation)) {
				t.Fatal("stream ownership lost", d)
			}
			if strings.Contains(d.Stdout.Text, "stderr for") || strings.Contains(d.Stderr.Text, "stdout for") || strings.Contains(d.Stdout.Text, s.dataDir) || len(d.Stdout.Text) > MaxStreamBytes || !d.Stdout.Truncated {
				t.Fatal("unsafe diagnostic representation")
			}
			if strings.Contains(d.Stderr.Text, "/host-storage-outside-data") || strings.Contains(d.Stderr.Text, "/run/user/") || !strings.Contains(d.Stderr.Text, "permission denied") {
				t.Fatal("host path leaked or useful failure lost")
			}
			encoded, _ := json.Marshal(tool)
			if strings.Contains(string(encoded), "private-human-identity") || strings.Contains(string(encoded), injected.Error()) {
				t.Fatal("raw identity/error leaked")
			}
			if runtime.builds != 1 || runtime.text != text {
				t.Fatal("observation installed replacement")
			}
			if cleanupFails {
				if !tool.OutcomeUnknown || len(s.entries) != 1 || len(s.PendingBackend()) != 1 {
					t.Fatal("uncertain cleanup released ownership")
				}
				if _, err := os.Stat(filepath.Join(buildDirectory(s.dataDir, first.Exact), "stderr.log")); err != nil {
					t.Fatal("uncertain cleanup deleted diagnostic", err)
				}
				if _, err := s.CreateWorkspace(context.Background(), i, &corrected); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal("uncertain cleanup accepted replacement", err)
				}
				return
			}
			if len(s.entries) != 0 {
				t.Fatal("cleanup failed to establish absence")
			}
			if _, err := os.Stat(buildDirectory(s.dataDir, first.Exact)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("exact diagnostics remain", err)
			}
			runtime.cleanupCheck = nil
			second, err := s.CreateWorkspace(context.Background(), i, &corrected)
			if err != nil || second.Exact == first.Exact {
				t.Fatal(second, err)
			}
			receiveExact(t, async.started)
			async.finish <- CompletedBackendError(injected)
			awaitWorker(t, s)
			_, err = s.CreateWorkspace(context.Background(), i, nil)
			next := mapToolError(err).(*ToolError).BuildDiagnostic
			if strings.Contains(next.Stdout.Text, string(first.Exact.Generation)) || !strings.Contains(next.Stdout.Text, string(second.Exact.Generation)) {
				t.Fatal("stale diagnostic crossed generation")
			}
		})
	}
}

func TestBuildDiagnosticRejectsStaleMetadataSymlinksAndOversizedFiles(t *testing.T) {
	for _, mode := range []string{"metadata", "directory-symlink", "file-symlink", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			async := newAsyncRuntime()
			s, _, _, i := openAsync(t, async)
			runtime := &diagnosticRuntime{asyncRuntime: async, t: t}
			s.runtime = runtime
			text := "FROM busybox"
			first, err := s.CreateWorkspace(context.Background(), i, &text)
			if err != nil {
				t.Fatal(err)
			}
			receiveExact(t, async.started)
			async.finish <- CompletedBackendError(injected)
			awaitWorker(t, s)
			directory := buildDirectory(s.dataDir, first.Exact)
			switch mode {
			case "metadata":
				wrong := first.Exact
				wrong.Generation = Generation(strings.Repeat("f", 64))
				data, _ := json.Marshal(buildLogResult{Exact: wrong})
				os.WriteFile(filepath.Join(directory, "result.json"), data, 0600)
			case "directory-symlink":
				if err := os.Rename(directory, directory+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(directory+"-old", directory); err != nil {
					t.Fatal(err)
				}
			case "file-symlink":
				if err := os.Remove(filepath.Join(directory, "stdout.log")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("stderr.log", filepath.Join(directory, "stdout.log")); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(filepath.Join(directory, "stdout.log"), []byte(strings.Repeat("x", retainedBuildBytes+1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			s.mu.Lock()
			d := s.buildDiagnostic(context.Background(), first.Exact)
			s.mu.Unlock()
			if d.Stdout.Text != "" {
				t.Fatal("unverified log returned")
			}
			if mode == "metadata" || mode == "directory-symlink" {
				if d.Available {
					t.Fatal("stale diagnostic accepted")
				}
			}
		})
	}
}

func TestUnknownBuildRebootRecoveryDoesNotInventFailureOrReplay(t *testing.T) {
	async := newAsyncRuntime()
	s, cfg, lock, i := openAsync(t, async)
	s.backend.bootID = previousBoot
	text := "FROM busybox"
	first, err := s.CreateWorkspace(context.Background(), i, &text)
	if err != nil {
		t.Fatal(err)
	}
	receiveExact(t, async.started)
	async.finish <- ErrOutcomeUnknown
	awaitWorker(t, s)
	s.Close()
	r, err := open(context.Background(), cfg, func() Runtime { return async }, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.bootID = func() (string, error) { return currentBoot, nil }
	for _, op := range r.PendingBackend() {
		if err := r.RecoverBackendAfterReboot(context.Background(), op); err != nil {
			t.Fatal(err)
		}
	}
	result, err := r.CreateWorkspace(context.Background(), i, &text)
	if !errors.Is(err, ErrOutcomeUnknown) || result.Exact != first.Exact || async.builds != 1 {
		t.Fatal("reboot invented failure or replay", result, err)
	}
	if err := r.DestroyGeneration(context.Background(), first.Exact); err != nil {
		t.Fatal(err)
	}
	if len(r.entries) != 0 {
		t.Fatal("cleanup did not establish absence")
	}
}

func TestFailureObservationSerializesTTLAndInitiatorCancellation(t *testing.T) {
	async := newAsyncRuntime()
	s, _, _, i := openAsync(t, async)
	runtime := &diagnosticRuntime{asyncRuntime: async, t: t}
	s.runtime = runtime
	ctx, cancel := context.WithCancel(context.Background())
	text := "FROM busybox"
	first, err := s.CreateWorkspace(ctx, i, &text)
	if err != nil {
		t.Fatal(err)
	}
	receiveExact(t, async.started)
	cancel()
	async.finish <- CompletedBackendError(injected)
	awaitWorker(t, s)
	if len(s.entries) != 1 || s.lifetimes[first.Exact].Build.Phase != buildFailed || async.cleanups != 0 {
		t.Fatal("caller disconnect discarded failure")
	}
	started, release := make(chan struct{}), make(chan struct{})
	runtime.cleanupCheck = func(e Exact) { close(started); <-release }
	observed := make(chan error, 1)
	go func() { _, err := s.CreateWorkspace(context.Background(), i, nil); observed <- err }()
	<-started
	ttl := make(chan error, 1)
	go func() { ttl <- s.CleanupExpired(context.Background(), first.Exact) }()
	close(release)
	if err := <-observed; !errors.Is(err, ErrBuildFailed) {
		t.Fatal(err)
	}
	if err := <-ttl; !errors.Is(err, ErrStaleGeneration) {
		t.Fatal("TTL bypassed observation serialization", err)
	}
	if async.cleanups != 1 || async.builds != 1 {
		t.Fatal("cleanup/build replayed")
	}
}

func TestBuildLostTransportOrCanceledResultRemainsUnknown(t *testing.T) {
	for _, mode := range []string{"canceled", "deadline", "lost-drain"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var err error
			switch mode {
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "deadline":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			case "lost-drain":
				err = exec.ErrWaitDelay
			}
			result := buildCommandError(ctx, err)
			if completionOf(CompletedBackendError(result)) != OutcomeUnknown {
				t.Fatal("parent stop invented terminal failure", result)
			}
		})
	}
}

func TestFinalizationLostDrainIsNotTerminalEvidence(t *testing.T) {
	err := localRuntimeError(context.Background(), exec.ErrWaitDelay)
	if !errors.Is(err, ErrOutcomeUnknown) || completionOf(err) != OutcomeUnknown {
		t.Fatal("lost finalization transport became definite failure", err)
	}
}
