//go:build !linux

package workspace

import (
	"context"
	"os"
)

func (*Podman) ProbeProcess(context.Context, Exact) (ProcessSnapshot, error) {
	return ProcessSnapshot{}, CompletedBackendError(ErrProcessUnsafe)
}

func openHostProcessRoots() (*os.Root, *os.Root, error) {
	return nil, nil, ErrProcessUnsafe
}
