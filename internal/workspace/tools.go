package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"path"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	MaxArgv        = 256
	MaxExecPayload = 256 << 10
	MaxTextBytes   = 1 << 20
	MaxStreamBytes = 64 << 10
)

type ExecRequest struct {
	Argv   []string          `json:"argv"`
	Cwd    string            `json:"cwd,omitempty"`
	Env    map[string]string `json:"env,omitempty"`
	Stdin  string            `json:"stdin,omitempty"`
	Detach bool              `json:"detach,omitempty"`
}

type StreamResult struct {
	Text        string `json:"text"`
	TotalBytes  uint64 `json:"total_bytes"`
	Truncated   bool   `json:"truncated"`
	InvalidUTF8 bool   `json:"invalid_utf8"`
}

type ExecResult struct {
	Status     string        `json:"status,omitempty"`
	ExitStatus *int          `json:"exit_status,omitempty"`
	Stdout     *StreamResult `json:"stdout,omitempty"`
	Stderr     *StreamResult `json:"stderr,omitempty"`
}

type ReadTextRequest struct {
	Path string `json:"path"`
}
type ReadTextResult struct {
	Text string `json:"text"`
}
type WriteTextRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
type WriteTextResult struct {
	Status string `json:"status"`
}

// ToolError contains only fixed, bounded messages. Runtime diagnostics, paths,
// argv, contents and underlying error strings never reach model-facing errors.
type ToolError struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	OutcomeUnknown bool   `json:"outcome_unknown,omitempty"`
	cause          error
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }
func (e *ToolError) Unwrap() error { return e.cause }
func toolError(code, message string, cause error) *ToolError {
	return &ToolError{Code: code, Message: message, cause: boundedToolCause(cause)}
}

// Retain only fixed sentinels, never arbitrary diagnostic strings or chains.
func boundedToolCause(err error) error {
	var causes []error
	for _, sentinel := range []error{ErrOutcomeUnknown, context.Canceled, context.DeadlineExceeded,
		ErrStaleGeneration, ErrNotReady, ErrInjectionBusy, ErrBackendLimit, ErrProcessUnsafe,
		ErrAbsoluteTTL, ErrIdleTTL, ErrStorageRecovery} {
		if errors.Is(err, sentinel) {
			causes = append(causes, sentinel)
		}
	}
	return errors.Join(causes...)
}

func mapToolError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrOutcomeUnknown) {
		return &ToolError{Code: "outcome_unknown", Message: "Completion is unknown; side effects may have begun. Do not replay automatically.", OutcomeUnknown: true, cause: boundedToolCause(err)}
	}
	var known *ToolError
	if errors.As(err, &known) {
		messages := map[string]string{
			"invalid_request": "Tool arguments do not match the request contract.",
			"invalid_path":    "An absolute container path without NUL or '..' components is required.",
			"text_too_large":  "Text exceeds the 1 MiB limit.",
			"invalid_utf8":    "Text is not valid UTF-8.",
			"file_io":         "Container file operation failed; partial content may remain after a write.",
			"unsupported":     "Runtime execution capability is unavailable.",
		}
		if message, ok := messages[known.Code]; ok {
			return toolError(known.Code, message, err)
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return toolError("timeout", "Request timed out.", err)
	case errors.Is(err, context.Canceled):
		return toolError("canceled", "Request was canceled.", err)
	case errors.Is(err, ErrStaleGeneration):
		return toolError("generation_unavailable", "The admitted exact generation is absent or has been replaced.", err)
	case errors.Is(err, ErrNotReady):
		return toolError("not_ready", "Workspace admission is closed.", err)
	case errors.Is(err, ErrInjectionBusy), errors.Is(err, ErrBackendLimit):
		return toolError("busy", "Runtime injection or backend capacity is unavailable.", err)
	case errors.Is(err, ErrProcessUnsafe):
		return toolError("process_unsafe", "Exact-generation process state could not be safely attested.", err)
	case errors.Is(err, ErrAbsoluteTTL), errors.Is(err, ErrIdleTTL):
		return toolError("expired", "The admitted generation has expired.", err)
	case errors.Is(err, ErrStorageRecovery):
		return toolError("storage_recovery", "Storage admission requires operator recovery.", err)
	default:
		return toolError("runtime_error", "Runtime operation failed.", err)
	}
}

func validateContainerPath(value string) error {
	if !path.IsAbs(value) || strings.ContainsRune(value, 0) {
		return toolError("invalid_path", "An absolute container path without NUL or '..' components is required.", nil)
	}
	for component := range strings.SplitSeq(value, "/") {
		if component == ".." {
			return toolError("invalid_path", "An absolute container path without NUL or '..' components is required.", nil)
		}
	}
	return nil
}

func (r ExecRequest) Validate() error {
	invalid := func() error {
		return toolError("invalid_request", "Invalid exec argv, environment, cwd or stdin.", nil)
	}
	if len(r.Argv) == 0 || len(r.Argv) > MaxArgv {
		return invalid()
	}
	// Preflight raw lengths before encoding to avoid allocating an unbounded copy.
	n := 0
	add := func(v string) bool {
		if strings.ContainsRune(v, 0) || len(v) > MaxExecPayload-n {
			return false
		}
		n += len(v) + 1
		return n <= MaxExecPayload
	}
	for _, a := range r.Argv {
		if !add(a) {
			return invalid()
		}
	}
	for k, v := range r.Env {
		if k == "" || strings.Contains(k, "=") || !add(k) || !add(v) {
			return invalid()
		}
	}
	payload, err := json.Marshal(struct {
		Argv []string          `json:"argv"`
		Env  map[string]string `json:"env,omitempty"`
	}{r.Argv, r.Env})
	if err != nil || len(payload) > MaxExecPayload {
		return invalid()
	}
	if r.Cwd != "" {
		if err := validateContainerPath(r.Cwd); err != nil {
			return err
		}
	}
	if len(r.Stdin) > MaxTextBytes || !utf8.ValidString(r.Stdin) || strings.ContainsRune(r.Stdin, 0) {
		return invalid()
	}
	return nil
}

// ExecRuntime is a trusted optional adapter capability. StartExec submits once,
// promptly, under the injection gate, and targets snapshot.ObjectID exclusively.
// It must honor ctx, never retry, and stream without retaining unbounded output.
// A nonnil error is uncertain unless marked CompletedBackendError (not started).
// A nil error transfers ownership of exactly one terminal outcome to Done.
// Internal file helpers can send UTF-8 stdin including NUL; public exec cannot.
type ExecRuntime interface {
	StartExec(context.Context, ProcessSnapshot, ExecRequest, io.Writer, io.Writer) (<-chan ExecOutcome, error)
}
type ExecOutcome struct {
	ExitStatus int
	Completion Completion // workload completion, or native detached acceptance
	Err        error
}

// runTool never retries. The waiter outlives caller cancellation only to account
// for trusted completion. The ledger bounds live waiters; no job object is kept.
func (s *Service) runTool(ctx context.Context, identity Identity, e Exact, r ExecRequest, stdout, stderr io.Writer) (ExecOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, s.toolTimeout)
	defer cancel()
	runtime, supported := s.runtime.(ExecRuntime)
	if err := ctx.Err(); err != nil {
		return ExecOutcome{}, mapToolError(err)
	}
	// Caller-owned collections may be reused after a timeout returns while the
	// original submission/completion worker is still unwinding.
	r.Argv, r.Env = slices.Clone(r.Argv), maps.Clone(r.Env)
	type result struct {
		outcome ExecOutcome
		err     error
	}
	finished := make(chan result, 1)
	go func() {
		// Keep lifecycle admission within the bounded waiter. A busy lifecycle
		// operation must not turn this preflight into an unbounded tool wait.
		s.mu.Lock()
		var admissionErr error
		if err := ctx.Err(); err != nil {
			admissionErr = err
		} else if err := s.admitted(identity); err != nil {
			admissionErr = err
		} else if identity.key != e.Workspace || s.entries[e.Workspace] != e {
			admissionErr = ErrStaleGeneration
		} else if s.records[e].Phase != Present {
			admissionErr = ErrNotReady
		} else if !supported {
			admissionErr = toolError("unsupported", "Runtime execution capability is unavailable.", nil)
		}
		s.mu.Unlock()
		if admissionErr != nil {
			finished <- result{err: admissionErr}
			return
		}
		outcome, err := s.awaitTool(ctx, runtime, identity, e, r, stdout, stderr)
		finished <- result{outcome, err}
	}()
	select {
	case result := <-finished:
		return result.outcome, mapToolError(result.err)
	case <-ctx.Done():
		// Admission/probe/submission can be racing cancellation. Conservatively
		// report uncertainty while the worker accounts for any later evidence.
		return ExecOutcome{}, mapToolError(errors.Join(ErrOutcomeUnknown, ctx.Err()))
	}
}

func (s *Service) awaitTool(ctx context.Context, runtime ExecRuntime, identity Identity, e Exact, r ExecRequest, stdout, stderr io.Writer) (ExecOutcome, error) {
	var done <-chan ExecOutcome
	op, err := s.Inject(ctx, identity, e, func(ctx context.Context, snapshot ProcessSnapshot) (result BackendResult) {
		// Preserve the landed gate's conservative panic accounting, while exposing
		// only a bounded error to the caller.
		defer func() {
			if recover() != nil {
				result = BackendResult{Err: ErrOutcomeUnknown}
			}
		}()
		var startErr error
		done, startErr = runtime.StartExec(ctx, snapshot, r, stdout, stderr)
		if startErr != nil {
			done = nil
			return BackendResult{Completion: completionOf(startErr), Err: startErr}
		}
		return BackendResult{Completion: OutcomeUnknown}
	})
	if done == nil || err != nil && !errors.Is(err, ErrOutcomeUnknown) {
		return ExecOutcome{}, err
	}
	outcome, ok := <-done
	if !ok {
		outcome = ExecOutcome{Err: ErrOutcomeUnknown}
	}
	var resultErr error
	if outcome.Completion == DefinitelyCompleted && !errors.Is(outcome.Err, ErrOutcomeUnknown) {
		resultErr = s.RecoverBackend(op, DefinitelyCompleted)
	} else {
		resultErr = ErrOutcomeUnknown
	}
	return outcome, errors.Join(outcome.Err, resultErr)
}

func (s *Service) Exec(ctx context.Context, identity Identity, e Exact, r ExecRequest) (ExecResult, error) {
	if err := r.Validate(); err != nil {
		return ExecResult{}, err
	}
	if r.Cwd == "" {
		r.Cwd = "/"
	}
	stdout, stderr := newStreamCapture(MaxStreamBytes), newStreamCapture(MaxStreamBytes)
	outcome, err := s.runTool(ctx, identity, e, r, stdout, stderr)
	if err != nil {
		return ExecResult{}, err
	}
	if r.Detach {
		if outcome.ExitStatus != 0 {
			return ExecResult{}, toolError("runtime_error", "Runtime did not accept detached execution.", nil)
		}
		return ExecResult{Status: "started"}, nil
	}
	out, diagnostic := stdout.result(), stderr.result()
	return ExecResult{ExitStatus: &outcome.ExitStatus, Stdout: &out, Stderr: &diagnostic}, nil
}

func (s *Service) ReadText(ctx context.Context, identity Identity, e Exact, r ReadTextRequest) (ReadTextResult, error) {
	if err := validateContainerPath(r.Path); err != nil {
		return ReadTextResult{}, err
	}
	// Only a fixed helper is interpreted by the container shell. Caller paths
	// are positional arguments; symlinks remain in the container namespace.
	stdout, stderr := newStreamCapture(MaxTextBytes+1), newStreamCapture(MaxStreamBytes)
	request := ExecRequest{Argv: []string{"/bin/sh", "-c", `exec head -c 1048577 -- "$1"`, "read_text", r.Path}, Cwd: "/"}
	outcome, err := s.runTool(ctx, identity, e, request, stdout, stderr)
	if err != nil {
		return ReadTextResult{}, err
	}
	if outcome.ExitStatus != 0 {
		return ReadTextResult{}, toolError("file_io", "Container file could not be read.", nil)
	}
	if stdout.totalBytes() > MaxTextBytes {
		return ReadTextResult{}, toolError("text_too_large", "File exceeds the 1 MiB text limit.", nil)
	}
	data := stdout.bytes()
	if !utf8.Valid(data) {
		return ReadTextResult{}, toolError("invalid_utf8", "File is not valid UTF-8 text.", nil)
	}
	return ReadTextResult{Text: string(data)}, nil
}

func (s *Service) WriteText(ctx context.Context, identity Identity, e Exact, r WriteTextRequest) (WriteTextResult, error) {
	if err := validateContainerPath(r.Path); err != nil {
		return WriteTextResult{}, err
	}
	if len(r.Content) > MaxTextBytes {
		return WriteTextResult{}, toolError("text_too_large", "Content exceeds the 1 MiB text limit.", nil)
	}
	if !utf8.ValidString(r.Content) {
		return WriteTextResult{}, toolError("invalid_utf8", "Content is not valid UTF-8 text.", nil)
	}
	stdout, stderr := newStreamCapture(MaxStreamBytes), newStreamCapture(MaxStreamBytes)
	request := ExecRequest{Argv: []string{"/bin/sh", "-c", `cat > "$1"`, "write_text", r.Path}, Cwd: "/", Stdin: r.Content}
	outcome, err := s.runTool(ctx, identity, e, request, stdout, stderr)
	if err != nil {
		return WriteTextResult{}, err
	}
	if outcome.ExitStatus != 0 {
		return WriteTextResult{}, toolError("file_io", "Container file write failed; partial content may remain.", nil)
	}
	return WriteTextResult{Status: "written"}, nil
}
