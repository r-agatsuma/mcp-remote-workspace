package workspace

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxStreamBytes          = 256 * 1024
	DefaultMaxTextFileBytes = 1024 * 1024
	DefaultExecTimeout      = 60 * time.Second
	MaxExecTimeout          = 10 * time.Minute
)

var (
	ErrInvalidPath     = errors.New("path must be workspace-relative without traversal or symlinks")
	ErrInvalidArgument = errors.New("invalid execution argument")
	ErrSizeLimit       = errors.New("text file exceeds the configured size limit")
	ErrNonUTF8         = errors.New("file is binary or not valid UTF-8")
)

//go:embed helper.py
var operationHelper string

type Options struct {
	// Zero selects DefaultMaxTextFileBytes. Negative values are invalid.
	MaxTextFileBytes int64
}

type ExecRequest struct {
	Argv    []string          `json:"argv"`
	Cwd     string            `json:"cwd"`
	Env     map[string]string `json:"env"`
	Timeout time.Duration     `json:"-"`
}

type ExecResult struct {
	ExitCode        *int   `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	TimedOut        bool   `json:"timed_out"`
}

type TextResult struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type inputRunner interface {
	RunInput(context.Context, []byte, int64, ...string) ([]byte, error)
}

type helperRequest struct {
	Operation string `json:"operation"`
	ExecRequest
	Path             string  `json:"path"`
	Content          string  `json:"content"`
	MaxTextFileBytes int64   `json:"max_text_file_bytes"`
	MaxStreamBytes   int     `json:"max_stream_bytes"`
	TimeoutSeconds   float64 `json:"timeout_seconds"`
}

func normalizePath(value string, file bool) (string, error) {
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsRune(value, 0) || !utf8.ValidString(value) {
		return "", ErrInvalidPath
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", ErrInvalidPath
		}
	}
	value = path.Clean(value)
	if file && value == "." {
		return "", ErrInvalidPath
	}
	return value, nil
}

func (m *Manager) Exec(ctx context.Context, id string, in ExecRequest) (ExecResult, error) {
	if in.Cwd == "" {
		in.Cwd = "."
	}
	var err error
	if in.Cwd, err = normalizePath(in.Cwd, false); err != nil {
		return ExecResult{}, err
	}
	if len(in.Argv) == 0 || in.Argv[0] == "" {
		return ExecResult{}, ErrInvalidArgument
	}
	for _, arg := range in.Argv {
		if strings.ContainsRune(arg, 0) || !utf8.ValidString(arg) {
			return ExecResult{}, ErrInvalidArgument
		}
	}
	for key, value := range in.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) || !utf8.ValidString(key+value) {
			return ExecResult{}, ErrInvalidArgument
		}
	}
	if in.Timeout == 0 {
		in.Timeout = DefaultExecTimeout
	}
	if in.Timeout < 0 || in.Timeout > MaxExecTimeout {
		return ExecResult{}, ErrInvalidArgument
	}
	var out ExecResult
	err = m.operation(ctx, id, helperRequest{Operation: "exec", ExecRequest: in, MaxStreamBytes: MaxStreamBytes, TimeoutSeconds: in.Timeout.Seconds()}, in.Timeout+10*time.Second, 12*MaxStreamBytes+4096, &out)
	// Container output is untrusted. Enforce tool limits on the host as well as
	// in the helper, even if the container returns an unexpected result.
	out.Stdout, out.StdoutTruncated = boundStream(out.Stdout, out.StdoutTruncated)
	out.Stderr, out.StderrTruncated = boundStream(out.Stderr, out.StderrTruncated)
	return out, err
}

func boundStream(value string, truncated bool) (string, bool) {
	if len(value) <= MaxStreamBytes {
		return value, truncated
	}
	value = value[:MaxStreamBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}

func (m *Manager) WriteText(ctx context.Context, id, name, content string) (TextResult, error) {
	name, err := normalizePath(name, true)
	if err != nil {
		return TextResult{}, err
	}
	if !utf8.ValidString(content) {
		return TextResult{}, ErrNonUTF8
	}
	var out TextResult
	err = m.operation(ctx, id, helperRequest{Operation: "write_text", Path: name, Content: content}, 60*time.Second, 6*int64(len(name))+4096, &out)
	return out, err
}

func (m *Manager) ReadText(ctx context.Context, id, name string) (TextResult, error) {
	name, err := normalizePath(name, true)
	if err != nil {
		return TextResult{}, err
	}
	overhead := 6*int64(len(name)) + 4096
	if m.maxTextFileBytes > (1<<63-1-overhead)/6 {
		return TextResult{}, errors.New("maximum text-file size is too large for the response bound")
	}
	var out TextResult
	err = m.operation(ctx, id, helperRequest{Operation: "read_text", Path: name, MaxTextFileBytes: m.maxTextFileBytes}, 60*time.Second, 6*m.maxTextFileBytes+overhead, &out)
	if err == nil {
		if int64(len(out.Content)) > m.maxTextFileBytes || out.SizeBytes > m.maxTextFileBytes {
			return TextResult{}, ErrSizeLimit
		}
		if !utf8.ValidString(out.Content) || strings.ContainsRune(out.Content, 0) {
			return TextResult{}, ErrNonUTF8
		}
	}
	return out, err
}

func (m *Manager) operation(ctx context.Context, id string, in helperRequest, timeout time.Duration, limit int64, out any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if e == nil {
		return ErrNotFound
	}
	if e.container == "" {
		return errors.New("workspace container is unavailable")
	}
	// Reassert ownership, profile and running state before entering the container.
	c, err := m.inspect(ctx, e.container)
	if err != nil {
		return err
	}
	w, err := c.identity()
	if err != nil {
		return err
	}
	if w.ID != e.ID || !w.CreatedAt.Equal(e.CreatedAt) {
		return errors.New("workspace ownership labels disagree")
	}
	if err := c.verify(m.imageID); err != nil {
		return err
	}
	if c.State == nil || !c.State.Running {
		return errors.New("workspace container is not running")
	}
	runner, ok := m.runner.(inputRunner)
	if !ok {
		return errors.New("workspace runner does not support stdin operations")
	}
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err = runner.RunInput(ctx, data, limit, "exec", "--interactive", "--workdir=/", e.container, "/usr/bin/python3", "-I", "-c", operationHelper)
	if err != nil {
		return err
	}
	var envelope struct {
		Error  string          `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("workspace helper response: %w", err)
	}
	switch envelope.Error {
	case "":
		if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
			return errors.New("missing workspace helper result")
		}
		return json.Unmarshal(envelope.Result, out)
	case "invalid_path":
		return ErrInvalidPath
	case "non_utf8":
		return ErrNonUTF8
	case "size_limit":
		return ErrSizeLimit
	default:
		return errors.New("workspace operation failed")
	}
}
