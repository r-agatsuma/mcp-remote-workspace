package workspace

import (
	"os"
	"sync"
)

const retainedBuildBytes = 1 << 20

// os/exec connects non-file Writers through bounded OS pipes and continuously
// copies from them. Return success even after saturation/write failure so both
// pipes keep draining. The caller checks err after Wait has joined the drains.
type buildLog struct {
	mu       sync.Mutex
	file     *os.File
	retained int
	err      error
}

func (l *buildLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err == nil && l.retained < retainedBuildBytes {
		n := min(len(p), retainedBuildBytes-l.retained)
		written, err := l.file.Write(p[:n])
		l.retained += written
		l.err = err
	}
	return len(p), nil
}
