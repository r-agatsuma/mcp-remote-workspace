//go:build linux

package workspace

import (
	"context"
	"errors"
	"io"
	"math/big"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var _ ProcessProbe = (*Podman)(nil)

// ProbeProcess is read-only and synchronous. Errors attest that no injection
// started, so the existing gate can release its reservation without recovery.
func (p *Podman) ProbeProcess(ctx context.Context, e Exact) (snapshot ProcessSnapshot, err error) {
	return p.probeProcess(ctx, e, false)
}

// Recovery uses the same identity, membership, inode and limit attestation as
// injection. Only the full-ceiling admission refusal is permitted there.
func (p *Podman) probeProcess(ctx context.Context, e Exact, pressure bool) (snapshot ProcessSnapshot, err error) {
	defer func() {
		if err != nil {
			snapshot = ProcessSnapshot{}
			err = CompletedBackendError(errors.Join(ErrProcessUnsafe, err, ctx.Err()))
		}
	}()
	if !p.initialized || e.validate() != nil {
		return snapshot, ErrProcessUnsafe
	}
	objects, err := p.List(ctx)
	if err != nil {
		return snapshot, err
	}
	id := ""
	for _, o := range objects {
		actual, owned, classifyErr := classify(o)
		if classifyErr != nil {
			return snapshot, classifyErr
		}
		if owned && actual == e {
			if id != "" {
				return snapshot, ErrProcessUnsafe
			}
			id = o.ID
		}
	}
	if id == "" {
		return snapshot, ErrProcessUnsafe
	}
	before, err := p.inspectProcess(ctx, e, id)
	if err != nil {
		return snapshot, err
	}
	openRoots := p.processRoots
	if openRoots == nil {
		openRoots = openHostProcessRoots
	}
	proc, cgroups, err := openRoots()
	if err != nil {
		return snapshot, err
	}
	defer proc.Close()
	defer cgroups.Close()
	pid := strconv.Itoa(before.State.Pid)
	process, err := proc.OpenRoot(pid)
	if err != nil {
		return snapshot, err
	}
	defer process.Close()
	start, group, soft, hard, err := processIdentity(ctx, process, before.State.Pid)
	if err != nil {
		return snapshot, err
	}
	resourceGroup := before.State.CgroupPath
	if group != resourceGroup && !strings.HasPrefix(group, resourceGroup+"/") {
		return snapshot, ErrProcessUnsafe
	}
	member, err := cgroups.OpenRoot(strings.TrimPrefix(group, "/"))
	if err != nil {
		return snapshot, err
	}
	defer member.Close()
	memberInfo, err := member.Stat(".")
	if err != nil {
		return snapshot, err
	}
	if err := validateMembership(ctx, cgroups, member, memberInfo, group, before.State.Pid); err != nil {
		return snapshot, err
	}
	cgroup, err := cgroups.OpenRoot(strings.TrimPrefix(resourceGroup, "/"))
	if err != nil {
		return snapshot, err
	}
	defer cgroup.Close()
	info, err := cgroup.Stat(".")
	if err != nil {
		return snapshot, err
	}
	inode, ok := info.Sys().(*syscall.Stat_t)
	if !ok || inode.Ino == 0 || inode.Nlink == 0 {
		return snapshot, ErrProcessUnsafe
	}
	snapshot = ProcessSnapshot{Exact: e, ObjectID: id, PID: before.State.Pid,
		PIDStart: start, CgroupPath: resourceGroup, CgroupID: inode.Ino, NoFileSoft: soft, NoFileHard: hard}
	var firstLimits, secondLimits []cgroupLimit
	if err = sampleProcessCgroup(ctx, cgroups, cgroup, group, &snapshot, &firstLimits); err != nil {
		return snapshot, err
	}
	// Reinspect the immutable object, then reread the pinned process and current
	// PID path. PID recycling, exit/restart, membership migration, limit changes
	// and cgroup path replacement must not turn the first sample into authority.
	after, err := p.inspectProcess(ctx, e, id)
	if err != nil || before.State != after.State {
		return snapshot, ErrProcessUnsafe
	}
	start2, group2, soft2, hard2, err := processIdentity(ctx, process, snapshot.PID)
	if err != nil || start != start2 || group != group2 || soft != soft2 || hard != hard2 {
		return snapshot, ErrProcessUnsafe
	}
	current, err := proc.OpenRoot(pid)
	if err != nil {
		return snapshot, err
	}
	defer current.Close()
	start2, group2, soft2, hard2, err = processIdentity(ctx, current, snapshot.PID)
	if err != nil || start != start2 || group != group2 || soft != soft2 || hard != hard2 {
		return snapshot, ErrProcessUnsafe
	}
	second := snapshot
	if err = sampleProcessCgroup(ctx, cgroups, cgroup, group, &second, &secondLimits); err != nil {
		return snapshot, err
	}
	// Task activity can legitimately change; use the higher observed subtree
	// count. Every identity and limit must agree across both observations.
	tasks := max(snapshot.Tasks, second.Tasks)
	second.Tasks = snapshot.Tasks
	if snapshot != second || !slices.Equal(firstLimits, secondLimits) {
		return snapshot, ErrProcessUnsafe
	}
	if err := validateMembership(ctx, cgroups, member, memberInfo, group, snapshot.PID); err != nil {
		return snapshot, err
	}
	snapshot.Tasks = tasks
	start2, group2, soft2, hard2, err = processIdentity(ctx, current, snapshot.PID)
	if err != nil || start != start2 || group != group2 || soft != soft2 || hard != hard2 {
		return snapshot, ErrProcessUnsafe
	}
	if err = ctx.Err(); err != nil {
		return snapshot, err
	}
	err = p.resourcePolicy().validateProcess(e, snapshot)
	if pressure && errors.Is(err, ErrInjectionBusy) {
		err = nil
	}
	return snapshot, err
}

func (p *Podman) inspectProcess(ctx context.Context, e Exact, id string) (containerInspection, error) {
	v, err := p.inspectContainer(ctx, id)
	if err != nil {
		return v, err
	}
	actual, owned, err := classify(v.object())
	if err != nil || !owned || actual != e || (v.object().Image != p.image && !immutableImage(v.object().Image)) ||
		!v.State.Running || v.State.Status != "running" || v.State.Paused ||
		v.State.Restarting || v.State.Dead || v.State.Pid <= 0 || v.State.StartedAt == "" {
		return v, ErrProcessUnsafe
	}
	started, err := time.Parse(time.RFC3339Nano, v.State.StartedAt)
	if err != nil || started.IsZero() {
		return v, ErrProcessUnsafe
	}
	group := v.State.CgroupPath
	if validateContainerPath(group) != nil || group == "/" || path.Clean(group) != group ||
		len(group) > 16<<10 || strings.Count(group, "/") > 128 {
		return v, ErrProcessUnsafe
	}
	// Podman's generated resource-bearing group must name the full immutable
	// object. A stale PID pointing at an unrelated live process is insufficient.
	name := path.Base(group)
	if name != "libpod-"+id+".scope" && name != "libpod-"+id && name != "libpod-payload-"+id {
		return v, ErrProcessUnsafe
	}
	return v, nil
}

func validateMembership(ctx context.Context, hierarchy, member *os.Root, original os.FileInfo, group string, pid int) error {
	current, err := hierarchy.Stat(strings.TrimPrefix(group, "/"))
	if err != nil || !os.SameFile(original, current) {
		return ErrProcessUnsafe
	}
	pinned, err := member.Stat(".")
	if err != nil || !os.SameFile(original, pinned) {
		return ErrProcessUnsafe
	}
	members, err := readProcessFile(ctx, member, "cgroup.procs", 1<<20)
	if err != nil {
		return err
	}
	found := false
	for value := range strings.FieldsSeq(members) {
		task, err := strconv.ParseUint(value, 10, 64)
		if err != nil || task == 0 {
			return ErrProcessUnsafe
		}
		found = found || task == uint64(pid)
	}
	if !found {
		return ErrProcessUnsafe
	}
	return nil
}

func readProcessFile(ctx context.Context, root *os.Root, name string, limit int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return "", ErrProcessUnsafe
	}
	return string(data), ctx.Err()
}

func openHostProcessRoots() (*os.Root, *os.Root, error) {
	proc, err := os.OpenRoot("/proc")
	if err != nil {
		return nil, nil, err
	}
	cgroups, err := os.OpenRoot("/sys/fs/cgroup")
	if err != nil {
		proc.Close()
		return nil, nil, err
	}
	fail := func() (*os.Root, *os.Root, error) {
		proc.Close()
		cgroups.Close()
		return nil, nil, ErrProcessUnsafe
	}
	for _, fs := range []struct {
		root  *os.Root
		magic int64
	}{{proc, 0x9fa0}, {cgroups, 0x63677270}} { // procfs, cgroup2
		f, err := fs.root.Open(".")
		if err != nil {
			return fail()
		}
		var stat syscall.Statfs_t
		err = syscall.Fstatfs(int(f.Fd()), &stat)
		f.Close()
		if err != nil || int64(stat.Type) != fs.magic {
			return fail()
		}
	}
	// A subtree mount would conceal inherited limits. Support only the full
	// unified hierarchy at the host's fixed mount, without caller path inputs.
	mounts, err := readProcessFile(context.Background(), proc, "self/mountinfo", 1<<20)
	if err != nil {
		return fail()
	}
	if !fullCgroupMount(mounts) {
		return fail()
	}
	return proc, cgroups, nil
}

func fullCgroupMount(mounts string) bool {
	found := 0
	for line := range strings.SplitSeq(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 6 && fields[4] == "/sys/fs/cgroup" {
			_, suffix, ok := strings.Cut(line, " - ")
			if !ok || fields[3] != "/" || !strings.HasPrefix(suffix, "cgroup2 ") {
				return false
			}
			found++
		}
	}
	return found == 1
}

func processIdentity(ctx context.Context, process *os.Root, pid int) (uint64, string, uint64, uint64, error) {
	stat, err := readProcessFile(ctx, process, "stat", 16<<10)
	if err != nil {
		return 0, "", 0, 0, err
	}
	// comm may contain spaces and ')' characters; fields after its final ')'
	// start at field 3. starttime is field 22 in proc_pid_stat(5).
	open, close := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if open < 1 || close <= open || strings.TrimSpace(stat[:open]) != strconv.Itoa(pid) {
		return 0, "", 0, 0, ErrProcessUnsafe
	}
	fields := strings.Fields(stat[close+1:])
	if len(fields) < 20 || len(fields[0]) != 1 || !strings.Contains("RSDI", fields[0]) {
		return 0, "", 0, 0, ErrProcessUnsafe
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return 0, "", 0, 0, ErrProcessUnsafe
	}
	membership, err := readProcessFile(ctx, process, "cgroup", 16<<10)
	if err != nil {
		return 0, "", 0, 0, err
	}
	line := strings.TrimSuffix(membership, "\n")
	group, ok := strings.CutPrefix(line, "0::")
	if !ok || strings.ContainsAny(group, "\n\x00") || group == "/" ||
		!path.IsAbs(group) || path.Clean(group) != group || strings.Count(group, "/") > 128 {
		return 0, "", 0, 0, ErrProcessUnsafe
	}
	limits, err := readProcessFile(ctx, process, "limits", 16<<10)
	if err != nil {
		return 0, "", 0, 0, err
	}
	var soft, hard uint64
	found := false
	for line := range strings.SplitSeq(limits, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && strings.Join(fields[:3], " ") == "Max open files" {
			if found || len(fields) != 6 || fields[5] != "files" {
				return 0, "", 0, 0, ErrProcessUnsafe
			}
			found = true
			soft, err = strconv.ParseUint(fields[3], 10, 64)
			if err != nil {
				return 0, "", 0, 0, ErrProcessUnsafe
			}
			hard, err = strconv.ParseUint(fields[4], 10, 64)
			if err != nil {
				return 0, "", 0, 0, ErrProcessUnsafe
			}
		}
	}
	if !found || soft == 0 || hard == 0 {
		return 0, "", 0, 0, ErrProcessUnsafe
	}
	return start, group, soft, hard, nil
}

func readCgroupNumber(ctx context.Context, root *os.Root, name string, unlimited bool) (uint64, error) {
	data, err := readProcessFile(ctx, root, name, 128)
	if err != nil {
		return 0, err
	}
	data = strings.TrimSpace(data)
	if unlimited && data == "max" {
		return ^uint64(0), nil
	}
	v, err := strconv.ParseUint(data, 10, 64)
	if err != nil {
		return 0, ErrProcessUnsafe
	}
	return v, nil
}

// cpu.max expresses a quota per period. Normalize to Podman's default 100ms
// period rather than silently authorizing a changed CPU bandwidth ratio.
func readCPUQuota(ctx context.Context, root *os.Root) (int64, error) {
	data, err := readProcessFile(ctx, root, "cpu.max", 128)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(data)
	if len(fields) != 2 {
		return 0, ErrProcessUnsafe
	}
	period, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil || period < 1000 || period > 1000000 {
		return 0, ErrProcessUnsafe
	}
	if fields[0] == "max" {
		return -1, nil
	}
	quota, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil || quota == 0 {
		return 0, ErrProcessUnsafe
	}
	value := new(big.Int).Mul(new(big.Int).SetUint64(quota), big.NewInt(100000))
	remainder := new(big.Int)
	value.QuoRem(value, new(big.Int).SetUint64(period), remainder)
	if remainder.Sign() != 0 || !value.IsInt64() || value.Sign() <= 0 {
		return 0, ErrProcessUnsafe
	}
	return value.Int64(), nil
}

type cgroupLimit struct {
	path                              string
	device, inode, memory, swap, pids uint64
	quota                             int64
}

func sampleProcessCgroup(ctx context.Context, hierarchy, cgroup *os.Root, memberPath string, v *ProcessSnapshot, limits *[]cgroupLimit) error {
	current, err := hierarchy.Stat(strings.TrimPrefix(v.CgroupPath, "/"))
	if err != nil {
		return err
	}
	pinned, err := cgroup.Stat(".")
	if err != nil || !os.SameFile(current, pinned) {
		return ErrProcessUnsafe
	}
	stat, ok := pinned.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino != v.CgroupID || stat.Nlink == 0 {
		return ErrProcessUnsafe
	}
	kind, err := readProcessFile(ctx, cgroup, "cgroup.type", 128)
	if err != nil || strings.TrimSpace(kind) != "domain" {
		return ErrProcessUnsafe
	}
	events, err := readProcessFile(ctx, cgroup, "cgroup.events", 1024)
	if err != nil {
		return err
	}
	eventFields := strings.Fields(events)
	eventValues := map[string]string{}
	if len(eventFields)%2 != 0 {
		return ErrProcessUnsafe
	}
	for n := 0; n < len(eventFields); n += 2 {
		if _, duplicate := eventValues[eventFields[n]]; duplicate {
			return ErrProcessUnsafe
		}
		eventValues[eventFields[n]] = eventFields[n+1]
	}
	if eventValues["populated"] != "1" || eventValues["frozen"] != "0" {
		return ErrProcessUnsafe
	}
	v.Tasks, err = readCgroupNumber(ctx, cgroup, "pids.current", false)
	if err != nil || v.Tasks == 0 {
		return ErrProcessUnsafe
	}
	v.CPUWeight, err = readCgroupNumber(ctx, cgroup, "cpu.weight", false)
	if err != nil {
		return err
	}
	memory, swap, pids := ^uint64(0), ^uint64(0), ^uint64(0)
	quota := int64(-1)
	// Ancestors can impose tighter limits than the container's own settings.
	// The unified root has no resource limit files and is not sampled.
	for group := memberPath; group != "/"; group = path.Dir(group) {
		root, err := hierarchy.OpenRoot(strings.TrimPrefix(group, "/"))
		if err != nil {
			return err
		}
		info, err := root.Stat(".")
		if err != nil {
			root.Close()
			return err
		}
		identity, ok := info.Sys().(*syscall.Stat_t)
		if !ok || identity.Ino == 0 || identity.Nlink == 0 {
			root.Close()
			return ErrProcessUnsafe
		}
		m, mErr := readCgroupNumber(ctx, root, "memory.max", true)
		s, sErr := readCgroupNumber(ctx, root, "memory.swap.max", true)
		p, pErr := readCgroupNumber(ctx, root, "pids.max", true)
		q, qErr := readCPUQuota(ctx, root)
		root.Close()
		if err := errors.Join(mErr, sErr, pErr, qErr); err != nil {
			return err
		}
		*limits = append(*limits, cgroupLimit{group, identity.Dev, identity.Ino, m, s, p, q})
		memory, swap, pids = min(memory, m), min(swap, s), min(pids, p)
		if q != -1 && (quota == -1 || q < quota) {
			quota = q
		}
	}
	for _, limit := range *limits {
		info, err := hierarchy.Stat(strings.TrimPrefix(limit.path, "/"))
		if err != nil {
			return err
		}
		identity, ok := info.Sys().(*syscall.Stat_t)
		if !ok || identity.Dev != limit.device || identity.Ino != limit.inode || identity.Nlink == 0 {
			return ErrProcessUnsafe
		}
	}
	if memory > uint64(^uint64(0)>>1) || swap > uint64(^uint64(0)>>1) {
		return ErrProcessUnsafe
	}
	v.MemoryBytes, v.SwapBytes, v.PIDsMax, v.CPUQuota = int64(memory), int64(swap), pids, quota
	return nil
}
