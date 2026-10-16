//go:build linux

package workspace

import (
	"io"
	"os"
	"strings"
	"syscall"
)

func kernelBootID() (string, error) {
	f, err := os.Open("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", ErrNotReady
	}
	defer f.Close()
	var fs syscall.Statfs_t
	if syscall.Fstatfs(int(f.Fd()), &fs) != nil || fs.Type != 0x9fa0 {
		return "", ErrNotReady
	}
	data, err := io.ReadAll(io.LimitReader(f, 38))
	id := strings.TrimSuffix(string(data), "\n")
	if err != nil || !validBootID(id) {
		return "", ErrNotReady
	}
	return id, nil
}
