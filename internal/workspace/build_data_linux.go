//go:build linux

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func (p *Podman) buildStorage(ctx context.Context, dataDir string) error {
	data, err := p.runner.run(ctx, "info", "--format=json")
	if err != nil {
		return err
	}
	var info struct{ Store struct{ GraphRoot string } }
	if json.Unmarshal(data, &info) != nil || !filepath.IsAbs(info.Store.GraphRoot) {
		return ErrStorageRecovery
	}
	var pool, graph unix.Stat_t
	if unix.Stat(dataDir, &pool) != nil || unix.Stat(info.Store.GraphRoot, &graph) != nil || pool.Dev != graph.Dev {
		return ErrStorageRecovery
	}
	return nil
}

// Only called after trusted backend terminal evidence and exact engine cleanup.
// openat2 pins every object without following symlinks or crossing any mount,
// including same-filesystem bind mounts. Permission repair is limited to owned
// directories reached from the exact generation root; files need no repair.
func removeBuildDirectory(dataDir string, e Exact) error {
	if e.validate() != nil {
		return ErrStaleGeneration
	}
	pool, err := unix.Open(dataDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(pool)
	root, err := unix.Openat2(pool, "builds", &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(root)
	var stat unix.Stat_t
	if unix.Fstat(root, &stat) != nil || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0077 != 0 {
		return errors.New("unverified build Data root")
	}
	return removeBuildEntry(root, objectName(e), 0)
}

func removeBuildEntry(parent int, name string, depth int) error {
	if depth > 128 {
		return errors.New("exact build Data nesting exceeds cleanup bound")
	}
	fd, err := unix.Openat2(parent, name, &unix.OpenHow{Flags: unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("unverified build Data owner")
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		if stat.Mode&unix.S_IFMT != unix.S_IFREG && stat.Mode&unix.S_IFMT != unix.S_IFLNK {
			return errors.New("unverified build Data type")
		}
		return unix.Unlinkat(parent, name, 0) // removes symlink itself, never its target
	}
	if stat.Mode&0700 != 0700 {
		if err := unix.Fchmodat(fd, "", stat.Mode&07777|0700, unix.AT_EMPTY_PATH); err != nil {
			return err
		}
	}
	readFD, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(readFD), name)
	defer dir.Close()
	for {
		entries, err := dir.ReadDir(64)
		if err != nil && err != io.EOF {
			return err
		}
		for _, entry := range entries {
			if err := removeBuildEntry(readFD, entry.Name(), depth+1); err != nil {
				return err
			}
		}
		if err == io.EOF {
			break
		}
	}
	return unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
}
