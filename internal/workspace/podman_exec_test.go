//go:build linux

package workspace

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type streamingStub struct {
	args    []string
	request ExecRequest
}

func (f *streamingStub) run(context.Context, ...string) ([]byte, error) {
	panic("exec must not use lifecycle buffered runner")
}
func (f *streamingStub) startExec(_ context.Context, r ExecRequest, _, _ io.Writer, args ...string) (<-chan ExecOutcome, error) {
	f.args, f.request = args, r
	return terminal(ExecOutcome{Completion: DefinitelyCompleted}), nil
}

func TestPodmanExecImmutableTargetAndLiteralArgv(t *testing.T) {
	e := Exact{identity(t, "s", "c").Workspace(), Generation(strings.Repeat("b", 64))}
	p := safeSnapshot(e, ReferencePolicy(), 1)
	for _, detach := range []bool{false, true} {
		stub := new(streamingStub)
		podman := &Podman{runner: stub, initialized: true}
		r := ExecRequest{Argv: []string{"/bin/echo", "$HOME;$(literal)", "--hostile"}, Cwd: "/work space", Env: map[string]string{"Z": "value=1", "A": "$HOME"}, Stdin: "input", Detach: detach}
		_, err := podman.StartExec(context.Background(), p, r, io.Discard, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"exec", "--interactive", "--workdir", "/work space"}
		if detach {
			want = append(want, "--detach")
		}
		want = append(want, "--env", "A=$HOME", "--env", "Z=value=1", "--", p.ObjectID)
		want = append(want, r.Argv...)
		if !reflect.DeepEqual(stub.args, want) || !reflect.DeepEqual(stub.request, r) {
			t.Fatal("literal argv/env/stdin or immutable target changed", stub.args)
		}
	}
}

func TestLocalStreamingCLICompletionAndOutput(t *testing.T) {
	root := t.TempDir()
	stub := filepath.Join(root, "podman")
	// Simulate a native runtime process. Container env overlays stay in argv;
	// remote-engine variables from the host must not reach this subprocess.
	script := `#!/bin/sh
if [ "$1" != '--remote=false' ] || [ "$CONTAINER_HOST" != '' ] || [ "$DOCKER_HOST" != '' ]; then exit 125; fi
shift
case "$1" in
separate) cat; printf diagnostic >&2; exit 7;;
large) head -c 2097152 /dev/zero; head -c 2097152 /dev/zero >&2;;
reserved) exit 125;;
detached) printf native-id; exit 0;;
reject) exit 1;;
timeout) exec sleep 5;;
esac
`
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINER_HOST", "must not escape")
	t.Setenv("DOCKER_HOST", "must not escape")
	for _, tc := range []struct {
		mode       string
		detach     bool
		completion Completion
		exit       int
	}{
		{"separate", false, DefinitelyCompleted, 7}, {"large", false, DefinitelyCompleted, 0},
		{"reserved", false, OutcomeUnknown, 125}, {"detached", true, DefinitelyCompleted, 0},
		{"reject", true, OutcomeUnknown, 1}, {"timeout", false, OutcomeUnknown, -1},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if tc.mode == "timeout" {
				var c context.CancelFunc
				ctx, c = context.WithTimeout(ctx, 20*time.Millisecond)
				defer c()
			}
			out, diagnostic := newStreamCapture(MaxStreamBytes), newStreamCapture(MaxStreamBytes)
			done, err := (localCLI{stub}).startExec(ctx, ExecRequest{Stdin: "UTF-8 input", Detach: tc.detach}, out, diagnostic, tc.mode)
			if err != nil {
				t.Fatal(err)
			}
			var outcome ExecOutcome
			select {
			case outcome = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("local process did not terminate")
			}
			if outcome.Completion != tc.completion || outcome.ExitStatus != tc.exit {
				t.Fatal(outcome)
			}
			if tc.mode == "separate" && (out.result().Text != "UTF-8 input" || diagnostic.result().Text != "diagnostic") {
				t.Fatal("streams/stdin changed")
			}
			if tc.mode == "large" && (out.totalBytes() != 2<<20 || diagnostic.totalBytes() != 2<<20 || !out.result().Truncated || !diagnostic.result().Truncated) {
				t.Fatal("output not drained and bounded")
			}
		})
	}
	_, err := (localCLI{filepath.Join(root, "missing")}).startExec(context.Background(), ExecRequest{}, io.Discard, io.Discard, "exec")
	if err == nil || completionOf(err) != DefinitelyCompleted {
		t.Fatal("unstarted local process lacks terminal evidence", err)
	}
}

func TestPodmanStillRequiresTrustedProcessProbe(t *testing.T) {
	s, f, i, e := openTools(t, Config{})
	// Exec capability alone cannot bypass the landed missing-probe behavior.
	type noProbe struct {
		Runtime
		ExecRuntime
	}
	s.runtime = noProbe{f.fakeRuntime, f}
	for _, call := range allToolCalls(s, i, e) {
		expectCode(t, call(), "process_unsafe")
	}
	if f.calls != 0 {
		t.Fatal("injected without process attestation")
	}
}
