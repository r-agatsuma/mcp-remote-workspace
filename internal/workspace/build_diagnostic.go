package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Only bounded, generation-owned build output is allowed in this error. The
// ordinary error chain is never serialized. Exact remains host-only.
type BuildDiagnostic struct {
	Stdout     StreamResult `json:"stdout"`
	Stderr     StreamResult `json:"stderr"`
	ExitStatus *int         `json:"exit_status,omitempty"`
	Available  bool         `json:"available"`
}

type buildFailure struct {
	exact      Exact
	diagnostic BuildDiagnostic
	cause      error
}

func (e *buildFailure) Error() string { return ErrBuildFailed.Error() }
func (e *buildFailure) Unwrap() error { return errors.Join(ErrBuildFailed, e.cause) }

// Small operational metadata accompanies the two ordinary Data log files. It
// is diagnostic only: neither these totals nor exit status attest terminality.
type buildLogResult struct {
	Exact       Exact  `json:"exact"`
	StdoutBytes uint64 `json:"stdout_bytes"`
	StderrBytes uint64 `json:"stderr_bytes"`
	ExitStatus  *int   `json:"exit_status,omitempty"`
}

// Engine errors and untrusted RUN output share these streams. Their absolute
// paths cannot reliably be classified as host-side versus image-local, so the
// public failure view conservatively redacts both (including quoted/flag paths).
// This operates only on the fixed retained prefix, never all emitted output.
var diagnosticPath = regexp.MustCompile("(^|[[:space:]\"'`=(:,\\[{])(/[^[:space:]\"'`,;)\\]}]*)")

func finishBuildLogs(directory string, e Exact, stdout, stderr *buildLog, exitStatus *int) error {
	// The worker has joined both drains before this is called. Complete both
	// streams even when one failed, then durably publish diagnostic metadata.
	err := errors.Join(stdout.err, stderr.err, stdout.file.Sync(), stderr.file.Sync(), stdout.file.Close(), stderr.file.Close())
	data, _ := json.Marshal(buildLogResult{Exact: e, StdoutBytes: stdout.total, StderrBytes: stderr.total, ExitStatus: exitStatus})
	f, openErr := os.OpenFile(filepath.Join(directory, "result.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if openErr != nil {
		return errors.Join(err, openErr)
	}
	_, writeErr := f.Write(data)
	err = errors.Join(err, writeErr, f.Sync(), f.Close())
	dir, openErr := os.Open(directory)
	if openErr != nil {
		return errors.Join(err, openErr)
	}
	return errors.Join(err, dir.Sync(), dir.Close())
}

func boundedBuildFile(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ownedByService(info) || info.Size() > limit {
		return nil, ErrNotReady
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, ErrNotReady
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, ErrNotReady
	}
	return data, nil
}

func (s *Service) buildDiagnostic(ctx context.Context, e Exact) BuildDiagnostic {
	var result BuildDiagnostic
	if ctx.Err() != nil || s.entries[e.Workspace] != e || s.lifetimes[e].Build == nil {
		return result
	}
	// No caller path and no symlinked generation root. Runtime cleanup cannot
	// race this read: the failure worker is terminal and lifecycle lock is held.
	directory := buildDirectory(s.dataDir, e)
	for _, dir := range []string{filepath.Join(s.dataDir, "builds"), directory} {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ownedByService(info) {
			return result
		}
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return result
	}
	defer root.Close()
	metadata := buildLogResult{Exact: e}
	if data, err := boundedBuildFile(root, "result.json", 2048); err == nil {
		if json.Unmarshal(data, &metadata) != nil || metadata.Exact != e {
			return result
		}
	}
	if metadata.ExitStatus != nil && *metadata.ExitStatus >= 0 && *metadata.ExitStatus <= 255 {
		result.ExitStatus = metadata.ExitStatus
	}
	streams := []struct {
		name   string
		total  uint64
		output *StreamResult
	}{
		{"stdout.log", metadata.StdoutBytes, &result.Stdout}, {"stderr.log", metadata.StderrBytes, &result.Stderr},
	}
	for _, stream := range streams {
		data, err := boundedBuildFile(root, stream.name, retainedBuildBytes)
		if err != nil {
			continue
		}
		// Redact service paths from runtime-emitted diagnostics. Arbitrary Go
		// errors and command arguments are never attached to the public result.
		text := string(data)
		for _, private := range []string{directory, s.dataDir, filepath.Dir(s.path)} {
			if private != "" && private != "/" {
				text = strings.ReplaceAll(text, private, "[service data]")
			}
		}
		text = diagnosticPath.ReplaceAllString(text, "${1}[path]")
		capture := newStreamCapture(MaxStreamBytes)
		_, _ = capture.Write([]byte(text))
		*stream.output = capture.result()
		stream.output.TotalBytes = max(uint64(len(data)), stream.total)
		stream.output.Truncated = stream.output.Truncated || stream.output.TotalBytes > uint64(len(data))
		result.Available = true
	}
	return result
}
