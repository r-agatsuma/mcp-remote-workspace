package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Serve handles exactly one request. The host bounds the transport and resets
// the process namespace before accepting another operation, even on success.
func Serve(root string, input io.Reader, output io.Writer) error {
	var req Request
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("expected one request")
	}
	if req.MaxTextBytes < 1 || req.MaxTextBytes > 64*1024*1024 {
		return errors.New("invalid text limit")
	}
	var response Response
	var err error
	switch req.Operation {
	case "exec":
		response.Exec, err = execute(root, req)
	case "read_text":
		response.Text, err = readText(root, req)
	case "write_text":
		response.Text, err = writeText(root, req)
	default:
		err = &Error{"invalid_argument", "unknown operation"}
	}
	if err != nil {
		var domain *Error
		if !errors.As(err, &domain) {
			domain = &Error{"backend_error", "control operation failed"}
		}
		response = Response{Error: domain}
	}
	return json.NewEncoder(output).Encode(response)
}

type boundedStream struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *boundedStream) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := StreamLimit - len(b.data)
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *boundedStream) result() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value := strings.ToValidUTF8(string(b.data), "�")
	truncated := b.truncated
	if len(value) > StreamLimit {
		value = value[:StreamLimit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
		truncated = true
	}
	return value, truncated
}

func execute(root string, req Request) (*ExecResult, error) {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return nil, &Error{"invalid_argument", "argv must contain an executable"}
	}
	for _, arg := range req.Argv {
		if strings.ContainsRune(arg, 0) {
			return nil, &Error{"invalid_argument", "argv cannot contain NUL"}
		}
	}
	if req.TimeoutSeconds < 1 || req.TimeoutSeconds > MaxTimeoutSeconds {
		return nil, &Error{"invalid_argument", "timeout is outside backend limits"}
	}
	cwd, err := Normalize(req.Cwd, false)
	if err != nil {
		return nil, err
	}
	dir, err := openDirectory(root, cwd, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	// Each helper is a separate process; changing its cwd cannot affect the
	// daemon or any other operation. Holding the descriptor avoids path re-walks.
	if err := unix.Fchdir(int(dir.Fd())); err != nil {
		return nil, err
	}
	environment := map[string]string{"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME": "/root", "LANG": "C.UTF-8", "HOSTNAME": "mcp-workspace"}
	for key, value := range req.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			return nil, &Error{"invalid_argument", "invalid environment override"}
		}
		environment[key] = value
	}
	program := req.Argv[0]
	if !strings.ContainsRune(program, '/') {
		found := false
		for _, directory := range filepath.SplitList(environment["PATH"]) {
			candidate := filepath.Join(directory, program)
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				program = candidate
				// An empty/relative PATH entry is explicitly requested by the caller.
				if !filepath.IsAbs(program) {
					program = "./" + program
				}
				found = true
				break
			}
		}
		if !found {
			return nil, &Error{"invalid_argument", "executable not found"}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(req.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, req.Argv[1:]...)
	cmd.Args[0] = req.Argv[0]
	for key, value := range environment {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// This only accelerates ordinary cleanup. Escaped descendants are handled
	// by the mandatory host stop/start, never by a process-group assumption.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var stdout, stderr boundedStream
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	result := &ExecResult{TimedOut: ctx.Err() != nil}
	result.Stdout, result.StdoutTruncated = stdout.result()
	result.Stderr, result.StderrTruncated = stderr.result()
	if result.TimedOut {
		return result, nil
	}
	if cmd.ProcessState == nil {
		return nil, &Error{"backend_error", "could not start executable"}
	}
	code := cmd.ProcessState.ExitCode()
	result.ExitCode = &code
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) && !errors.Is(err, exec.ErrWaitDelay) {
		return nil, err
	}
	return result, nil
}

// Init is the immutable PID 1. Reap adopted descendants until Podman kills the
// namespace on stop; trusted recovery never executes a mutable rootfs program.
func Init() error {
	if os.Getpid() != 1 {
		return errors.New("init must be container PID 1")
	}
	for {
		var status syscall.WaitStatus
		_, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if err != nil && err != syscall.ECHILD && err != syscall.EINTR {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
