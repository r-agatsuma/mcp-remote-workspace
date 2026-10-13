package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type execCommandRunner interface {
	startExec(context.Context, ExecRequest, io.Writer, io.Writer, ...string) (<-chan ExecOutcome, error)
}

// StartExec consumes an already attested immutable snapshot; it never discovers
// or targets a container by name. Service.Inject obtains that snapshot from the
// production ProcessProbe before permitting submission.
func (p *Podman) StartExec(ctx context.Context, snapshot ProcessSnapshot, r ExecRequest, stdout, stderr io.Writer) (<-chan ExecOutcome, error) {
	if !p.initialized {
		return nil, CompletedBackendError(ErrNotReady)
	}
	if snapshot.Exact.validate() != nil || !validHex(snapshot.ObjectID) {
		return nil, CompletedBackendError(ErrProcessUnsafe)
	}
	runner, ok := p.runner.(execCommandRunner)
	if !ok {
		return nil, CompletedBackendError(errors.New("streaming CLI capability is unavailable"))
	}
	args := []string{"exec", "--interactive", "--workdir", r.Cwd}
	if r.Detach {
		args = append(args, "--detach")
	}
	keys := make([]string, 0, len(r.Env))
	for key := range r.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--env", key+"="+r.Env[key])
	}
	args = append(args, "--", snapshot.ObjectID)
	args = append(args, r.Argv...)
	return runner.startExec(ctx, r, stdout, stderr, args...)
}

func (c localCLI) startExec(ctx context.Context, r ExecRequest, stdout, stderr io.Writer, args ...string) (<-chan ExecOutcome, error) {
	if err := ctx.Err(); err != nil {
		return nil, CompletedBackendError(err)
	}
	cmd := exec.CommandContext(ctx, c.path, append([]string{"--remote=false"}, args...)...)
	for _, key := range []string{"HOME", "PATH", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Stdin = strings.NewReader(r.Stdin)
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return nil, CompletedBackendError(err)
	}
	done := make(chan ExecOutcome, 1)
	go func() {
		err := cmd.Wait()
		outcome := ExecOutcome{ExitStatus: cmd.ProcessState.ExitCode(), Completion: DefinitelyCompleted}
		// Killing the local CLI, pipe loss, reserved runtime failures or failed
		// detached acceptance do not attest that injected work has completed.
		if ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay) || outcome.ExitStatus < 0 ||
			outcome.ExitStatus >= 125 && outcome.ExitStatus <= 127 || r.Detach && err != nil {
			outcome.Completion, outcome.Err = OutcomeUnknown, ErrOutcomeUnknown
		} else if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				outcome.Completion, outcome.Err = OutcomeUnknown, ErrOutcomeUnknown
			}
		}
		done <- outcome
		close(done)
	}()
	return done, nil
}
