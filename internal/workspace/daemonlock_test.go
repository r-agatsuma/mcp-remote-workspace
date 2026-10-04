package workspace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Exercise the production lock with a fake registry, without touching Podman.
type daemonLockedFake struct {
	*fakePodman
	dir string
}

func (f *daemonLockedFake) acquireDaemonLock() (int, error) {
	return (&podman{dir: f.dir}).acquireDaemonLock()
}

func TestDaemonLockOwnerProcess(t *testing.T) {
	dir := os.Getenv("MCP_DAEMON_LOCK_TEST_DIR")
	if dir == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := New(ctx, &daemonLockedFake{fakePodman: newFake(), dir: dir}); err != nil {
		os.Exit(1)
	}
	// Context/Manager lifetime must not end process exclusivity. In particular,
	// an os.File finalizer must not close a still-required lock descriptor.
	cancel()
	runtime.GC()
	runtime.GC()
	if _, err := io.WriteString(os.Stdout, "ready\n"); err != nil {
		os.Exit(2)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestDaemonExclusivityBeforeRecoveryAndReleaseOnExit(t *testing.T) {
	for _, shutdown := range []string{"exit", "crash"} {
		t.Run(shutdown, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "runtime")
			f := newFake()
			original := openFake(t, f)
			w, err := original.Create(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-test.run=^TestDaemonLockOwnerProcess$")
			cmd.Env = append(os.Environ(), "MCP_DAEMON_LOCK_TEST_DIR="+dir)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			ready := make([]byte, len("ready\n"))
			if _, err := io.ReadFull(stdout, ready); err != nil || string(ready) != "ready\n" {
				t.Fatalf("lock owner did not start: %q %v", ready, err)
			}
			calls := len(f.calls)
			contender := &daemonLockedFake{fakePodman: f, dir: dir}
			if _, err := New(context.Background(), contender); !errors.Is(err, ErrDaemonActive) {
				t.Fatalf("second Manager was not rejected: %v", err)
			}
			if len(f.calls) != calls || f.counts["stop"] != 0 {
				t.Fatal("second Manager touched Podman before acquiring exclusivity")
			}
			if shutdown == "exit" {
				if err := stdin.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			waited = true
			if shutdown == "exit" && err != nil {
				t.Fatalf("owner exit: %v, stderr: %s", err, &stderr)
			}
			if shutdown == "crash" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("owner did not crash: %v", err)
				}
			}
			// The OS releases the lock on both graceful exit and SIGKILL, even
			// though the lock file persists. Recovery can now reset the registry.
			recovered, err := New(context.Background(), contender)
			if err != nil {
				t.Fatal(err)
			}
			if e := recovered.entries[w.ID]; e == nil || !e.CreatedAt.Equal(w.CreatedAt) || f.counts["stop"] != 1 {
				t.Fatal("recovery did not run after acquiring released daemon lock")
			}
			// A second Manager in the same process must also be refused.
			calls = len(f.calls)
			if _, err := New(context.Background(), contender); !errors.Is(err, ErrDaemonActive) || len(f.calls) != calls {
				t.Fatalf("same-process contender began recovery: %v", err)
			}
		})
	}
}

func TestDaemonLockFailedStartupAndDescriptorInheritance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")
	f := &daemonLockedFake{fakePodman: newFake(), dir: dir}
	f.fail = func(context.Context, string, int) error { return errors.New("startup failed") }
	if _, err := New(context.Background(), f); err == nil {
		t.Fatal("failed startup returned a Manager")
	}
	fd, err := acquireDaemonLock(dir)
	if err != nil {
		t.Fatal("failed startup retained its lock", err)
	}
	defer unix.Close(fd)
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("daemon lock would be inherited by Podman: flags=%d err=%v", flags, err)
	}
}

func TestDaemonLockRejectsUnsafePaths(t *testing.T) {
	for _, kind := range []string{"symlink", "public-file", "fifo", "hardlink", "public-directory", "symlink-directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			lock := filepath.Join(dir, "daemon.lock")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(filepath.Join(dir, "other"), lock)
			case "public-file":
				err = os.WriteFile(lock, nil, 0600)
				if err == nil {
					err = os.Chmod(lock, 0644)
				}
			case "fifo":
				err = unix.Mkfifo(lock, 0600)
			case "hardlink":
				other := filepath.Join(dir, "other")
				err = os.WriteFile(other, nil, 0600)
				if err == nil {
					err = os.Link(other, lock)
				}
			case "public-directory":
				err = os.Chmod(dir, 0755)
			case "symlink-directory":
				link := filepath.Join(dir, "runtime")
				err = os.Symlink(t.TempDir(), link)
				dir = link
			}
			if err != nil {
				t.Fatal(err)
			}
			if fd, err := acquireDaemonLock(dir); err == nil {
				unix.Close(fd)
				t.Fatal("unsafe daemon lock path accepted")
			}
		})
	}
}
