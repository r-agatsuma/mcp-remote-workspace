//go:build linux

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const buildCgroupRecordFile = "build-cgroup.json"

type buildCgroupRecord struct {
	Exact Exact      `json:"exact"`
	Token Generation `json:"token"`
	Limit uint64     `json:"limit"`
}

func (r buildCgroupRecord) description() string {
	return "mcp-workspaced build " + objectName(r.Exact) + " " + string(r.Token)
}

type buildGroupSample struct {
	Exists        bool
	Limit, Tasks  uint64
	Populated     bool
	Device, Inode uint64
}

type buildGroupFS interface {
	Sample(context.Context, string) (buildGroupSample, error)
	Units(context.Context) ([]string, error)
}

type systemdBuildCgroups struct {
	dataDir      string
	bus, control commandRunner
	fs           buildGroupFS
}

func newBuildCgroups(dataDir string) buildCgroupManager {
	return &systemdBuildCgroups{dataDir: dataDir,
		bus: localBuildahCLI{"/usr/bin/busctl"}, control: localBuildahCLI{"/usr/bin/systemctl"},
		fs: hostBuildGroupFS{openRoots: openHostProcessRoots}}
}

func buildSliceExact(unit string) (Exact, error) {
	s, ok := strings.CutPrefix(unit, buildSlicePrefix)
	s, suffix := strings.CutSuffix(s, ".slice")
	if !ok || !suffix || len(s) != 128 {
		return Exact{}, ErrOutcomeUnknown
	}
	e := Exact{WorkspaceKey(s[:64]), Generation(s[64:])}
	if e.validate() != nil || unit != buildSlice(e) {
		return Exact{}, ErrOutcomeUnknown
	}
	return e, nil
}

func buildGroupPath(unit string) string {
	uid := strconv.Itoa(os.Geteuid())
	return "/user.slice/user-" + uid + ".slice/user@" + uid + ".service/" + unit
}

func (m *systemdBuildCgroups) recordPath(e Exact) string {
	return filepath.Join(m.dataDir, "builds", objectName(e), buildCgroupRecordFile)
}

// Pin Data and reject substituted metadata, links and mount crossings. The
// record is written/synced before StartTransientUnit, so an interrupted request
// remains attributable without adopting a pre-existing unit by name alone.
func (m *systemdBuildCgroups) record(e Exact) (buildCgroupRecord, error) {
	var r buildCgroupRecord
	if e.validate() != nil || !filepath.IsAbs(m.dataDir) {
		return r, ErrNotReady
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, m.dataDir, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return r, err
	}
	defer unix.Close(fd)
	if _, err := buildCleanupStat(fd, true); err != nil {
		return r, fmt.Errorf("Data ownership: %w", err)
	}
	for _, name := range []string{"builds", objectName(e)} {
		next, err := openBuildCleanupEntry(fd, name)
		if err != nil {
			return r, err
		}
		defer unix.Close(next)
		if _, err := buildCleanupStat(next, true); err != nil {
			return r, fmt.Errorf("%s ownership: %w", name, err)
		}
		fd = next
	}
	file, err := unix.Openat2(fd, buildCgroupRecordFile, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return r, err
	}
	f := os.NewFile(uintptr(file), "build cgroup record")
	defer f.Close()
	st, err := buildCleanupStat(file, false)
	if err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0077 != 0 || st.Size > 2048 {
		return r, fmt.Errorf("invalid cgroup record file: %w", ErrOutcomeUnknown)
	}
	d := json.NewDecoder(io.LimitReader(f, 2049))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF || r.Exact != e || !validHex(string(r.Token)) || r.Limit == 0 || r.Limit == ^uint64(0) {
		return r, fmt.Errorf("cgroup record decode: %w", ErrOutcomeUnknown)
	}
	return r, nil
}

func (m *systemdBuildCgroups) units(ctx context.Context) (map[string]bool, error) {
	data, err := m.control.run(ctx, "--user", "--no-pager", "--no-legend", "--plain", "--full", "list-units", "--all", "--type=slice")
	if err != nil {
		return nil, err
	}
	units := map[string]bool{}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 4 {
			return nil, ErrOutcomeUnknown
		}
		unit := fields[0]
		if !strings.HasPrefix(unit, buildSlicePrefix) {
			continue
		}
		if _, err := buildSliceExact(unit); err != nil || units[unit] || len(units) >= 4096 {
			return nil, ErrOutcomeUnknown
		}
		units[unit] = true
	}
	// A cgroup with no manager unit is ambiguous, even if it has no tasks.
	groups, err := m.fs.Units(ctx)
	if err != nil {
		return nil, err
	}
	for _, unit := range groups {
		if !units[unit] {
			return nil, ErrOutcomeUnknown
		}
	}
	return units, nil
}

func (m *systemdBuildCgroups) properties(ctx context.Context, r buildCgroupRecord) (string, error) {
	data, err := m.control.run(ctx, "--user", "--no-pager", "show", "--all", "--property=Id,LoadState,ActiveState,Transient,Description,ControlGroup,TasksMax", "--", buildSlice(r.Exact))
	if err != nil {
		return "", err
	}
	v := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		k, value, ok := strings.Cut(line, "=")
		if _, duplicate := v[k]; !ok || duplicate {
			return "", ErrOutcomeUnknown
		}
		v[k] = value
	}
	if len(v) != 7 || v["Id"] != buildSlice(r.Exact) || v["LoadState"] != "loaded" || v["Transient"] != "yes" || v["Description"] != r.description() || v["TasksMax"] != strconv.FormatUint(r.Limit, 10) {
		return "", ErrOutcomeUnknown
	}
	state := v["ActiveState"]
	if state != "active" && state != "activating" && state != "inactive" {
		return "", ErrOutcomeUnknown
	}
	if v["ControlGroup"] != buildGroupPath(buildSlice(r.Exact)) && !(state != "active" && v["ControlGroup"] == "") {
		return "", ErrOutcomeUnknown
	}
	return state, nil
}

func (m *systemdBuildCgroups) Prepare(ctx context.Context, e Exact, limit uint64) (string, error) {
	if e.validate() != nil || !filepath.IsAbs(m.dataDir) || limit == 0 || limit == ^uint64(0) {
		return "", ErrNotReady
	}
	units, err := m.units(ctx)
	if err != nil {
		return "", err
	}
	if units[buildSlice(e)] {
		return "", ErrOutcomeUnknown
	}
	sample, err := m.fs.Sample(ctx, buildSlice(e))
	if err != nil || sample.Exists {
		return "", errors.Join(ErrOutcomeUnknown, err)
	}
	token, err := freshGeneration()
	if err != nil {
		return "", err
	}
	r := buildCgroupRecord{e, token, limit}
	data, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(m.recordPath(e), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return "", err
	}
	dir, err := os.Open(filepath.Dir(m.recordPath(e)))
	if err != nil {
		return "", err
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		return "", err
	}
	if saved, err := m.record(e); err != nil || saved != r {
		return "", errors.Join(ErrOutcomeUnknown, err)
	}
	// No processes are attached or moved. Only RUN descendants enter via the
	// later OCI parent setting; the daemon, CLI and recovery stay outside.
	_, err = m.bus.run(ctx, "--user", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))", buildSlice(e), "fail", "3", "Description", "s", r.description(), "TasksMax", "t", strconv.FormatUint(limit, 10), "CollectMode", "s", "inactive-or-failed", "0")
	if err != nil {
		return "", errors.Join(ErrOutcomeUnknown, err)
	}
	// StartTransientUnit returns a job, not terminal start evidence. Only poll
	// observations, never retry submission or fall back to an unbounded build.
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		state, err := m.properties(wait, r)
		if err != nil {
			return "", errors.Join(ErrOutcomeUnknown, err)
		}
		if state == "active" {
			if err := m.Check(wait, e); err != nil {
				return "", errors.Join(ErrOutcomeUnknown, err)
			}
			return buildCgroupParent(e), nil
		}
		select {
		case <-wait.Done():
			return "", errors.Join(ErrOutcomeUnknown, wait.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (m *systemdBuildCgroups) Check(ctx context.Context, e Exact) error {
	r, err := m.record(e)
	if err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	first, err := m.fs.Sample(ctx, buildSlice(e))
	if err != nil {
		return err
	}
	state, err := m.properties(ctx, r)
	if err != nil {
		return err
	}
	second, err := m.fs.Sample(ctx, buildSlice(e))
	if err != nil {
		return err
	}
	if state != "active" || !first.Exists || first != second || first.Limit != r.Limit || first.Tasks != 0 || first.Populated {
		return ErrOutcomeUnknown
	}
	return nil
}

func (m *systemdBuildCgroups) Release(ctx context.Context, e Exact) error {
	if e.validate() != nil {
		return ErrStaleGeneration
	}
	units, err := m.units(ctx)
	if err != nil {
		return err
	}
	if !units[buildSlice(e)] {
		v, err := m.fs.Sample(ctx, buildSlice(e))
		if err != nil || v.Exists {
			return errors.Join(ErrOutcomeUnknown, err)
		}
		return nil // Includes preparation failures and a rebooted user manager.
	}
	if err := m.Check(ctx, e); err != nil {
		return err
	}
	// A populated slice is never stopped as a substitute for backend recovery.
	if _, err := m.control.run(ctx, "--user", "--no-pager", "--no-ask-password", "stop", "--", buildSlice(e)); err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	units, err = m.units(ctx)
	if err != nil {
		return err
	}
	v, err := m.fs.Sample(ctx, buildSlice(e))
	if err != nil || v.Exists || units[buildSlice(e)] {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	return nil
}

func (m *systemdBuildCgroups) Generations(ctx context.Context) ([]Exact, error) {
	units, err := m.units(ctx)
	if err != nil {
		return nil, err
	}
	result := []Exact{}
	for unit := range units {
		e, err := buildSliceExact(unit)
		if err != nil {
			return nil, err
		}
		r, err := m.record(e)
		if err != nil {
			return nil, errors.Join(ErrOutcomeUnknown, err)
		}
		if _, err := m.properties(ctx, r); err != nil {
			return nil, err
		}
		result = append(result, e) // Discovery never authorizes publication.
	}
	return result, nil
}
