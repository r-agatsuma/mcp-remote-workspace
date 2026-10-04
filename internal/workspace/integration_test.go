package workspace

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/r-agatsuma/mcp-remote-workspace/internal/control"
)

// Opt-in only: requires a dedicated prepared account and prebuilt fixed image.
// This test never installs dependencies, builds/pulls images, or edits host config.
func TestRootlessPodmanIntegration(t *testing.T) {
	if os.Getenv("MCP_WORKSPACE_INTEGRATION") != "1" {
		t.Skip("set MCP_WORKSPACE_INTEGRATION=1 on a prepared rootless Podman host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	p, err := newPodman()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyControlRuntime(controlHostPath); err != nil {
		t.Fatal(err)
	}
	m, err := New(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	w, err := m.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := m.Destroy(cleanupCtx, w.ID); err != nil {
			t.Errorf("integration cleanup: %v", err)
		}
	})
	id := m.entries[w.ID].container
	// Test-only execution probes actual runtime behavior; it is not an MCP exec
	// backend or a host shell. Resource files assert enforcement, not just flags.
	probe := `import os, pathlib
p = pathlib.Path('/workspace/probe.txt')
p.write_text('workspace writable\n')
assert p.read_text() == 'workspace writable\n'
status = dict(line.split(':', 1) for line in pathlib.Path('/proc/self/status').read_text().splitlines() if ':' in line)
assert int(status['CapEff'].strip(), 16) == 0
assert int(status['CapBnd'].strip(), 16) == 0
assert status['NoNewPrivs'].strip() == '1'
assert os.environ['HOSTNAME'] == 'mcp-workspace'
assert os.uname().nodename == 'mcp-workspace'
for name in ('NOTIFY_SOCKET', 'LISTEN_PID', 'LISTEN_FDS', 'LISTEN_FDNAMES', 'SSH_AUTH_SOCK', 'GITHUB_TOKEN', 'OPENAI_API_KEY', 'HTTP_PROXY', 'HTTPS_PROXY'):
    assert name not in os.environ, name
for name in ('/root/.ssh', '/root/.codex', '/root/.config/containers/auth.json', '/run/podman/podman.sock', '/var/run/docker.sock', '/dev/sda', '/dev/kvm', '/dev/fuse'):
    assert not pathlib.Path(name).exists(), name
assert pathlib.Path('/sys/fs/cgroup/memory.max').read_text().strip() == '2147483648'
assert pathlib.Path('/sys/fs/cgroup/memory.swap.max').read_text().strip() == '0'
assert pathlib.Path('/sys/fs/cgroup/pids.max').read_text().strip() == '256'
assert pathlib.Path('/sys/fs/cgroup/cpu.max').read_text().strip() == '200000 100000'
assert p.stat().st_uid == 0
`
	if _, err := p.Run(ctx, "exec", id, "/usr/bin/python3", "-c", probe); err != nil {
		t.Fatalf("runtime isolation probe: %v", err)
	}
	if _, err := p.Run(ctx, "exec", id, "/usr/bin/curl", "--fail", "--silent", "--show-error", "--max-time", "20", "https://deb.debian.org/debian/README"); err != nil {
		t.Fatalf("outbound Internet: %v", err)
	}
	t.Run("managed operations", func(t *testing.T) {
		program := "print('hello from workspace')\n"
		written, err := m.WriteText(ctx, w.ID, "./src//hello.py", program)
		if err != nil {
			t.Fatal(err)
		}
		read, err := m.ReadText(ctx, w.ID, "src/hello.py")
		if err != nil || read.Content != program || read.SHA256 != written.SHA256 {
			t.Fatalf("roundtrip: %+v %v", read, err)
		}
		r, err := m.Exec(ctx, w.ID, ExecOptions{Argv: []string{"python3", "src/hello.py"}})
		if err != nil || r.Stdout != "hello from workspace\n" {
			t.Fatalf("program: %+v %v", r, err)
		}
		r, err = m.Exec(ctx, w.ID, ExecOptions{Argv: []string{"printf", "hello"}})
		if err != nil || r.Stdout != "hello" {
			t.Fatalf("argv: %+v %v", r, err)
		}
		r, err = m.Exec(ctx, w.ID, ExecOptions{Argv: []string{"sleep", "30"}, TimeoutSeconds: 1})
		if err != nil || !r.TimedOut {
			t.Fatalf("timeout: %+v %v", r, err)
		}
		r, err = m.Exec(ctx, w.ID, ExecOptions{Argv: []string{"python3", "-c", "import sys;sys.stdout.write('x'*300000);sys.stderr.write('y'*300000)"}})
		if err != nil || !r.StdoutTruncated || !r.StderrTruncated || len(r.Stdout) != control.StreamLimit || len(r.Stderr) != control.StreamLimit {
			t.Fatalf("stream bounds: %v", err)
		}
		if _, err := m.Exec(ctx, w.ID, ExecOptions{Argv: []string{"python3", "-c", "import os;os.symlink('/etc', '/workspace/escape');open('/workspace/binary','wb').write(b'\\xff\\x00')"}}); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"../etc/passwd", "/etc/passwd", "escape/passwd"} {
			if _, err := m.ReadText(ctx, w.ID, path); err == nil {
				t.Fatal("escape read accepted", path)
			}
			if _, err := m.WriteText(ctx, w.ID, path, "overwrite"); err == nil {
				t.Fatal("escape write accepted", path)
			}
		}
		if _, err := m.ReadText(ctx, w.ID, "binary"); err == nil {
			t.Fatal("binary read accepted")
		}
	})
	t.Run("daemonized descendants and helper death", func(t *testing.T) {
		// setsid, double-fork and closed output descriptors must all be covered
		// by host cleanup. The second variant also kills the actual helper.
		for _, killHelper := range []bool{false, true} {
			code := `import os, time, signal
if os.fork() == 0:
    os.setsid()
    if os.fork() != 0: os._exit(0)
    for fd in (0,1,2): os.close(fd)
    while True: time.sleep(1)
time.sleep(0.1)
`
			if killHelper {
				code += "os.kill(os.getppid(), signal.SIGKILL)\n"
			}
			_, err := m.Exec(ctx, w.ID, ExecOptions{Argv: []string{"python3", "-c", code}})
			if killHelper && err == nil {
				t.Fatal("helper death returned success")
			}
			if !killHelper && err != nil {
				t.Fatal(err)
			}
			assertIdleContainer(t, ctx, p, id)
			if _, err := m.ReadText(ctx, w.ID, "src/hello.py"); err != nil {
				t.Fatal("cleanup lost workspace usability", err)
			}
		}
	})
	t.Run("cancelled file transport", func(t *testing.T) {
		fault := &cancelFilePodman{podman: p, container: id, entered: make(chan struct{})}
		m.runner = fault
		defer func() { m.runner = p }()
		callCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { _, err := m.ReadText(callCtx, w.ID, "src/hello.py"); done <- err }()
		select {
		case <-fault.entered:
			cancel()
		case <-ctx.Done():
			cancel()
			t.Fatal(ctx.Err())
		}
		if err := <-done; err == nil {
			t.Fatal("cancel returned success")
		}
		assertIdleContainer(t, ctx, p, id)
	})
	t.Run("mutable development runtime", func(t *testing.T) {
		code := `import os
os.rename('/usr/bin/python3', '/usr/bin/python3.saved')
open('/usr/bin/python3', 'w').write('#!/bin/sh\nexit 99\n')
os.chmod('/usr/bin/python3', 0o755)
open('/usr/lib/python3/dist-packages/sitecustomize.py', 'w').write('raise SystemExit(99)\n')
open('/usr/bin/sleep','w').write('#!/bin/sh\nexit 99\n')
open('/etc/mcp-rootfs-preserved','w').write('installed state')
`
		if _, err := m.Exec(ctx, w.ID, ExecOptions{Argv: []string{"python3", "-c", code}}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.WriteText(ctx, w.ID, "after-tamper", "still works"); err != nil {
			t.Fatal(err)
		}
		read, err := m.ReadText(ctx, w.ID, "after-tamper")
		if err != nil || read.Content != "still works" {
			t.Fatalf("trusted I/O compromised: %+v %v", read, err)
		}
		r, err := m.Exec(ctx, w.ID, ExecOptions{Argv: []string{"/bin/sh", "-c", "cat /etc/mcp-rootfs-preserved"}})
		if err != nil || r.Stdout != "installed state" {
			t.Fatalf("reset lost rootfs: %+v %v", r, err)
		}
		if m.entries[w.ID].container != id {
			t.Fatal("reset recreated container")
		}
	})
	if _, err := New(ctx, p); !errors.Is(err, ErrDaemonActive) {
		t.Fatalf("second manager accepted during daemon lifetime: %v", err)
	}
	if err := m.Destroy(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := m.discover(ctx, w.ID)
	if err != nil || len(ids) != 0 {
		t.Fatalf("destroy left compute: %v, %v", ids, err)
	}
	if err := m.Destroy(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Destroy(ctx, "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ID: %v", err)
	}
}

func assertIdleContainer(t *testing.T, ctx context.Context, p *podman, id string) {
	t.Helper()
	data, err := p.Run(ctx, "top", id, "pid", "args")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], controlContainerPath+" init") {
		t.Fatalf("unmanaged processes remain: %s", data)
	}
}

// Fault injection emulates a file helper/transport that leaves a daemonized
// process and then gets cancelled. Real Podman reset must clear the namespace.
type cancelFilePodman struct {
	*podman
	container string
	entered   chan struct{}
}

func (p *cancelFilePodman) Operate(ctx context.Context, _ []byte, _ int64, _ ...string) ([]byte, error) {
	_, err := p.podman.Run(ctx, "exec", p.container, "python3", "-c", `import os,time
if os.fork() == 0:
    os.setsid()
    for fd in (0,1,2): os.close(fd)
    while True: time.sleep(1)
`)
	close(p.entered)
	if err != nil {
		return nil, err
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
