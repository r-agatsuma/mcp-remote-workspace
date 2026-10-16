package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
)

// A terminal transport failure and a lost build result are different facts.
// Stopping the parent later cannot reconstruct a canceled/lost exit result.
func buildCommandError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay) {
		return errors.Join(ErrOutcomeUnknown, err, ctx.Err())
	}
	return err
}

// Two streams share the existing 1 MiB aggregate retained-storage budget.
const retainedBuildBytes = 512 << 10

// os/exec connects non-file Writers through bounded OS pipes and continuously
// copies from them. Return success even after saturation/write failure so both
// pipes keep draining. The caller checks err after Wait has joined the drains.
type buildLog struct {
	mu       sync.Mutex
	file     *os.File
	retained int
	total    uint64
	err      error
}

func (l *buildLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if uint64(len(p)) > ^uint64(0)-l.total {
		l.total = ^uint64(0)
	} else {
		l.total += uint64(len(p))
	}
	if l.err == nil && l.retained < retainedBuildBytes {
		n := min(len(p), retainedBuildBytes-l.retained)
		written, err := l.file.Write(p[:n])
		l.retained += written
		l.err = err
		if written != n && err == nil {
			l.err = io.ErrShortWrite
		}
	}
	return len(p), nil
}
