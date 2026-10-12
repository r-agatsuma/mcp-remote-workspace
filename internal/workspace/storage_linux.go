//go:build linux

package workspace

import (
	"context"
	"errors"
	"syscall"
)

type localStorage struct{}

func (localStorage) Sample(ctx context.Context, path string) (StorageSample, error) {
	if err := ctx.Err(); err != nil {
		return StorageSample{}, err
	}
	var v syscall.Statfs_t
	if err := syscall.Statfs(path, &v); err != nil {
		return StorageSample{}, err
	}
	if v.Bsize <= 0 || v.Bavail > ^uint64(0)/uint64(v.Bsize) {
		return StorageSample{}, errors.New("invalid storage sample")
	}
	return StorageSample{v.Bavail * uint64(v.Bsize), v.Ffree}, nil
}
