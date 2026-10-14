package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Build logs retain a fixed prefix per stream. Only the service writes these
// files; the subprocess receives os/exec's bounded, continuously drained pipes.
// No output slice or growing byte counter is retained in process memory.
const buildLogBytes = 64 << 10

type buildLog struct {
	mu       sync.Mutex
	file     *os.File
	retained int
	err      error
	closed   bool
	cancel   context.CancelFunc
}

func openBuildLog(path string, cancel context.CancelFunc) (*buildLog, error) {
	// The parent is a newly allocated private exact-generation directory.
	// Refuse existing files (including symlinks) rather than reopening log paths.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	return &buildLog{file: f, cancel: cancel}, nil
}

func (l *buildLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, os.ErrClosed
	}
	if n := min(len(p), buildLogBytes-l.retained); n > 0 && l.err == nil {
		written, err := l.file.Write(p[:n])
		l.retained += written
		if err == nil && written != n {
			err = io.ErrShortWrite
		}
		if err != nil {
			// Revoke successful completion and stop the live build, but continue
			// consuming its pipes while cancellation/terminal handling runs.
			l.err = err
			l.cancel()
		}
	}
	// Saturation is normal: discard excess without allocation, another backend
	// operation, a short write, or stopping the subprocess's drain loop.
	return len(p), nil
}

func (l *buildLog) finish() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		l.err = errors.Join(l.err, l.file.Sync(), l.file.Close())
		if l.err != nil {
			l.cancel()
		}
	}
	return l.err
}

type buildLogs struct{ stdout, stderr *buildLog }

func prepareBuildLogs(path string, cancel context.CancelFunc) (*buildLogs, error) {
	out, err := openBuildLog(filepath.Join(path, "stdout"), cancel)
	if err != nil {
		return nil, err
	}
	diagnostic, err := openBuildLog(filepath.Join(path, "stderr"), cancel)
	if err != nil {
		return nil, errors.Join(err, out.finish())
	}
	return &buildLogs{out, diagnostic}, nil
}

func (l *buildLogs) finish() error {
	// Both streams reach terminal handling even if one failed. Closing the
	// original descriptors also fences any stale writer; no path is reopened.
	return errors.Join(l.stdout.finish(), l.stderr.finish())
}
