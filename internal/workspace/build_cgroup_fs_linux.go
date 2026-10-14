//go:build linux

package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// All access is read-only under verified host procfs/cgroup-v2 roots. No cgroup
// file write, process migration, signalling or recursive cgroup deletion exists.
var errBuildGroupAbsent = errors.New("exact build cgroup absent")

type hostBuildGroupFS struct {
	openRoots func() (*os.Root, *os.Root, error)
}

func (fs hostBuildGroupFS) open(ctx context.Context, unit string) (*os.File, error) {
	proc, hierarchy, err := fs.openRoots()
	if err != nil {
		return nil, err
	}
	defer proc.Close()
	defer hierarchy.Close()
	membership, err := readProcessFile(ctx, proc, "self/cgroup", 16384)
	if err != nil {
		return nil, err
	}
	group, ok := strings.CutPrefix(strings.TrimSuffix(membership, "\n"), "0::")
	if !ok || !filepath.IsAbs(group) || filepath.Clean(group) != group || strings.ContainsAny(group, "\n\x00") {
		return nil, ErrOutcomeUnknown
	}
	target := buildGroupPath(unit)
	if unit != "" && (group == target || strings.HasPrefix(group, target+"/")) {
		return nil, ErrOutcomeUnknown
	}
	root, err := hierarchy.Open(".")
	if err != nil {
		return nil, err
	}
	defer root.Close()
	fd, err := unix.Openat2(int(root.Fd()), strings.TrimPrefix(target, "/"), &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
	if errors.Is(err, unix.ENOENT) && unit != "" {
		return nil, errBuildGroupAbsent
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "build cgroup"), nil
}

func (fs hostBuildGroupFS) Sample(ctx context.Context, unit string) (buildGroupSample, error) {
	var result buildGroupSample
	if _, err := buildSliceExact(unit); err != nil {
		return result, err
	}
	f, err := fs.open(ctx, unit)
	if err == errBuildGroupAbsent {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return result, err
	}
	result.Exists, result.Device, result.Inode = true, uint64(st.Dev), st.Ino
	read := func(name string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		fd, err := unix.Openat2(int(f.Fd()), name, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
		if err != nil {
			return "", err
		}
		entry := os.NewFile(uintptr(fd), name)
		defer entry.Close()
		b, err := io.ReadAll(io.LimitReader(entry, 1025))
		if err != nil || len(b) > 1024 {
			return "", ErrOutcomeUnknown
		}
		return strings.TrimSpace(string(b)), nil
	}
	kind, err := read("cgroup.type")
	if err != nil || kind != "domain" {
		return result, ErrOutcomeUnknown
	}
	limit, err := read("pids.max")
	if err != nil {
		return result, err
	}
	result.Limit, err = strconv.ParseUint(limit, 10, 64)
	if err != nil || result.Limit == 0 || result.Limit == ^uint64(0) {
		return result, ErrOutcomeUnknown
	}
	tasks, err := read("pids.current")
	if err != nil {
		return result, err
	}
	result.Tasks, err = strconv.ParseUint(tasks, 10, 64)
	if err != nil {
		return result, err
	}
	events, err := read("cgroup.events")
	if err != nil {
		return result, err
	}
	fields := strings.Fields(events)
	if len(fields) != 4 || fields[0] != "populated" || (fields[1] != "0" && fields[1] != "1") || fields[2] != "frozen" || fields[3] != "0" {
		return result, ErrOutcomeUnknown
	}
	result.Populated = fields[1] == "1"
	return result, ctx.Err()
}

func (fs hostBuildGroupFS) Units(ctx context.Context) ([]string, error) {
	f, err := fs.open(ctx, "")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	units := []string{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := f.ReadDir(64)
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), buildSlicePrefix) {
				continue
			}
			if _, parseErr := buildSliceExact(entry.Name()); parseErr != nil || !entry.IsDir() || len(units) >= 4096 {
				return nil, ErrOutcomeUnknown
			}
			units = append(units, entry.Name())
		}
		if err == io.EOF {
			return units, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
