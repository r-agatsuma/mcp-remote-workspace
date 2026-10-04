package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Only this test runner executes the helper on the host, with /workspace
// replaced by a test-owned temporary root. Production always uses podman exec.
type helperPodman struct {
	*fakePodman
	root     string
	input    []byte
	args     []string
	response []byte
}

func (f *helperPodman) RunInput(ctx context.Context, input []byte, limit int64, args ...string) ([]byte, error) {
	f.input, f.args = slices.Clone(input), slices.Clone(args)
	if f.response != nil {
		return f.response, nil
	}
	root, _ := json.Marshal(f.root)
	script := strings.Replace(operationHelper, "os.open('/workspace', flags)", "os.open("+string(root)+", flags)", 1)
	cmd := exec.CommandContext(ctx, "/usr/bin/python3", "-I", "-c", script)
	cmd.Stdin = bytes.NewReader(input)
	stdout, stderr := &boundedBuffer{limit: limit}, &boundedBuffer{limit: MaxStreamBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if stdout.truncated {
		return nil, errors.New("test helper transport overflow")
	}
	return stdout.data, nil
}

func operationWorkspace(t *testing.T, max int64) (*Manager, *helperPodman, string) {
	t.Helper()
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("Python 3 is required to exercise the embedded container helper")
	}
	f := &helperPodman{fakePodman: newFake(), root: t.TempDir()}
	m, err := NewWithOptions(context.Background(), f, Options{MaxTextFileBytes: max})
	if err != nil {
		t.Fatal(err)
	}
	w, err := m.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m, f, w.ID
}

func TestWriteReadAndExecute(t *testing.T) {
	m, f, id := operationWorkspace(t, 0)
	ctx := context.Background()
	if err := os.Mkdir(filepath.Join(f.root, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	content := "print('hello 日本語')\n"
	name := "src/program ' $(touch injected).py"
	written, err := m.WriteText(ctx, id, "./src//"+filepath.Base(name), content)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(content))
	if written.Path != name || written.SizeBytes != int64(len(content)) || written.SHA256 != hex.EncodeToString(hash[:]) {
		t.Fatalf("write result: %+v", written)
	}
	read, err := m.ReadText(ctx, id, name)
	if err != nil || read.Content != content || read.Path != written.Path || read.SizeBytes != written.SizeBytes || read.SHA256 != written.SHA256 {
		t.Fatalf("read result: %+v, %v", read, err)
	}
	out, err := m.Exec(ctx, id, ExecRequest{Argv: []string{"python3", filepath.Base(name)}, Cwd: "src"})
	if err != nil || out.ExitCode == nil || *out.ExitCode != 0 || out.Stdout != "hello 日本語\n" || out.Stderr != "" {
		t.Fatalf("execute program: %+v, %v", out, err)
	}
	if !slices.Equal(f.args[:3], []string{"exec", "--interactive", "--workdir=/"}) || f.args[3] != m.entries[id].container || !slices.Equal(f.args[4:7], []string{"/usr/bin/python3", "-I", "-c"}) || f.args[7] != operationHelper {
		t.Fatalf("unsafe operation command: %v", f.args[:7])
	}
	// Replace completely, including shrinking to an empty file.
	if _, err := m.WriteText(ctx, id, name, ""); err != nil {
		t.Fatal(err)
	}
	read, err = m.ReadText(ctx, id, name)
	if err != nil || read.Content != "" || read.SizeBytes != 0 {
		t.Fatalf("empty replacement: %+v, %v", read, err)
	}
	if matches, err := filepath.Glob(filepath.Join(f.root, "src", ".mcp-write-*")); err != nil || len(matches) != 0 {
		t.Fatalf("temporary files: %v, %v", matches, err)
	}
}

func TestExecArgvEnvironmentAndExit(t *testing.T) {
	m, f, id := operationWorkspace(t, 0)
	ctx := context.Background()
	for _, tc := range []struct {
		argv           []string
		stdout, stderr string
		exit           int
		env            map[string]string
	}{
		{[]string{"printf", "hello"}, "hello", "", 0, nil},
		{[]string{"printf", "%s", "$HOME; $(touch injected) *", ""}, "$HOME; $(touch injected) *", "", 0, nil},
		{[]string{"bash", "-lc", "printf '%s' \"$VALUE\"; printf error >&2; exit 7"}, "override; $HOME", "error", 7, map[string]string{"VALUE": "override; $HOME"}},
		{[]string{"python3", "-c", "import os; print(os.getcwd())"}, f.root + "\n", "", 0, nil},
	} {
		out, err := m.Exec(ctx, id, ExecRequest{Argv: tc.argv, Env: tc.env})
		if err != nil || out.ExitCode == nil || *out.ExitCode != tc.exit || out.Stdout != tc.stdout || out.Stderr != tc.stderr || out.TimedOut || out.StdoutTruncated || out.StderrTruncated {
			t.Fatalf("argv %v: %+v, %v", tc.argv, out, err)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, "injected")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shell interpolation: %v", err)
	}
}

func TestExecTimeoutKillsProcessGroup(t *testing.T) {
	m, f, id := operationWorkspace(t, 0)
	start := time.Now()
	out, err := m.Exec(context.Background(), id, ExecRequest{Timeout: 200 * time.Millisecond, Argv: []string{"bash", "-c", "sleep 30 & child=$!; printf '%s' \"$child\" > child.pid; wait"}})
	if err != nil || !out.TimedOut || out.ExitCode != nil || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout: %+v, %v", out, err)
	}
	pidBytes, err := os.ReadFile(filepath.Join(f.root, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("timeout left a running or zombie child: %v", err)
	}
	// Closed output pipes must not disable the deadline.
	out, err = m.Exec(context.Background(), id, ExecRequest{Timeout: 100 * time.Millisecond, Argv: []string{"bash", "-c", "exec 1>&- 2>&-; sleep 30"}})
	if err != nil || !out.TimedOut {
		t.Fatalf("closed-pipe timeout: %+v, %v", out, err)
	}
}

func TestExecDescendantCleanup(t *testing.T) {
	m, _, id := operationWorkspace(t, 0)
	testExecDescendantCleanup(t, context.Background(), m, id)
}

// Run against both the embedded-helper test runner and the actual container.
func testExecDescendantCleanup(t *testing.T, ctx context.Context, m *Manager, id string) {
	t.Helper()
	for _, mode := range []string{"group", "setsid", "double-fork", "closed-pipes", "normal-exit"} {
		t.Run(mode, func(t *testing.T) {
			// Repeated timeouts must leave neither running descendants nor zombies.
			for iteration := 0; iteration < 3; iteration++ {
				script := `import os, sys, time
mode = sys.argv[1]
with open('descendants.pid', 'w') as stream:
    stream.write(str(os.getpid()) + '\n')
pid = os.fork()
if pid == 0:
    if mode != 'group':
        os.setsid()
    with open('descendants.pid', 'a') as stream:
        stream.write(str(os.getpid()) + '\n')
    if mode == 'double-fork':
        pid = os.fork()
        if pid != 0:
            os._exit(0)
        with open('descendants.pid', 'a') as stream:
            stream.write(str(os.getpid()) + '\n')
    if mode in ('closed-pipes', 'normal-exit'):
        os.close(1)
        os.close(2)
    time.sleep(30)
if mode == 'double-fork':
    os.waitpid(pid, 0)
if mode == 'normal-exit':
    while len(open('descendants.pid').read().split()) < 2:
        time.sleep(0.001)
    sys.exit(7)
if mode == 'closed-pipes':
    os.close(1)
    os.close(2)
time.sleep(30)
`
				start := time.Now()
				out, err := m.Exec(ctx, id, ExecRequest{Argv: []string{"python3", "-c", script, mode}, Timeout: 300 * time.Millisecond})
				if err != nil || time.Since(start) > 3*time.Second {
					t.Fatalf("iteration %d: %+v, %v", iteration, out, err)
				}
				if mode == "normal-exit" {
					if out.TimedOut || out.ExitCode == nil || *out.ExitCode != 7 {
						t.Fatalf("normal exit: %+v", out)
					}
				} else if !out.TimedOut || out.ExitCode != nil {
					t.Fatalf("timeout: %+v", out)
				}
				pids, err := m.ReadText(ctx, id, "descendants.pid")
				want := 2
				if mode == "double-fork" {
					want = 3
				}
				if err != nil || len(strings.Fields(pids.Content)) != want {
					t.Fatalf("descendant records: %+v, %v", pids, err)
				}
				// /proc entries persist for zombies too. Check in the same PID
				// namespace as the operation, after the helper has returned.
				check := "import os, sys; remaining = [pid for pid in sys.argv[1].split() if os.path.exists('/proc/' + pid)]; print(remaining); sys.exit(bool(remaining))"
				out, err = m.Exec(ctx, id, ExecRequest{Argv: []string{"python3", "-c", check, pids.Content}})
				if err != nil || out.ExitCode == nil || *out.ExitCode != 0 {
					t.Fatalf("iteration %d left descendants (including zombies): %+v, %v", iteration, out, err)
				}
			}
		})
	}
}

func TestExecOutputBounds(t *testing.T) {
	m, _, id := operationWorkspace(t, 0)
	for _, size := range []int{MaxStreamBytes, MaxStreamBytes + 1, 4 * MaxStreamBytes} {
		script := "import os; os.write(1, b'o'*" + strconv.Itoa(size) + "); os.write(2, b'e'*" + strconv.Itoa(size) + ")"
		out, err := m.Exec(context.Background(), id, ExecRequest{Argv: []string{"python3", "-c", script}})
		want := min(size, MaxStreamBytes)
		if err != nil || out.Stdout != strings.Repeat("o", want) || out.Stderr != strings.Repeat("e", want) || out.StdoutTruncated != (size > want) || out.StderrTruncated != (size > want) {
			t.Fatalf("output bound %d: stdout=%d stderr=%d flags=%v/%v err=%v", size, len(out.Stdout), len(out.Stderr), out.StdoutTruncated, out.StderrTruncated, err)
		}
	}
	// Invalid UTF-8 replacement must not increase the tool path beyond the bound.
	out, err := m.Exec(context.Background(), id, ExecRequest{Argv: []string{"python3", "-c", "import os; os.write(1, b'\\xff'*262144)"}})
	if err != nil || len(out.Stdout) > MaxStreamBytes || !out.StdoutTruncated {
		t.Fatalf("binary output: %d bytes, %v", len(out.Stdout), err)
	}
}

func TestPathSecurity(t *testing.T) {
	m, f, id := operationWorkspace(t, 0)
	ctx := context.Background()
	for _, name := range []string{"../escape", "a/../../escape", "a/../escape", "/etc/passwd", "/workspace/file", "file\x00suffix"} {
		if _, err := m.WriteText(ctx, id, name, "text"); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("write %q: %v", name, err)
		}
		if _, err := m.ReadText(ctx, id, name); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("read %q: %v", name, err)
		}
		if _, err := m.Exec(ctx, id, ExecRequest{Argv: []string{"true"}, Cwd: name}); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("cwd %q: %v", name, err)
		}
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"link": outside, "file-link": filepath.Join(outside, "secret"), "dangling": filepath.Join(outside, "new"), "inside": f.root} {
		if err := os.Symlink(target, filepath.Join(f.root, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"link/secret", "link/new", "file-link", "dangling", "inside/file"} {
		if _, err := m.WriteText(ctx, id, name, "overwrite"); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("write symlink %s: %v", name, err)
		}
		if _, err := m.ReadText(ctx, id, name); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("read symlink %s: %v", name, err)
		}
	}
	for _, name := range []string{"link", "inside", "file-link"} {
		if _, err := m.Exec(ctx, id, ExecRequest{Argv: []string{"true"}, Cwd: name}); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("cwd symlink %s: %v", name, err)
		}
	}
	secret, err := os.ReadFile(filepath.Join(outside, "secret"))
	if err != nil || string(secret) != "unchanged" {
		t.Fatalf("outside modified: %q, %v", secret, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside created: %v", err)
	}
	if _, err := m.WriteText(ctx, id, "missing/file", "text"); err == nil {
		t.Fatal("write created missing parent")
	}
	if _, err := os.Stat(filepath.Join(f.root, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("parent unexpectedly created")
	}
	if err := syscall.Mkfifo(filepath.Join(f.root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadText(ctx, id, "fifo"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("special file: %v", err)
	}
}

func TestReadTextBinaryAndSizeLimits(t *testing.T) {
	m, f, id := operationWorkspace(t, 4)
	for _, tc := range []struct {
		data []byte
		err  error
	}{
		{[]byte("1234"), nil}, {[]byte("日本"), ErrSizeLimit}, {[]byte("12345"), ErrSizeLimit},
		{[]byte{0xff}, ErrNonUTF8}, {[]byte{'a', 0, 'b'}, ErrNonUTF8}, {[]byte{0xc3}, ErrNonUTF8},
		{[]byte("é"), nil}, {[]byte{}, nil},
	} {
		if err := os.WriteFile(filepath.Join(f.root, "file"), tc.data, 0600); err != nil {
			t.Fatal(err)
		}
		out, err := m.ReadText(context.Background(), id, "./file")
		if !errors.Is(err, tc.err) {
			t.Fatalf("read %x: %+v, %v, want %v", tc.data, out, err, tc.err)
		}
		if err == nil && (out.Content != string(tc.data) || out.SizeBytes != int64(len(tc.data))) {
			t.Fatalf("truncated read: %+v", out)
		}
	}
}

func TestOperationValidationAndOwnership(t *testing.T) {
	m, f, id := operationWorkspace(t, 0)
	ctx := context.Background()
	for _, in := range []ExecRequest{{}, {Argv: []string{""}}, {Argv: []string{"true", "a\x00b"}}, {Argv: []string{"true"}, Env: map[string]string{"A=B": "x"}}, {Argv: []string{"true"}, Env: map[string]string{"A": "x\x00"}}, {Argv: []string{"true"}, Timeout: -1}, {Argv: []string{"true"}, Timeout: MaxExecTimeout + 1}} {
		if _, err := m.Exec(ctx, id, in); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("exec validation %+v: %v", in, err)
		}
	}
	if _, err := m.WriteText(ctx, id, "file", "\xff"); !errors.Is(err, ErrNonUTF8) {
		t.Fatalf("write validation: %v", err)
	}
	if _, err := m.ReadText(ctx, "unknown", "file"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ID: %v", err)
	}
	if _, err := NewWithOptions(ctx, newFake(), Options{MaxTextFileBytes: -1}); err == nil {
		t.Fatal("negative size accepted")
	}
	for _, c := range f.containers {
		c.Config.Labels[idLabel] = "ws_" + strings.Repeat("f", 64)
	}
	f.input = nil
	if _, err := m.Exec(ctx, id, ExecRequest{Argv: []string{"true"}}); err == nil || f.input != nil {
		t.Fatalf("ownership drift entered container: %v", err)
	}
}

func TestBoundedBufferDrains(t *testing.T) {
	b := &boundedBuffer{limit: 3}
	for _, data := range []string{"ab", "c", "def", "ghi"} {
		if n, err := b.Write([]byte(data)); err != nil || n != len(data) {
			t.Fatalf("write: %d, %v", n, err)
		}
	}
	if string(b.data) != "abc" || !b.truncated {
		t.Fatalf("buffer: %+v", b)
	}
}

func TestUntrustedHelperOutputIsBoundedOnHost(t *testing.T) {
	m, f, id := operationWorkspace(t, 4)
	f.response, _ = json.Marshal(map[string]any{"result": ExecResult{Stdout: strings.Repeat("é", MaxStreamBytes), Stderr: strings.Repeat("e", MaxStreamBytes+1)}})
	out, err := m.Exec(context.Background(), id, ExecRequest{Argv: []string{"true"}})
	if err != nil || len(out.Stdout) != MaxStreamBytes || len(out.Stderr) != MaxStreamBytes || !out.StdoutTruncated || !out.StderrTruncated {
		t.Fatalf("untrusted exec: stdout=%d stderr=%d err=%v", len(out.Stdout), len(out.Stderr), err)
	}
	f.response = []byte(`{"result":{"content":"12345","size_bytes":5}}`)
	if _, err := m.ReadText(context.Background(), id, "file"); !errors.Is(err, ErrSizeLimit) {
		t.Fatalf("untrusted read: %v", err)
	}
}
