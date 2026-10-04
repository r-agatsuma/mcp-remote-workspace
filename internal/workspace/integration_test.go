package workspace

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in only: requires a dedicated prepared account and prebuilt fixed image.
// This test never installs dependencies, builds/pulls images, or edits host config.
func TestRootlessPodmanIntegration(t *testing.T) {
	if os.Getenv("MCP_WORKSPACE_INTEGRATION") != "1" {
		t.Skip("set MCP_WORKSPACE_INTEGRATION=1 on a prepared rootless Podman host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p, err := newPodman()
	if err != nil {
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
	t.Run("safe execution and text IO", func(t *testing.T) {
		written, err := m.WriteText(ctx, w.ID, "./program.py", "print('hello 日本語')\n")
		if err != nil {
			t.Fatal(err)
		}
		read, err := m.ReadText(ctx, w.ID, "program.py")
		if err != nil || read.Path != "program.py" || read.Content != "print('hello 日本語')\n" || read.SizeBytes != written.SizeBytes || read.SHA256 != written.SHA256 {
			t.Fatalf("read: %+v, %v", read, err)
		}
		out, err := m.Exec(ctx, w.ID, ExecRequest{Argv: []string{"python3", "program.py"}})
		if err != nil || out.ExitCode == nil || *out.ExitCode != 0 || out.Stdout != "hello 日本語\n" {
			t.Fatalf("program: %+v, %v", out, err)
		}
		out, err = m.Exec(ctx, w.ID, ExecRequest{Argv: []string{"printf", "hello"}})
		if err != nil || out.Stdout != "hello" || out.Stderr != "" {
			t.Fatalf("argv: %+v, %v", out, err)
		}
		out, err = m.Exec(ctx, w.ID, ExecRequest{Argv: []string{"sleep", "30"}, Timeout: 200 * time.Millisecond})
		if err != nil || !out.TimedOut || out.ExitCode != nil {
			t.Fatalf("timeout: %+v, %v", out, err)
		}
		out, err = m.Exec(ctx, w.ID, ExecRequest{Argv: []string{"python3", "-c", "import os; os.write(1, b'o'*262145); os.write(2, b'e'*262145)"}})
		if err != nil || !out.StdoutTruncated || !out.StderrTruncated || out.Stdout != strings.Repeat("o", MaxStreamBytes) || out.Stderr != strings.Repeat("e", MaxStreamBytes) {
			t.Fatalf("output limits: stdout=%d stderr=%d err=%v", len(out.Stdout), len(out.Stderr), err)
		}
		_, err = m.Exec(ctx, w.ID, ExecRequest{Argv: []string{"python3", "-c", "import os; os.symlink('/tmp', 'escape'); open('binary', 'wb').write(b'\\xff'); open('oversized', 'wb').truncate(1048577)"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.ReadText(ctx, w.ID, "binary"); !errors.Is(err, ErrNonUTF8) {
			t.Fatalf("binary: %v", err)
		}
		if _, err := m.ReadText(ctx, w.ID, "oversized"); !errors.Is(err, ErrSizeLimit) {
			t.Fatalf("oversized: %v", err)
		}
		for _, name := range []string{"../escape.txt", "escape/file"} {
			if _, err := m.WriteText(ctx, w.ID, name, "text"); !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("escape %s: %v", name, err)
			}
		}
		if _, err := m.Exec(ctx, w.ID, ExecRequest{Argv: []string{"true"}, Cwd: "escape"}); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("cwd escape: %v", err)
		}
	})
	t.Run("descendant cleanup", func(t *testing.T) {
		testExecDescendantCleanup(t, ctx, m, w.ID)
	})
	t.Run("helper interference fails closed", func(t *testing.T) {
		for _, mode := range []string{"kill", "kill-closed-pipes", "stop"} {
			t.Run(mode, func(t *testing.T) {
				// Keep a daemonized mutator alive after disabling the helper.
				// It also tries the directory-move race from the review. No
				// mutator may remain when managed file operations resume.
				script := `import os, signal, sys, time
helper = os.getppid()
namespace = os.readlink('/proc/self/ns/pid')
open('execution.namespace', 'w').write(namespace)
os.makedirs('race-dir', exist_ok=True)
if os.fork() == 0:
    os.setsid()
    if os.fork() != 0:
        os._exit(0)
    os.close(1)
    os.close(2)
    open('mutator.progress', 'w').write('x')
    open('mutator.ready', 'w').write('ready')
    while True:
        open('mutator.progress', 'a').write('x')
        os.rename('race-dir', '/tmp/mcp-race-dir')
        os.symlink('/tmp/mcp-race-dir', 'race-dir')
        os.unlink('race-dir')
        os.rename('/tmp/mcp-race-dir', 'race-dir')
        time.sleep(0.005)
while not os.path.exists('mutator.ready'):
    time.sleep(0.001)
if sys.argv[1] == 'kill-closed-pipes':
    os.close(1)
    os.close(2)
os.kill(helper, signal.SIGSTOP if sys.argv[1] == 'stop' else signal.SIGKILL)
time.sleep(30)
`
				if _, err := m.Exec(ctx, w.ID, ExecRequest{Argv: []string{"python3", "-c", script, mode}, Timeout: 100 * time.Millisecond}); err == nil {
					t.Fatal("disabled helper returned successful execution")
				}
				c, err := m.inspectOperationContainer(ctx, m.entries[w.ID])
				if err != nil || c.State == nil || !c.State.Running {
					t.Fatalf("workspace identity/profile did not survive reset: %+v, %v", c, err)
				}
				// Direct trusted probe avoids changing the namespace a second
				// time and detects zombies as well as running processes.
				probe := `import os
assert open('/workspace/execution.namespace').read() != os.readlink('/proc/self/ns/pid')
remaining = [pid for pid in os.listdir('/proc') if pid.isdecimal() and int(pid) not in (1, os.getpid())]
assert not remaining, remaining
`
				if _, err := p.Run(ctx, "exec", id, "/usr/bin/python3", "-I", "-c", probe); err != nil {
					t.Fatalf("host reset left processes: %v", err)
				}
				before, err := m.ReadText(ctx, w.ID, "mutator.progress")
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(100 * time.Millisecond)
				after, err := m.ReadText(ctx, w.ID, "mutator.progress")
				if err != nil || after.SHA256 != before.SHA256 {
					t.Fatalf("mutator persisted into file I/O: %+v, %v", after, err)
				}
				// Restore the stopped mutator's final directory state, then
				// demonstrate that file I/O still uses the same writable layer.
				out, err := m.Exec(ctx, w.ID, ExecRequest{Argv: []string{"python3", "-c", "import os; os.unlink('mutator.ready'); os.unlink('mutator.progress'); os.path.islink('race-dir') and os.unlink('race-dir'); os.path.exists('/tmp/mcp-race-dir') and os.rename('/tmp/mcp-race-dir', 'race-dir')"}})
				if err != nil || out.ExitCode == nil || *out.ExitCode != 0 {
					t.Fatalf("directory restoration: %+v, %v", out, err)
				}
				if _, err := m.WriteText(ctx, w.ID, "race-dir/new", "preserved"); err != nil {
					t.Fatal(err)
				}
				read, err := m.ReadText(ctx, w.ID, "race-dir/new")
				if err != nil || read.Content != "preserved" {
					t.Fatalf("file I/O after interference: %+v, %v", read, err)
				}
			})
		}
	})
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
	restarted, err := New(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if recovered := restarted.entries[w.ID]; recovered == nil || !recovered.CreatedAt.Equal(w.CreatedAt) {
		t.Fatal("restart lost workspace ID")
	}
	if err := restarted.Destroy(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := restarted.discover(ctx, w.ID)
	if err != nil || len(ids) != 0 {
		t.Fatalf("destroy left compute: %v, %v", ids, err)
	}
	if err := restarted.Destroy(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Destroy(ctx, "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ID: %v", err)
	}
}
