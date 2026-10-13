//go:build linux

package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type toolRuntime struct {
	*processRuntime
	start   func(context.Context, ProcessSnapshot, ExecRequest, io.Writer, io.Writer) (<-chan ExecOutcome, error)
	calls   int
	request ExecRequest
	target  ProcessSnapshot
}

func (f *toolRuntime) StartExec(ctx context.Context, p ProcessSnapshot, r ExecRequest, out, diagnostic io.Writer) (<-chan ExecOutcome, error) {
	f.calls++
	f.request, f.target = r, p
	if f.start != nil {
		return f.start(ctx, p, r, out, diagnostic)
	}
	return terminal(ExecOutcome{Completion: DefinitelyCompleted}), nil
}
func terminal(outcome ExecOutcome) <-chan ExecOutcome {
	done := make(chan ExecOutcome, 1)
	done <- outcome
	close(done)
	return done
}
func openTools(t *testing.T, cfg Config) (*Service, *toolRuntime, Identity, Exact) {
	t.Helper()
	f := &toolRuntime{processRuntime: &processRuntime{fakeRuntime: newFake()}}
	s, _, _ := openControlled(t, f, cfg)
	i := identity(t, "tools", "session")
	e, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	f.snapshot = safeSnapshot(e, s.policy, 1)
	return s, f, i, e
}
func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	var tool *ToolError
	if !errors.As(err, &tool) || tool.Code != code {
		t.Fatalf("error=%v; want code=%s", err, code)
	}
	encoded, marshalErr := json.Marshal(tool)
	if marshalErr != nil || len(encoded) > 300 || len(err.Error()) > 240 {
		t.Fatal("unbounded error", len(encoded), marshalErr)
	}
}

func TestExecRequestContract(t *testing.T) {
	valid := []ExecRequest{
		{Argv: []string{"/bin/echo", "$(no shell); $HOME", ""}},
		{Argv: []string{"/bin/sh", "-lc", "echo explicit shell"}, Cwd: "/a/./b//", Env: map[string]string{"A": "=value"}, Stdin: "日本語"},
		{Argv: make([]string, MaxArgv), Stdin: strings.Repeat("x", MaxTextBytes), Detach: true},
		{Argv: []string{strings.Repeat("a", MaxExecPayload-len(`{"argv":[""]}`))}},
	}
	for _, r := range valid {
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	invalid := []ExecRequest{
		{}, {Argv: []string{}}, {Argv: make([]string, MaxArgv+1)},
		{Argv: []string{"x\x00y"}}, {Argv: []string{"x"}, Cwd: "relative"},
		{Argv: []string{"x"}, Cwd: "/a/../b"}, {Argv: []string{"x"}, Cwd: "/a\x00"},
		{Argv: []string{"x"}, Env: map[string]string{"": "x"}},
		{Argv: []string{"x"}, Env: map[string]string{"a=b": "x"}},
		{Argv: []string{"x"}, Env: map[string]string{"a\x00b": "x"}},
		{Argv: []string{"x"}, Env: map[string]string{"a": "x\x00"}},
		{Argv: []string{strings.Repeat("a", MaxExecPayload)}},
		{Argv: []string{"x"}, Env: map[string]string{"a": strings.Repeat("b", MaxExecPayload)}},
		{Argv: []string{strings.Repeat("\n", MaxExecPayload/2)}},
		{Argv: []string{"x"}, Stdin: strings.Repeat("x", MaxTextBytes+1)},
		{Argv: []string{"x"}, Stdin: "\xff"}, {Argv: []string{"x"}, Stdin: "\x00"},
	}
	for n, r := range invalid {
		if r.Validate() == nil {
			t.Fatalf("invalid request %d accepted", n)
		}
	}
}

func TestExecResultAndInjectionAccounting(t *testing.T) {
	s, f, i, e := openTools(t, Config{})
	r := ExecRequest{Argv: []string{"/bin/echo", "$HOME;$(bad)"}, Env: map[string]string{"CUSTOM": "value"}, Stdin: "input"}
	f.start = func(_ context.Context, p ProcessSnapshot, request ExecRequest, out, diagnostic io.Writer) (<-chan ExecOutcome, error) {
		if p.Exact != e || p.ObjectID != string(e.Generation) || request.Cwd != "/" ||
			!reflect.DeepEqual(request.Argv, r.Argv) || !reflect.DeepEqual(request.Env, r.Env) || request.Stdin != r.Stdin {
			t.Fatal("request or exact target changed")
		}
		if s.injections[e] == nil || len(s.backend.Pending()) != 1 {
			t.Fatal("injection bypassed admission")
		}
		io.WriteString(out, "stdout 日本語")
		io.WriteString(diagnostic, "stderr")
		return terminal(ExecOutcome{ExitStatus: 7, Completion: DefinitelyCompleted}), nil
	}
	result, err := s.Exec(context.Background(), i, e, r)
	if err != nil || result.ExitStatus == nil || *result.ExitStatus != 7 || result.Stdout.Text != "stdout 日本語" || result.Stderr.Text != "stderr" {
		t.Fatal(result, err)
	}
	if result.Stdout.TotalBytes != uint64(len("stdout 日本語")) || result.Stdout.Truncated || result.Stdout.InvalidUTF8 || f.probes != 1 || f.calls != 1 || len(s.PendingBackend()) != 0 {
		t.Fatal("completion accounting or metadata", result)
	}
}

func allToolCalls(s *Service, i Identity, e Exact) []func() error {
	return []func() error{
		func() error {
			_, err := s.Exec(context.Background(), i, e, ExecRequest{Argv: []string{"true"}})
			return err
		},
		func() error {
			_, err := s.ReadText(context.Background(), i, e, ReadTextRequest{Path: "/f"})
			return err
		},
		func() error {
			_, err := s.WriteText(context.Background(), i, e, WriteTextRequest{Path: "/f", Content: "x"})
			return err
		},
	}
}

func TestToolsAbsentStaleAndClosedNeverCreateOrRetarget(t *testing.T) {
	s, f, i, old := openTools(t, Config{})
	if err := s.Destroy(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	for _, call := range allToolCalls(s, i, old) {
		expectCode(t, call(), "generation_unavailable")
	}
	if f.creates != 1 || f.calls != 0 || f.probes != 0 {
		t.Fatal("absent operation created or submitted")
	}
	fresh, err := s.Create(context.Background(), i)
	if err != nil || fresh == old {
		t.Fatal(fresh, err)
	}
	f.snapshot = safeSnapshot(fresh, s.policy, 1)
	for _, call := range allToolCalls(s, i, old) {
		expectCode(t, call(), "generation_unavailable")
	}
	other := identity(t, "other", "session")
	for _, call := range allToolCalls(s, other, fresh) {
		expectCode(t, call(), "generation_unavailable")
	}
	if f.calls != 0 || f.probes != 0 {
		t.Fatal("stale or cross-identity request retargeted")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, call := range allToolCalls(s, i, fresh) {
		expectCode(t, call(), "not_ready")
	}
}

func TestToolProcessAndBackendLimits(t *testing.T) {
	for _, mode := range []string{"pid-limit", "bad-exact", "bad-object", "backend-limit", "unknown-lifecycle"} {
		t.Run(mode, func(t *testing.T) {
			s, f, i, e := openTools(t, Config{})
			code := "process_unsafe"
			switch mode {
			case "pid-limit":
				f.snapshot.Tasks = s.policy.PIDsMax
				code = "busy"
			case "bad-exact":
				f.snapshot.Exact.Generation = Generation(strings.Repeat("a", 64))
			case "bad-object":
				f.snapshot.ObjectID = "bad"
			case "backend-limit":
				for range s.policy.MaxBackendOperations {
					if _, err := s.BeginBackend(e, "test"); err != nil {
						t.Fatal(err)
					}
				}
				code = "busy"
			case "unknown-lifecycle":
				if _, err := s.backend.Acquire(e, "create"); err != nil {
					t.Fatal(err)
				}
				code = "outcome_unknown"
			}
			for _, call := range allToolCalls(s, i, e) {
				expectCode(t, call(), code)
			}
			if f.calls != 0 {
				t.Fatal("unsafe or over-capacity operation injected")
			}
		})
	}
}

func TestToolUnknownStartPanicAndChannelLossNoReplay(t *testing.T) {
	for _, mode := range []string{"start-error", "panic", "nil-channel", "closed-channel", "terminal-unknown", "contradictory-completion"} {
		t.Run(mode, func(t *testing.T) {
			s, f, i, e := openTools(t, Config{})
			f.start = func(context.Context, ProcessSnapshot, ExecRequest, io.Writer, io.Writer) (<-chan ExecOutcome, error) {
				switch mode {
				case "start-error":
					return nil, errors.New(strings.Repeat("sensitive", 1<<20))
				case "panic":
					panic("sensitive panic")
				case "nil-channel":
					return nil, nil
				case "closed-channel":
					ch := make(chan ExecOutcome)
					close(ch)
					return ch, nil
				case "contradictory-completion":
					return terminal(ExecOutcome{Completion: DefinitelyCompleted, Err: ErrOutcomeUnknown}), nil
				default:
					return terminal(ExecOutcome{Err: ErrOutcomeUnknown}), nil
				}
			}
			_, err := s.Exec(context.Background(), i, e, ExecRequest{Argv: []string{"true"}})
			expectCode(t, err, "outcome_unknown")
			if !errors.Is(err, ErrOutcomeUnknown) || len(s.PendingBackend()) != 1 || f.calls != 1 {
				t.Fatal("lost unknown accounting or replay", err)
			}
			for _, call := range allToolCalls(s, i, e) {
				expectCode(t, call(), "busy")
			}
			if f.calls != 1 {
				t.Fatal("unknown submission was replayed")
			}
		})
	}
}

func TestToolDefinitelyNotStartedReleasesPermit(t *testing.T) {
	s, f, i, e := openTools(t, Config{})
	f.start = func(context.Context, ProcessSnapshot, ExecRequest, io.Writer, io.Writer) (<-chan ExecOutcome, error) {
		return nil, CompletedBackendError(errors.New("not started"))
	}
	_, err := s.Exec(context.Background(), i, e, ExecRequest{Argv: []string{"true"}})
	expectCode(t, err, "runtime_error")
	if len(s.PendingBackend()) != 0 || f.calls != 1 {
		t.Fatal("unstarted work leaked permit or retried")
	}
}

func waitNoPending(t *testing.T, s *Service) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for len(s.PendingBackend()) != 0 {
		select {
		case <-deadline.C:
			t.Fatal("terminal backend was not recovered")
		case <-ticker.C:
		}
	}
}

func TestToolTimeoutCancellationAndLateCompletion(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel", "before-start"} {
		t.Run(mode, func(t *testing.T) {
			s, f, i, e := openTools(t, Config{ToolTimeout: 20 * time.Millisecond})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan ExecOutcome, 1)
			started := make(chan struct{})
			f.start = func(context.Context, ProcessSnapshot, ExecRequest, io.Writer, io.Writer) (<-chan ExecOutcome, error) {
				close(started)
				if mode == "cancel" {
					cancel()
				}
				return done, nil
			}
			if mode == "before-start" {
				cancel()
			}
			begin := time.Now()
			_, err := s.Exec(ctx, i, e, ExecRequest{Argv: []string{"true"}})
			if time.Since(begin) > time.Second {
				t.Fatal("unbounded control-plane lifetime")
			}
			if mode == "before-start" {
				expectCode(t, err, "canceled")
				if f.calls != 0 || len(s.PendingBackend()) != 0 {
					t.Fatal("canceled request submitted")
				}
				return
			}
			expectCode(t, err, "outcome_unknown")
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("injection did not start")
			}
			if f.calls != 1 || len(s.PendingBackend()) != 1 {
				t.Fatal("cancellation released unknown work")
			}
			if err := s.DestroyGeneration(context.Background(), e); !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatal("destroy passed unresolved injection", err)
			}
			done <- ExecOutcome{Completion: DefinitelyCompleted}
			close(done)
			waitNoPending(t, s)
			if err := s.DestroyGeneration(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			fresh, err := s.Create(context.Background(), i)
			if err != nil || fresh == e {
				t.Fatal(fresh, err)
			}
			for _, call := range allToolCalls(s, i, e) {
				expectCode(t, call(), "generation_unavailable")
			}
		})
	}
}

func TestDetachedNativeAcceptanceOnly(t *testing.T) {
	s, f, i, e := openTools(t, Config{})
	f.start = func(_ context.Context, _ ProcessSnapshot, r ExecRequest, out, diagnostic io.Writer) (<-chan ExecOutcome, error) {
		if !r.Detach {
			t.Fatal("detach flag dropped")
		}
		io.WriteString(out, "native runtime ID must not escape")
		return terminal(ExecOutcome{Completion: DefinitelyCompleted}), nil
	}
	result, err := s.Exec(context.Background(), i, e, ExecRequest{Argv: []string{"sleep", "1"}, Detach: true})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	if string(data) != `{"status":"started"}` || len(s.PendingBackend()) != 0 || f.calls != 1 {
		t.Fatal("detached job/output retained", string(data))
	}
	// Acceptance is a terminal control-plane result, not a liveness check.
	if err := s.DestroyGeneration(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func TestDetachedWaitsForNativeAcceptance(t *testing.T) {
	s, f, i, e := openTools(t, Config{ToolTimeout: time.Second})
	done := make(chan ExecOutcome, 1)
	started := make(chan struct{})
	f.start = func(context.Context, ProcessSnapshot, ExecRequest, io.Writer, io.Writer) (<-chan ExecOutcome, error) {
		close(started)
		return done, nil
	}
	type result struct {
		value ExecResult
		err   error
	}
	returned := make(chan result, 1)
	go func() {
		value, err := s.Exec(context.Background(), i, e, ExecRequest{Argv: []string{"true"}, Detach: true})
		returned <- result{value, err}
	}()
	<-started
	select {
	case r := <-returned:
		t.Fatal("reported acceptance before runtime evidence", r)
	default:
	}
	if len(s.PendingBackend()) != 1 {
		t.Fatal("native submission was not accounted")
	}
	done <- ExecOutcome{Completion: DefinitelyCompleted, ExitStatus: 1}
	close(done)
	r := <-returned
	expectCode(t, r.err, "runtime_error")
	if r.value.Status == "started" || len(s.PendingBackend()) != 0 {
		t.Fatal("failed acceptance reported started")
	}
}

func TestToolLifetimeBoundsStalledProbeWithoutLateInjection(t *testing.T) {
	s, f, i, e := openTools(t, Config{ToolTimeout: 20 * time.Millisecond})
	probing, release := make(chan struct{}), make(chan struct{})
	f.hook = func() { close(probing); <-release }
	returned := make(chan error, 1)
	go func() {
		_, err := s.Exec(context.Background(), i, e, ExecRequest{Argv: []string{"true"}})
		returned <- err
	}()
	<-probing
	select {
	case err := <-returned:
		expectCode(t, err, "outcome_unknown")
	case <-time.After(time.Second):
		close(release)
		t.Fatal("stalled probe defeated control-plane deadline")
	}
	close(release)
	waitNoPending(t, s)
	if f.calls != 0 {
		t.Fatal("expired request injected after stalled probe returned")
	}
}

func TestRequestCollectionsRemainBoundAfterCallerTimeout(t *testing.T) {
	s, f, i, e := openTools(t, Config{ToolTimeout: 20 * time.Millisecond})
	done := make(chan ExecOutcome, 1)
	started := make(chan struct{})
	f.start = func(context.Context, ProcessSnapshot, ExecRequest, io.Writer, io.Writer) (<-chan ExecOutcome, error) {
		close(started)
		return done, nil
	}
	r := ExecRequest{Argv: []string{"original"}, Env: map[string]string{"A": "original"}}
	_, err := s.Exec(context.Background(), i, e, r)
	expectCode(t, err, "outcome_unknown")
	<-started
	r.Argv[0], r.Env["A"] = "reused", "reused"
	if f.request.Argv[0] != "original" || f.request.Env["A"] != "original" {
		t.Fatal("late request retargeted to caller's reused arguments")
	}
	done <- ExecOutcome{Completion: DefinitelyCompleted}
	close(done)
	waitNoPending(t, s)
}

func TestToolUnknownInjectionPersistsWithoutReplay(t *testing.T) {
	s, f, i, e := openTools(t, Config{})
	f.start = func(context.Context, ProcessSnapshot, ExecRequest, io.Writer, io.Writer) (<-chan ExecOutcome, error) {
		return terminal(ExecOutcome{Err: ErrOutcomeUnknown}), nil
	}
	_, err := s.WriteText(context.Background(), i, e, WriteTextRequest{Path: "/f", Content: "possibly written"})
	expectCode(t, err, "outcome_unknown")
	cfg, lock := testConfig(filepath.Dir(s.path)), s.lock.Name()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := open(context.Background(), cfg, func() Runtime { return f }, lock)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	for _, call := range allToolCalls(restarted, i, e) {
		expectCode(t, call(), "busy")
	}
	pending := restarted.PendingBackend()
	if f.calls != 1 || len(pending) != 1 || pending[0].Kind != "injection" || pending[0].Exact != e {
		t.Fatal("unknown write lost accounting or gained a job object", pending)
	}
}

func TestStreamCaptureMemoryResponseAndUTF8Bounds(t *testing.T) {
	b := newStreamCapture(MaxStreamBytes)
	block := bytes.Repeat([]byte("a"), 4096)
	io.WriteString(b, "HEAD")
	for range 1024 {
		if n, err := b.Write(block); n != len(block) || err != nil {
			t.Fatal(n, err)
		}
	}
	io.WriteString(b, "TAIL")
	r := b.result()
	if len(r.Text) != MaxStreamBytes || !strings.HasPrefix(r.Text, "HEAD") || !strings.HasSuffix(r.Text, "TAIL") || r.TotalBytes != 4<<20+8 || !r.Truncated || r.InvalidUTF8 {
		t.Fatal("head/tail metadata", r.TotalBytes)
	}
	if cap(b.head)+cap(b.tail) != MaxStreamBytes || cap(b.pending) > 8 {
		t.Fatal("output allocated beyond fixed retention")
	}
	invalid := newStreamCapture(MaxStreamBytes)
	for range 1024 {
		invalid.Write(bytes.Repeat([]byte{0xff}, 4096))
	}
	r = invalid.result()
	if len(r.Text) > MaxStreamBytes || !utf8.ValidString(r.Text) || !r.InvalidUTF8 || r.TotalBytes != 4<<20 {
		t.Fatal("unbounded invalid encoding", r.TotalBytes)
	}
	encoded, _ := json.Marshal(r)
	if len(encoded) > 6*MaxStreamBytes+200 {
		t.Fatal("unbounded JSON stream result")
	}
	for _, data := range [][]byte{[]byte("日本語🙂"), {0xf0, 0x28, 0x8c, 0xbc}, {0xe2, 0x82}, {0xff}, []byte("\xef\xbf\xbd")} {
		capture := newStreamCapture(64)
		for _, c := range data {
			capture.Write([]byte{c})
		}
		if capture.result().InvalidUTF8 == utf8.Valid(data) {
			t.Fatalf("UTF-8 scanner disagrees for %x", data)
		}
	}
	// Arbitrary chunk sizes and ring wraps must preserve exact retained bytes.
	random := rand.New(rand.NewSource(63))
	for range 300 {
		data := make([]byte, random.Intn(4096))
		random.Read(data)
		capture := newStreamCapture(127)
		for n := 0; n < len(data); {
			end := min(len(data), n+1+random.Intn(173))
			capture.Write(data[n:end])
			n = end
		}
		expected := data
		if len(data) > 127 {
			expected = append(append([]byte(nil), data[:64]...), data[len(data)-63:]...)
		}
		if !bytes.Equal(capture.bytes(), expected) || capture.result().InvalidUTF8 == utf8.Valid(data) {
			t.Fatal("ring or scanner disagrees")
		}
	}
}

func TestExecBoundsBothStreams(t *testing.T) {
	s, f, i, e := openTools(t, Config{})
	f.start = func(_ context.Context, _ ProcessSnapshot, _ ExecRequest, out, diagnostic io.Writer) (<-chan ExecOutcome, error) {
		for range 256 {
			out.Write(bytes.Repeat([]byte{0}, 4096))
			diagnostic.Write(bytes.Repeat([]byte{0xff}, 4096))
		}
		return terminal(ExecOutcome{Completion: DefinitelyCompleted}), nil
	}
	r, err := s.Exec(context.Background(), i, e, ExecRequest{Argv: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []*StreamResult{r.Stdout, r.Stderr} {
		if stream.TotalBytes != 1<<20 || !stream.Truncated || len(stream.Text) > MaxStreamBytes {
			t.Fatal("stream bound", stream.TotalBytes)
		}
	}
	encoded, _ := json.Marshal(r)
	if len(encoded) > 12*MaxStreamBytes+500 {
		t.Fatal("exec response bound", len(encoded))
	}
}

// This test adapter executes the fixed file helpers in a temporary fixture.
// It translates only the test container path, and is never production host I/O.
func fileFixtureAdapter(t *testing.T, s *Service, f *toolRuntime, root string) {
	t.Helper()
	f.start = func(ctx context.Context, p ProcessSnapshot, r ExecRequest, out, diagnostic io.Writer) (<-chan ExecOutcome, error) {
		if s.injections[p.Exact] == nil || len(s.backend.Pending()) != 1 {
			t.Fatal("file helper bypassed injection/backend gate")
		}
		if len(r.Argv) != 5 || r.Argv[0] != "/bin/sh" || r.Cwd != "/" || r.Detach {
			t.Fatal("unexpected file helper")
		}
		args := append([]string(nil), r.Argv...)
		args[4] = filepath.Join(root, strings.TrimPrefix(args[4], "/"))
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(r.Stdin), out, diagnostic
		err := cmd.Run()
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				return nil, CompletedBackendError(err)
			}
		}
		return terminal(ExecOutcome{ExitStatus: cmd.ProcessState.ExitCode(), Completion: DefinitelyCompleted}), nil
	}
}

func TestFilePathAndTextValidationBeforeInjection(t *testing.T) {
	s, f, i, e := openTools(t, Config{})
	for _, p := range []string{"", "relative", "/../x", "/a/../x", "/a/..", "/a\x00b"} {
		_, err := s.ReadText(context.Background(), i, e, ReadTextRequest{Path: p})
		expectCode(t, err, "invalid_path")
		_, err = s.WriteText(context.Background(), i, e, WriteTextRequest{Path: p})
		expectCode(t, err, "invalid_path")
	}
	_, err := s.WriteText(context.Background(), i, e, WriteTextRequest{Path: "/f", Content: strings.Repeat("x", MaxTextBytes+1)})
	expectCode(t, err, "text_too_large")
	_, err = s.WriteText(context.Background(), i, e, WriteTextRequest{Path: "/f", Content: "\xff"})
	expectCode(t, err, "invalid_utf8")
	if f.calls != 0 || f.probes != 0 || len(s.PendingBackend()) != 0 {
		t.Fatal("invalid file request injected")
	}
}

func TestFileCreateTruncateUTF8SizeAndSymlinks(t *testing.T) {
	s, f, i, e := openTools(t, Config{})
	root := t.TempDir()
	fileFixtureAdapter(t, s, f, root)
	path := "/file;$(caller text)"
	for _, content := range []string{strings.Repeat("界", MaxTextBytes/3) + "x", "短い", "", "UTF-8 NUL is valid: \x00"} {
		write, err := s.WriteText(context.Background(), i, e, WriteTextRequest{Path: path, Content: content})
		if err != nil || write.Status != "written" {
			t.Fatal(write, err)
		}
		read, err := s.ReadText(context.Background(), i, e, ReadTextRequest{Path: path})
		if err != nil || read.Text != content {
			t.Fatal("create/truncate/text round trip failed", len(read.Text), err)
		}
	}
	if err := os.Symlink(strings.TrimPrefix(path, "/"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteText(context.Background(), i, e, WriteTextRequest{Path: "/link", Content: "through symlink"}); err != nil {
		t.Fatal(err)
	}
	read, err := s.ReadText(context.Background(), i, e, ReadTextRequest{Path: path})
	if err != nil || read.Text != "through symlink" {
		t.Fatal(read, err)
	}
	for _, fixture := range []struct {
		name string
		data []byte
		code string
	}{
		{"large", bytes.Repeat([]byte("x"), MaxTextBytes+1), "text_too_large"},
		{"binary", []byte{0xff, 0xfe}, "invalid_utf8"},
	} {
		if err := os.WriteFile(filepath.Join(root, fixture.name), fixture.data, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := s.ReadText(context.Background(), i, e, ReadTextRequest{Path: "/" + fixture.name})
		expectCode(t, err, fixture.code)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/missing-parent/file", "/directory"} {
		_, err := s.WriteText(context.Background(), i, e, WriteTextRequest{Path: p, Content: "x"})
		expectCode(t, err, "file_io")
	}
	if _, err := os.Stat(filepath.Join(root, "missing-parent")); !os.IsNotExist(err) {
		t.Fatal("write created missing parent")
	}
	_, err = s.ReadText(context.Background(), i, e, ReadTextRequest{Path: "/absent"})
	expectCode(t, err, "file_io")
	if len(s.PendingBackend()) != 0 || f.calls != f.probes {
		t.Fatal("file helper accounting", f.calls, f.probes)
	}
}

func TestPartialWriteUnknownIsNeverReplayed(t *testing.T) {
	s, f, i, e := openTools(t, Config{})
	partial := ""
	f.start = func(_ context.Context, _ ProcessSnapshot, r ExecRequest, _, diagnostic io.Writer) (<-chan ExecOutcome, error) {
		partial = r.Stdin[:3]
		io.WriteString(diagnostic, strings.Repeat("private error", 1<<17))
		return terminal(ExecOutcome{Err: ErrOutcomeUnknown}), nil
	}
	_, err := s.WriteText(context.Background(), i, e, WriteTextRequest{Path: "/f", Content: "abcdef"})
	expectCode(t, err, "outcome_unknown")
	if partial != "abc" || f.calls != 1 || len(s.PendingBackend()) != 1 {
		t.Fatal("partial write hidden or retried")
	}
	for _, call := range allToolCalls(s, i, e) {
		expectCode(t, call(), "busy")
	}
	if f.calls != 1 {
		t.Fatal("partial write replayed")
	}
}

func TestErrorMappingDropsUnboundedDiagnostics(t *testing.T) {
	private := strings.Repeat("SECRET", 1<<20)
	for _, err := range []error{errors.New(private), CompletedBackendError(errors.New(private)), &ToolError{Code: private, Message: private}, &ToolError{Code: "file_io", Message: private}} {
		bounded := mapToolError(err)
		var tool *ToolError
		if !errors.As(bounded, &tool) || strings.Contains(bounded.Error(), "SECRET") {
			t.Fatal("diagnostics reflected")
		}
		encoded, _ := json.Marshal(tool)
		if len(encoded) > 300 || tool.cause != nil {
			t.Fatal("unbounded error response or retained cause")
		}
	}
	err := mapToolError(errors.Join(ErrOutcomeUnknown, context.Canceled, errors.New(private)))
	expectCode(t, err, "outcome_unknown")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal("lost bounded typed causes")
	}
}
