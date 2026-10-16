//go:build linux

package workspace

import (
	"context"
	"errors"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func libpodScopePath(id string) string {
	uid := strconv.Itoa(os.Geteuid())
	return "/user.slice/user-" + uid + ".slice/user@" + uid + ".service/user.slice/libpod-" + id + ".scope"
}

func (p *Podman) recoverPressureScope(ctx context.Context, e Exact, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	before, err := p.probeProcess(ctx, e, true)
	if err != nil || before.ObjectID != id || before.Tasks < p.resourcePolicy().PIDsMax || before.CgroupPath != libpodScopePath(id) {
		return ErrProcessUnsafe
	}
	// Pin the attested scope inode across manager validation, revalidation and
	// kill. No cached path, PID, name prefix or whole-user target is accepted.
	openRoots := p.processRoots
	if openRoots == nil {
		openRoots = openHostProcessRoots
	}
	proc, hierarchy, err := openRoots()
	if err != nil {
		return err
	}
	defer proc.Close()
	defer hierarchy.Close()
	relative := strings.TrimPrefix(before.CgroupPath, "/")
	scope, err := hierarchy.OpenRoot(relative)
	if err != nil {
		return err
	}
	defer scope.Close()
	info, err := scope.Stat(".")
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino != before.CgroupID || stat.Nlink == 0 {
		return ErrProcessUnsafe
	}
	unit := path.Base(before.CgroupPath)
	data, err := p.host(ctx, "/usr/bin/systemctl", "--user", "show", "--property=ControlGroup", "--value", "--", unit)
	if err != nil || strings.TrimSpace(string(data)) != before.CgroupPath {
		return ErrProcessUnsafe
	}
	after, err := p.probeProcess(ctx, e, true)
	if err != nil {
		return err
	}
	// Counts may change; ownership, process identity and all limits may not.
	if after.Tasks < p.resourcePolicy().PIDsMax {
		return ErrProcessUnsafe
	}
	after.Tasks = before.Tasks
	if after != before {
		return ErrProcessUnsafe
	}
	current, err := hierarchy.Stat(relative)
	if err != nil || !os.SameFile(info, current) {
		return ErrProcessUnsafe
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := p.host(ctx, "/usr/bin/systemctl", "--user", "kill", "--kill-whom=all", "--signal=SIGKILL", "--", unit); err != nil {
		return err
	}
	// Command success is not termination. The pinned subtree must become empty
	// or be removed before normal exact runtime removal can be retried.
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := hierarchy.Stat(relative)
		if errors.Is(err, os.ErrNotExist) {
			// Path disappearance alone could be a rename. The pinned kernel
			// cgroup itself must have been unlinked, which requires an empty
			// subtree; a still-linked old inode is not terminal evidence.
			removed, statErr := scope.Stat(".")
			if statErr != nil || !os.SameFile(info, removed) {
				return ErrProcessUnsafe
			}
			inode, ok := removed.Sys().(*syscall.Stat_t)
			if !ok || inode.Nlink != 0 {
				return ErrProcessUnsafe
			}
			return nil
		}
		if err != nil || !os.SameFile(info, current) {
			return ErrProcessUnsafe
		}
		events, err := readProcessFile(ctx, scope, "cgroup.events", 1024)
		if err != nil {
			return err
		}
		fields := strings.Fields(events)
		values := map[string]string{}
		if len(fields)%2 != 0 {
			return ErrProcessUnsafe
		}
		for i := 0; i < len(fields); i += 2 {
			if _, duplicate := values[fields[i]]; duplicate {
				return ErrProcessUnsafe
			}
			values[fields[i]] = fields[i+1]
		}
		if values["populated"] != "0" && values["populated"] != "1" {
			return ErrProcessUnsafe
		}
		tasks, err := readCgroupNumber(ctx, scope, "pids.current", false)
		if err != nil {
			return err
		}
		if values["populated"] == "0" && tasks == 0 {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
