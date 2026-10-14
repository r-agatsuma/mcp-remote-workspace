//go:build linux

package workspace

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// removeBuildData runs only after exact backend absence and local worker/drain
// completion, under lifecycle serialization. There must be no live tree writer.
// Pin the trusted Data directory, then reject all symlinks and mount crossings
// (including same-filesystem bind mounts) on the way into and within exact G.
func (s *Service) removeBuildData(ctx context.Context, e Exact) (err error) {
	defer func() {
		if err != nil {
			err = errors.Join(ErrOutcomeUnknown, err)
		}
	}()
	if err := e.validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := unix.Openat2(unix.AT_FDCWD, s.dataDir, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return err
	}
	defer unix.Close(data)
	if _, err := buildCleanupStat(data, true); err != nil {
		return err
	}
	parent, err := openBuildCleanupEntry(data, "builds")
	if errors.Is(err, unix.ENOENT) {
		return nil // Preparation may have failed before builds/ was allocated.
	}
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	if _, err := buildCleanupStat(parent, true); err != nil {
		return err
	}
	name := objectName(e)
	if err := removeBuildEntry(ctx, parent, name, true, 0); err != nil {
		return err
	}
	// Independently read back the exact entry through its pinned parent. A
	// replacement of any type, including a symlink/mount, prevents ABSENT.
	fd, err := openBuildCleanupEntry(parent, name)
	if err == nil {
		unix.Close(fd)
		return ErrOutcomeUnknown
	}
	if !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

func openBuildCleanupEntry(parent int, name string) (int, error) {
	return unix.Openat2(parent, name, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
}

func buildCleanupStat(fd int, privateDir bool) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return st, err
	}
	if st.Uid != uint32(os.Geteuid()) ||
		privateDir && (st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0077 != 0) {
		return st, ErrOutcomeUnknown
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
	case unix.S_IFREG:
		if st.Nlink != 1 {
			return st, ErrOutcomeUnknown
		}
	default:
		return st, ErrOutcomeUnknown
	}
	return st, nil
}

func removeBuildEntry(ctx context.Context, parent int, name string, root bool, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Bound traversal stack and open descriptors, independently of tree size.
	if depth > 128 {
		return ErrOutcomeUnknown
	}
	fd, err := openBuildCleanupEntry(parent, name)
	if root && errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	st, err := buildCleanupStat(fd, root)
	if err != nil {
		return err
	}
	flags := 0
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		// Add only missing owner read/write/search bits to a pinned, verified
		// directory. Files and group/other permissions are never chmod'ed.
		// AT_EMPTY_PATH requires fchmodat2; unsupported kernels fail closed.
		if st.Mode&0700 != 0700 {
			if err := unix.Fchmodat(fd, "", st.Mode&07777|0700, unix.AT_EMPTY_PATH); err != nil {
				return err
			}
		}
		if err := removeBuildChildren(ctx, fd, depth); err != nil {
			return err
		}
		flags = unix.AT_REMOVEDIR
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A changed entry is not deletion authority. Terminal handling is required
	// above: descriptor-relative traversal cannot authorize concurrent writers.
	current, err := openBuildCleanupEntry(parent, name)
	if err != nil {
		return err
	}
	actual, statErr := buildCleanupStat(current, root)
	unix.Close(current)
	if statErr != nil || actual.Dev != st.Dev || actual.Ino != st.Ino {
		return ErrOutcomeUnknown
	}
	return unix.Unlinkat(parent, name, flags)
}

func removeBuildChildren(ctx context.Context, dir int, depth int) error {
	fd, err := unix.Openat2(dir, ".", &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "build cleanup directory")
	defer f.Close()
	for {
		// Do not retain a complete directory inventory in memory.
		entries, readErr := f.ReadDir(64)
		for _, entry := range entries {
			if err := removeBuildEntry(ctx, dir, entry.Name(), false, depth+1); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
