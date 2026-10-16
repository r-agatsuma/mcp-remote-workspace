//go:build !linux

package workspace

func kernelBootID() (string, error) { return "", ErrNotReady }
