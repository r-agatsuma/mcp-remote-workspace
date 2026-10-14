//go:build linux

package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func buildUnit(e Exact) string { return "mcpws" + string(e.Workspace) + string(e.Generation) }

func buildParentPath(e Exact) string {
	uid := strconv.Itoa(os.Geteuid())
	return "/user.slice/user-" + uid + ".slice/user@" + uid + ".service/" + buildUnit(e) + ".slice"
}

func buildParentProperties(policy Policy) []string {
	properties := []string{"TasksMax=" + strconv.FormatUint(policy.PIDsMax, 10),
		"MemoryMax=" + strconv.FormatInt(policy.MemoryBytes, 10), "MemorySwapMax=" + strconv.FormatInt(policy.SwapBytes, 10),
		"CPUWeight=" + strconv.FormatUint(policy.CPUWeight, 10)}
	if policy.CPUQuota != -1 {
		properties = append(properties, fmt.Sprintf("CPUQuota=%g%%", float64(policy.CPUQuota)/1000))
	}
	return properties
}

func (p *Podman) startBuildParent(ctx context.Context, e Exact) (string, error) {
	policy, slice := p.resourcePolicy(), buildUnit(e)+".slice"
	args := append([]string{"--user", "set-property", "--runtime", slice}, buildParentProperties(policy)...)
	if _, err := runHostCommand(ctx, "/usr/bin/systemctl", args...); err != nil {
		return "", err
	}
	// An empty oneshot anchor keeps the transient exact slice alive. All image
	// fetch/build processes and Buildah RUN scopes are children of this parent;
	// daemon, drain and recovery processes stay outside it.
	if _, err := runHostCommand(ctx, "/usr/bin/systemd-run", "--user", "--unit="+buildUnit(e)+"-hold.service",
		"--slice="+slice, "--property=Type=oneshot", "--remain-after-exit", "--", "/usr/bin/true"); err != nil {
		return "", err
	}
	data, err := runHostCommand(ctx, "/usr/bin/systemctl", "--user", "show", slice, "--property=ControlGroup", "--value")
	if err != nil || strings.TrimSpace(string(data)) != buildParentPath(e) {
		return "", ErrProcessUnsafe
	}
	root, err := os.OpenRoot("/sys/fs/cgroup")
	if err != nil {
		return "", err
	}
	defer root.Close()
	checks := map[string]string{"pids.max": strconv.FormatUint(policy.PIDsMax, 10),
		"memory.max": strconv.FormatInt(policy.MemoryBytes, 10), "memory.swap.max": strconv.FormatInt(policy.SwapBytes, 10),
		"cpu.weight": strconv.FormatUint(policy.CPUWeight, 10)}
	for file, want := range checks {
		f, err := root.Open(strings.TrimPrefix(buildParentPath(e), "/") + "/" + file)
		if err != nil {
			return "", err
		}
		var fs unix.Statfs_t
		fsErr := unix.Fstatfs(int(f.Fd()), &fs)
		var buf [128]byte
		n, readErr := f.Read(buf[:])
		f.Close()
		if fsErr != nil || fs.Type != unix.CGROUP2_SUPER_MAGIC || readErr != nil || n == len(buf) || strings.TrimSpace(string(buf[:n])) != want {
			return "", ErrProcessUnsafe
		}
	}
	// OCI's systemd cgroupsPath names the exact slice and a generation-specific
	// scope. --cgroup-manager=systemd ensures RUN scopes stay below this slice.
	return slice + ":mcpws:" + buildUnit(e), nil
}

func buildUnitCommand(ctx context.Context, e Exact, suffix, directory string, policy Policy, executable string, args ...string) *exec.Cmd {
	options := []string{"--user", "--quiet", "--pipe", "--wait", "--collect",
		"--unit=" + buildUnit(e) + "-" + suffix + ".service", "--slice=" + buildUnit(e) + ".slice",
		"--property=Type=exec", "--property=KillMode=control-group", "--property=TimeoutStopSec=5s",
		"--property=LimitNOFILE=" + strconv.FormatUint(policy.NoFile, 10),
		"--setenv=TMPDIR=" + filepath.Join(directory, "tmp"), "--", executable}
	return hostCommand(ctx, "/usr/bin/systemd-run", append(options, args...)...)
}

func (p *Podman) stopBuildParent(ctx context.Context, e Exact) error {
	slice := buildUnit(e) + ".slice"
	if _, err := runHostCommand(ctx, "/usr/bin/systemctl", "--user", "stop", slice); err != nil {
		return err
	}
	if _, err := runHostCommand(ctx, "/usr/bin/systemctl", "--user", "revert", slice); err != nil {
		return err
	}
	// A successful CLI request is insufficient: verify the entire exact kernel
	// subtree is gone, including every RUN scope and worker service.
	if _, err := os.Lstat("/sys/fs/cgroup" + buildParentPath(e)); !errors.Is(err, os.ErrNotExist) {
		return ErrOutcomeUnknown
	}
	return nil
}
