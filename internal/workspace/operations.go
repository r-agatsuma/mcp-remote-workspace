package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/r-agatsuma/mcp-remote-workspace/internal/control"
)

var ErrBlocked = errors.New("workspace is blocked; destroy and recreate it")

// OperationRunner keeps stdin and the bounded control response separate from
// lifecycle commands. Implementations must wait for the host exec child to exit.
type OperationRunner interface {
	Operate(context.Context, []byte, int64, ...string) ([]byte, error)
}

type ExecOptions struct {
	Argv           []string
	Cwd            string
	Env            map[string]string
	TimeoutSeconds int
}

func (m *Manager) Exec(ctx context.Context, id string, in ExecOptions) (*control.ExecResult, error) {
	if in.Cwd == "" {
		in.Cwd = "."
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = control.DefaultTimeoutSeconds
	}
	if in.TimeoutSeconds < 1 || in.TimeoutSeconds > control.MaxTimeoutSeconds {
		return nil, &control.Error{Code: "invalid_argument", Message: "timeout_seconds must be between 1 and 300"}
	}
	if _, err := control.Normalize(in.Cwd, false); err != nil {
		return nil, err
	}
	response, err := m.operate(ctx, id, control.Request{Operation: "exec", Argv: in.Argv, Cwd: in.Cwd, Env: in.Env, TimeoutSeconds: in.TimeoutSeconds})
	if err != nil {
		return nil, err
	}
	return response.Exec, nil
}

func (m *Manager) WriteText(ctx context.Context, id, path, content string) (*control.TextResult, error) {
	if !utf8.ValidString(content) {
		return nil, &control.Error{Code: "non_utf8", Message: "content must be valid UTF-8"}
	}
	if int64(len(content)) > m.maxTextBytes {
		return nil, &control.Error{Code: "size_limit", Message: "text content exceeds the configured byte limit"}
	}
	response, err := m.operate(ctx, id, control.Request{Operation: "write_text", Path: path, Content: content})
	if err != nil {
		return nil, err
	}
	return response.Text, nil
}

func (m *Manager) ReadText(ctx context.Context, id, path string) (*control.TextResult, error) {
	response, err := m.operate(ctx, id, control.Request{Operation: "read_text", Path: path})
	if err != nil {
		return nil, err
	}
	return response.Text, nil
}

func (m *Manager) operate(ctx context.Context, id string, req control.Request) (*control.Response, error) {
	m.mu.Lock()
	e := m.entries[id]
	m.mu.Unlock()
	if e == nil {
		return nil, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	if e.deleted {
		return nil, ErrNotFound
	}
	if e.blocked || e.container == "" {
		return nil, ErrBlocked
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runner, ok := m.runner.(OperationRunner)
	if !ok {
		return nil, errors.New("backend does not support control operations")
	}
	req.MaxTextBytes = m.maxTextBytes
	input, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	seconds := req.TimeoutSeconds + 5
	if req.Operation != "exec" {
		seconds = control.FileTimeoutSeconds
	}
	opCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	// JSON escaping may cost up to six bytes per text byte. Bound even a forged
	// helper response on the host; no unbounded Podman stdout/stderr buffers.
	limit := 6*req.MaxTextBytes + 4096
	if req.Operation == "exec" {
		limit = 6*(2*control.StreamLimit) + 4096
	}
	data, runErr := runner.Operate(opCtx, input, limit, "exec", "--interactive", "--user=0:0", "--workdir=/", e.container, controlContainerPath, "operate")
	var response control.Response
	if runErr == nil && !utf8.Valid(data) {
		runErr = errors.New("control response is not UTF-8")
	}
	if runErr == nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		runErr = decoder.Decode(&response)
		var extra any
		if runErr == nil && decoder.Decode(&extra) != io.EOF {
			runErr = errors.New("multiple control responses")
		}
		if runErr == nil {
			runErr = validateResponseFields(data, req.Operation)
		}
		if runErr == nil {
			runErr = validateResponse(req, response)
		}
	}
	if runErr == nil {
		runErr = opCtx.Err()
	}
	// The helper shares the workspace identity, so even a well-formed success
	// cannot prove that daemonized descendants are gone. Reset on EVERY path.
	if resetErr := m.reset(e); resetErr != nil {
		if err := m.block(e); err != nil {
			return nil, fmt.Errorf("cleanup failed: %v; %w; block marker failed: %v", resetErr, ErrBlocked, err)
		}
		return nil, fmt.Errorf("workspace process cleanup failed; %w: %v", ErrBlocked, resetErr)
	}
	if runErr != nil {
		return nil, fmt.Errorf("uncertain control operation: %w", runErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, response.Error
	}
	return &response, nil
}

func validTextResponse(content string, result *control.TextResult) bool {
	hash := sha256.Sum256([]byte(content))
	return utf8.ValidString(content) && int64(len(content)) == result.SizeBytes && result.SHA256 == hex.EncodeToString(hash[:])
}

func validateResponseFields(data []byte, operation string) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	invalid := errors.New("malformed control response")
	if len(envelope) != 1 {
		return invalid
	}
	key := "text"
	required := []string{"path", "size_bytes", "sha256"}
	if operation == "read_text" {
		required = append(required, "content")
	}
	if operation == "exec" {
		key = "exec"
		required = []string{"exit_code", "stdout", "stderr", "stdout_truncated", "stderr_truncated", "timed_out"}
	}
	if _, ok := envelope["error"]; ok {
		key = "error"
		required = []string{"code", "message"}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(envelope[key], &fields); err != nil {
		return invalid
	}
	for _, field := range required {
		value, ok := fields[field]
		if !ok || (field != "exit_code" && bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
			return invalid
		}
	}
	return nil
}

func validateResponse(req control.Request, response control.Response) error {
	invalid := errors.New("malformed control response")
	if response.Error != nil {
		if response.Exec != nil || response.Text != nil || response.Error.Message == "" || len(response.Error.Message) > 1024 {
			return invalid
		}
		switch response.Error.Code {
		case "invalid_argument", "invalid_path", "non_utf8", "size_limit", "backend_error":
			return nil
		default:
			return invalid
		}
	}
	if req.Operation == "exec" {
		r := response.Exec
		if r == nil || response.Text != nil || len(r.Stdout) > control.StreamLimit || len(r.Stderr) > control.StreamLimit || (r.TimedOut && r.ExitCode != nil) || (!r.TimedOut && r.ExitCode == nil) {
			return invalid
		}
		return nil
	}
	r := response.Text
	want, err := control.Normalize(req.Path, true)
	if err != nil {
		return invalid
	}
	if r == nil || response.Exec != nil || r.Path != want || r.SizeBytes < 0 || r.SizeBytes > req.MaxTextBytes {
		return invalid
	}
	content := r.Content
	if req.Operation == "read_text" && strings.ContainsRune(content, 0) {
		return invalid
	}
	if req.Operation == "write_text" {
		if r.Content != "" {
			return invalid
		}
		content = req.Content
	}
	if !validTextResponse(content, r) {
		return invalid
	}
	return nil
}

// Stop of PID 1 destroys the private PID namespace, including setsid,
// double-forked children and zombies. Only a verified stopped OCI container is
// accepted; start uses the same ID, immutable PID 1 and existing volumes/layer.
func (m *Manager) reset(e *entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := m.runner.Run(ctx, "stop", "--time=0", e.container); err != nil {
		return err
	}
	c, err := m.inspect(ctx, e.container)
	if err != nil {
		return err
	}
	if c.State == nil || c.State.Running || (c.State.Status != "stopped" && c.State.Status != "exited" && c.State.Status != "created") {
		return errors.New("container did not stop")
	}
	if err := c.verify(m.imageID); err != nil {
		return err
	}
	if _, err := m.runner.Run(ctx, "start", e.container); err != nil {
		return err
	}
	c, err = m.inspect(ctx, e.container)
	if err != nil {
		return err
	}
	if err := c.verify(m.imageID); err != nil {
		return err
	}
	if c.State == nil || !c.State.Running || c.State.Status != "running" {
		return errors.New("container did not restart")
	}
	return nil
}
