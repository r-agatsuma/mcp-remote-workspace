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
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

type processCLI struct {
	t                       *testing.T
	object                  Object
	state                   map[string]any
	inspectCalls, execCalls int
	listed                  []Object
	hook                    func(int)
	start                   func(context.Context, ExecRequest, io.Writer, io.Writer, ...string) (<-chan ExecOutcome, error)
}

func (f *processCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch {
	case reflect.DeepEqual(args, []string{"info", "--format=json"}):
		return []byte(infoStep().output), nil
	case reflect.DeepEqual(args, []string{"container", "list", "--all", "--no-trunc", "--format=json"}):
		return []byte(listJSON(f.t, f.listed...)), nil
	case reflect.DeepEqual(args, []string{"container", "inspect", "--", f.object.ID}):
		f.inspectCalls++
		if f.hook != nil {
			f.hook(f.inspectCalls)
		}
		return json.Marshal([]any{map[string]any{
			"Id": f.object.ID, "Name": f.object.Name, "ImageName": f.object.Image,
			"Config": map[string]any{"Labels": f.object.Labels}, "State": f.state,
		}})
	default:
		return nil, errors.New("unexpected probe command")
	}
}

func (f *processCLI) startExec(ctx context.Context, r ExecRequest, out, diagnostic io.Writer, args ...string) (<-chan ExecOutcome, error) {
	f.execCalls++
	if len(args) < len(r.Argv)+2 || args[len(args)-len(r.Argv)-1] != f.object.ID {
		return nil, CompletedBackendError(errors.New("exec did not target immutable ID"))
	}
	if f.start != nil {
		return f.start(ctx, r, out, diagnostic, args...)
	}
	return terminal(ExecOutcome{Completion: DefinitelyCompleted}), nil
}

type processFixture struct {
	proc, cgroups, process, group string
	cgroupPath                    string
}

func writeProbeFixture(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func probeStat(pid int, start uint64) string {
	// 19 fields beginning with field 3, then field 22 (starttime).
	fields := append([]string{"S"}, strings.Fields(strings.Repeat("0 ", 18))...)
	return fmt.Sprintf("%d (comm with ) spaces) %s %d\n", pid, strings.Join(fields, " "), start)
}

func newProcessFixture(t *testing.T, policy Policy, id string) processFixture {
	t.Helper()
	root := t.TempDir()
	f := processFixture{proc: filepath.Join(root, "proc"), cgroups: filepath.Join(root, "cgroup")}
	f.process = filepath.Join(f.proc, "123")
	f.cgroupPath = "/service/libpod-" + id + ".scope"
	f.group = filepath.Join(f.cgroups, strings.TrimPrefix(f.cgroupPath, "/"))
	for _, dir := range []string{f.process, f.group} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeProbeFixture(t, filepath.Join(f.process, "stat"), probeStat(123, 456))
	writeProbeFixture(t, filepath.Join(f.process, "cgroup"), "0::"+f.cgroupPath+"\n")
	writeProbeFixture(t, filepath.Join(f.process, "limits"), fmt.Sprintf(
		"Limit                     Soft Limit           Hard Limit           Units\nMax open files            %d                 %d                 files\n", policy.NoFile, policy.NoFile))
	values := map[string]string{
		"cgroup.type": "domain", "cgroup.events": "populated 1\nfrozen 0\n",
		"cgroup.procs": "123\n124\n", "pids.current": "2",
		"memory.max":      strconv.FormatInt(policy.MemoryBytes, 10),
		"memory.swap.max": strconv.FormatInt(policy.SwapBytes, 10),
		"cpu.max":         "max 100000", "cpu.weight": strconv.FormatUint(policy.CPUWeight, 10),
		"pids.max": strconv.FormatUint(policy.PIDsMax, 10),
	}
	if policy.CPUQuota != -1 {
		values["cpu.max"] = fmt.Sprintf("%d 100000", policy.CPUQuota)
	}
	for name, value := range values {
		writeProbeFixture(t, filepath.Join(f.group, name), value+"\n")
	}
	for _, name := range []string{"memory.max", "memory.swap.max", "pids.max", "cpu.max"} {
		value := "max"
		if name == "cpu.max" {
			value = "max 100000"
		}
		writeProbeFixture(t, filepath.Join(f.cgroups, "service", name), value+"\n")
	}
	return f
}

func (f processFixture) openRoots() (*os.Root, *os.Root, error) {
	proc, err := os.OpenRoot(f.proc)
	if err != nil {
		return nil, nil, err
	}
	cgroups, err := os.OpenRoot(f.cgroups)
	if err != nil {
		proc.Close()
		return nil, nil, err
	}
	return proc, cgroups, nil
}

func fixturePodman(t *testing.T, e Exact, policy Policy) (*Podman, *processCLI, processFixture) {
	t.Helper()
	object := fakeObject(e)
	object.ID = strings.Repeat("a", 64) // runtime object identity is not the generation token
	fixture := newProcessFixture(t, policy, object.ID)
	cli := &processCLI{t: t, object: object, listed: []Object{object}, state: map[string]any{
		"Status": "running", "Running": true, "Pid": 123, "StartedAt": "2026-10-13T00:00:00Z",
		"CgroupPath": fixture.cgroupPath,
	}}
	p := NewPodmanWithPolicy("/stub/podman", testImage, policy, "9.9.9.9")
	p.profileDir = filepath.Join(t.TempDir(), "profile")
	p.runner, p.processRoots = cli, fixture.openRoots
	if err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return p, cli, fixture
}

func TestPodmanProcessAttestsActualIdentityAndLimits(t *testing.T) {
	for _, custom := range []bool{false, true} {
		policy := ReferencePolicy()
		if custom {
			policy.MemoryBytes, policy.SwapBytes = 2<<30, 1<<30
			policy.CPUQuota, policy.CPUWeight, policy.NoFile = 50000, 200, 256
			policy.PIDsMax, policy.MigrationTasksMax = 64, 65
		}
		e := Exact{identity(t, "probe", "session").Workspace(), Generation(strings.Repeat("b", 64))}
		p, cli, fixture := fixturePodman(t, e, policy)
		if custom {
			// An equivalent bandwidth ratio with a different period is attested
			// from cpu.max, rather than from desired HostConfig settings.
			writeProbeFixture(t, filepath.Join(fixture.group, "cpu.max"), "25000 50000")
		}
		v, err := p.ProbeProcess(context.Background(), e)
		if err != nil {
			t.Fatal(err)
		}
		expected := safeSnapshot(e, policy, 2)
		info, err := os.Stat(fixture.group)
		if err != nil {
			t.Fatal(err)
		}
		expected.ObjectID = cli.object.ID
		expected.CgroupPath, expected.CgroupID = fixture.cgroupPath, info.Sys().(*syscall.Stat_t).Ino
		if v.CgroupID == 0 || !info.IsDir() || v != expected || cli.inspectCalls != 3 {
			t.Fatalf("snapshot=%+v; expected=%+v; inspections=%d", v, expected, cli.inspectCalls)
		}
	}
}

func TestPodmanProcessFailsClosedOnMissingMalformedAndMismatchedState(t *testing.T) {
	cases := []struct {
		name   string
		change func(*processCLI, processFixture)
	}{
		{"absent", func(c *processCLI, _ processFixture) { c.listed = []Object{} }},
		{"duplicate", func(c *processCLI, _ processFixture) { c.listed = append(c.listed, c.object) }},
		{"generation", func(c *processCLI, _ processFixture) { c.object.Labels[generationLabel] = strings.Repeat("d", 64) }},
		{"image", func(c *processCLI, _ processFixture) { c.object.Image = "other@sha256:" + strings.Repeat("c", 64) }},
		{"stopped", func(c *processCLI, _ processFixture) { c.state["Running"] = false }},
		{"paused", func(c *processCLI, _ processFixture) { c.state["Paused"] = true }},
		{"no-pid", func(c *processCLI, _ processFixture) { delete(c.state, "Pid") }},
		{"no-started-at", func(c *processCLI, _ processFixture) { delete(c.state, "StartedAt") }},
		{"no-runtime-cgroup", func(c *processCLI, _ processFixture) { delete(c.state, "CgroupPath") }},
		{"runtime-cgroup-mismatch", func(c *processCLI, _ processFixture) {
			c.state["CgroupPath"] = "/service/libpod-" + strings.Repeat("d", 64) + ".scope"
		}},
		{"runtime-cgroup-traversal", func(c *processCLI, f processFixture) {
			c.state["CgroupPath"] = f.cgroupPath + "/../" + filepath.Base(f.group)
		}},
		{"invalid-started-at", func(c *processCLI, _ processFixture) { c.state["StartedAt"] = "unknown" }},
		{"dead-pid", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.process, "stat"), strings.Replace(probeStat(123, 456), ") S ", ") Z ", 1))
		}},
		{"wrong-pid", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.process, "stat"), probeStat(124, 456))
		}},
		{"zero-start", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.process, "stat"), probeStat(123, 0))
		}},
		{"no-process", func(_ *processCLI, f processFixture) { os.Remove(filepath.Join(f.process, "stat")) }},
		{"root-cgroup", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.process, "cgroup"), "0::/\n")
		}},
		{"relative-cgroup", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.process, "cgroup"), "0::relative\n")
		}},
		{"traversal", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.process, "cgroup"), "0::/service/../exact\n")
		}},
		{"ambiguous-membership", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.process, "cgroup"), "0::/service/exact\n0::/other\n")
		}},
		{"wrong-membership", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.group, "cgroup.procs"), "124\n")
		}},
		{"threaded", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.group, "cgroup.type"), "threaded")
		}},
		{"frozen", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.group, "cgroup.events"), "populated 1\nfrozen 1")
		}},
		{"missing-limit", func(_ *processCLI, f processFixture) { os.Remove(filepath.Join(f.group, "memory.max")) }},
		{"unlimited-memory", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.group, "memory.max"), "max")
		}},
		{"memory", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.group, "memory.max"), "1234")
		}},
		{"swap", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.group, "memory.swap.max"), "1")
		}},
		{"quota", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.group, "cpu.max"), "50000 100000")
		}},
		{"quota-period", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.group, "cpu.max"), "max 0")
		}},
		{"weight", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.group, "cpu.weight"), "200")
		}},
		{"pids-limit", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.group, "pids.max"), "1024")
		}},
		{"pids-full", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.group, "pids.current"), "512")
		}},
		{"migration-full", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.group, "pids.current"), "513")
		}},
		{"migration-overflow", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.group, "pids.current"), "514")
		}},
		{"zero-tasks", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.group, "pids.current"), "0")
		}},
		{"nofile-soft", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.process, "limits"), "Max open files 512 1024 files")
		}},
		{"nofile-hard", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.process, "limits"), "Max open files 1024 2048 files")
		}},
		{"nofile-missing", func(_ *processCLI, f processFixture) { writeProbeFixture(t, filepath.Join(f.process, "limits"), "") }},
		{"oversized-proc", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.process, "stat"), strings.Repeat("secret", 1<<14))
		}},
		{"inherited-memory", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.cgroups, "service", "memory.max"), "1024")
		}},
		{"inherited-pids", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.cgroups, "service", "pids.max"), "64")
		}},
		{"inherited-cpu", func(_ *processCLI, f processFixture) {
			writeProbeFixture(t, filepath.Join(f.cgroups, "service", "cpu.max"), "50000 100000")
		}},
		{"ancestor-missing", func(_ *processCLI, f processFixture) { os.Remove(filepath.Join(f.cgroups, "service", "cpu.max")) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, i, e := openTools(t, Config{})
			p, cli, fixture := fixturePodman(t, e, s.policy)
			s.runtime = p // ordinary production type, never a ProcessProbe wrapper
			tc.change(cli, fixture)
			for _, call := range allToolCalls(s, i, e) {
				err := call()
				if !errors.Is(err, ErrProcessUnsafe) || len(s.PendingBackend()) != 0 {
					t.Fatal("unsafe probe did not fail closed and release unstarted reservation", err)
				}
			}
			if cli.execCalls != 0 {
				t.Fatal("unsafe state reached runtime submission")
			}
		})
	}
}

func TestPodmanProcessRejectsRaces(t *testing.T) {
	for _, mode := range []string{"pid-start", "pid-reuse", "pid-change", "restart", "exit", "membership", "cgroup-reuse", "limit", "hidden-ancestor-limit", "nofile", "tasks-at-limit"} {
		t.Run(mode, func(t *testing.T) {
			s, _, i, e := openTools(t, Config{})
			p, cli, fixture := fixturePodman(t, e, s.policy)
			s.runtime = p
			cli.hook = func(n int) {
				if n != 3 {
					return
				}
				switch mode {
				case "pid-start":
					writeProbeFixture(t, filepath.Join(fixture.process, "stat"), probeStat(123, 999))
				case "pid-reuse":
					if err := os.Rename(fixture.process, fixture.process+"-old"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(fixture.process, 0700); err != nil {
						t.Fatal(err)
					}
					for _, name := range []string{"stat", "cgroup", "limits"} {
						data, err := os.ReadFile(filepath.Join(fixture.process+"-old", name))
						if err != nil {
							t.Fatal(err)
						}
						writeProbeFixture(t, filepath.Join(fixture.process, name), string(data))
					}
					writeProbeFixture(t, filepath.Join(fixture.process, "stat"), probeStat(123, 999))
				case "pid-change":
					cli.state["Pid"] = 124
				case "restart":
					cli.state["StartedAt"] = "2026-10-13T00:01:00Z"
				case "exit":
					cli.state["Running"] = false
				case "membership":
					writeProbeFixture(t, filepath.Join(fixture.process, "cgroup"), "0::/service/other\n")
				case "cgroup-reuse":
					if err := os.Rename(fixture.group, fixture.group+"-old"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(fixture.group, 0700); err != nil {
						t.Fatal(err)
					}
				case "limit":
					writeProbeFixture(t, filepath.Join(fixture.group, "cpu.weight"), "200")
				case "hidden-ancestor-limit":
					writeProbeFixture(t, filepath.Join(fixture.cgroups, "service", "memory.max"), "8589934592")
				case "nofile":
					writeProbeFixture(t, filepath.Join(fixture.process, "limits"), "Max open files 512 1024 files")
				case "tasks-at-limit":
					writeProbeFixture(t, filepath.Join(fixture.group, "pids.current"), "512")
				}
			}
			_, err := s.Exec(context.Background(), i, e, ExecRequest{Argv: []string{"true"}})
			if !errors.Is(err, ErrProcessUnsafe) || cli.execCalls != 0 || len(s.PendingBackend()) != 0 {
				t.Fatal("raced probe authorized injection or retained an unstarted permit", err)
			}
		})
	}
}

func TestProductionPodmanToolsPassGateAndUnknownWriteIsNotReplayed(t *testing.T) {
	s, _, i, e := openTools(t, Config{})
	p, cli, _ := fixturePodman(t, e, s.policy)
	s.runtime = p
	cli.start = func(_ context.Context, r ExecRequest, out, diagnostic io.Writer, _ ...string) (<-chan ExecOutcome, error) {
		if s.injections[e] == nil || len(s.backend.Pending()) != 1 {
			t.Fatal("admission bypassed")
		}
		if r.Argv[0] == "echo" {
			io.WriteString(out, "stdout")
			io.WriteString(diagnostic, "stderr")
		} else if len(r.Argv) == 5 && r.Argv[3] == "read_text" {
			io.WriteString(out, "日本語")
		}
		return terminal(ExecOutcome{Completion: DefinitelyCompleted}), nil
	}
	result, err := s.Exec(context.Background(), i, e, ExecRequest{Argv: []string{"echo"}})
	if err != nil || result.Stdout.Text != "stdout" || result.Stderr.Text != "stderr" {
		t.Fatal(result, err)
	}
	read, err := s.ReadText(context.Background(), i, e, ReadTextRequest{Path: "/f"})
	if err != nil || read.Text != "日本語" {
		t.Fatal(read, err)
	}
	write, err := s.WriteText(context.Background(), i, e, WriteTextRequest{Path: "/f", Content: "text"})
	if err != nil || write.Status != "written" || cli.execCalls != 3 || len(s.PendingBackend()) != 0 {
		t.Fatal(write, err)
	}
	detached, err := s.Exec(context.Background(), i, e, ExecRequest{Argv: []string{"true"}, Detach: true})
	if err != nil || detached.Status != "started" {
		t.Fatal(detached, err)
	}
	cli.start = func(context.Context, ExecRequest, io.Writer, io.Writer, ...string) (<-chan ExecOutcome, error) {
		return terminal(ExecOutcome{Err: ErrOutcomeUnknown}), nil
	}
	_, err = s.WriteText(context.Background(), i, e, WriteTextRequest{Path: "/f", Content: "possibly partial"})
	expectCode(t, err, "outcome_unknown")
	for _, call := range allToolCalls(s, i, e) {
		expectCode(t, call(), "busy")
	}
	if cli.execCalls != 5 || len(s.PendingBackend()) != 1 {
		t.Fatal("unknown write replayed or lost accounting")
	}
}

func TestPodmanProcessCancellationAndUnavailableHostRoots(t *testing.T) {
	e := Exact{identity(t, "probe", "session").Workspace(), Generation(strings.Repeat("b", 64))}
	p, cli, _ := fixturePodman(t, e, ReferencePolicy())
	ctx, cancel := context.WithCancel(context.Background())
	cli.hook = func(n int) {
		if n == 3 {
			cancel()
		}
	}
	v, err := p.ProbeProcess(ctx, e)
	if !errors.Is(err, context.Canceled) || completionOf(err) != DefinitelyCompleted || v != (ProcessSnapshot{}) {
		t.Fatal("canceled read-only probe lacked bounded terminal evidence", v, err)
	}
	p.processRoots = func() (*os.Root, *os.Root, error) { return nil, nil, errors.New("host mount unavailable") }
	cli.hook = nil
	if _, err := p.ProbeProcess(context.Background(), e); !errors.Is(err, ErrProcessUnsafe) || completionOf(err) != DefinitelyCompleted {
		t.Fatal("missing host roots did not fail closed", err)
	}
	p.initialized = false
	if _, err := p.ProbeProcess(context.Background(), e); !errors.Is(err, ErrProcessUnsafe) {
		t.Fatal(err)
	}
}

func TestPodmanProcessCgroupLayoutsAndSubtreeAccounting(t *testing.T) {
	for _, layout := range []string{"cgroupfs", "split", "descendant", "descendant-limit"} {
		t.Run(layout, func(t *testing.T) {
			e := Exact{identity(t, "probe", "session").Workspace(), Generation(strings.Repeat("b", 64))}
			p, cli, f := fixturePodman(t, e, ReferencePolicy())
			if layout == "cgroupfs" || layout == "split" {
				name := "libpod-" + cli.object.ID
				if layout == "split" {
					name = "libpod-payload-" + cli.object.ID
				}
				newPath := "/service/" + name
				if err := os.Rename(f.group, filepath.Join(f.cgroups, strings.TrimPrefix(newPath, "/"))); err != nil {
					t.Fatal(err)
				}
				cli.state["CgroupPath"] = newPath
				writeProbeFixture(t, filepath.Join(f.process, "cgroup"), "0::"+newPath+"\n")
			} else {
				child := filepath.Join(f.group, "init.scope")
				if err := os.Mkdir(child, 0700); err != nil {
					t.Fatal(err)
				}
				writeProbeFixture(t, filepath.Join(f.process, "cgroup"), "0::"+f.cgroupPath+"/init.scope\n")
				writeProbeFixture(t, filepath.Join(f.group, "cgroup.procs"), "124\n")
				writeProbeFixture(t, filepath.Join(child, "cgroup.procs"), "123\n")
				for _, name := range []string{"memory.max", "memory.swap.max", "pids.max", "cpu.max"} {
					value := "max"
					if name == "cpu.max" {
						value = "max 100000"
					}
					if name == "memory.max" && layout == "descendant-limit" {
						value = "1024"
					}
					writeProbeFixture(t, filepath.Join(child, name), value)
				}
			}
			v, err := p.ProbeProcess(context.Background(), e)
			if layout == "descendant-limit" {
				if !errors.Is(err, ErrProcessUnsafe) {
					t.Fatal("descendant effective limit ignored", v, err)
				}
			} else if err != nil || v.Tasks != 2 || v.CgroupPath != cli.state["CgroupPath"] {
				t.Fatal("resource subtree or actual leaf membership lost", v, err)
			}
		})
	}
}

func TestFullCgroupMountRejectsPartialAmbiguousAndWrongFilesystems(t *testing.T) {
	full := "42 1 0:28 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n"
	for _, tc := range []struct {
		mounts string
		valid  bool
	}{
		{full, true},
		{full + "43 1 0:1 / /proc rw - proc proc rw\n", true},
		{"", false}, {full + full, false},
		{strings.Replace(full, " / /sys", " /user.slice /sys", 1), false},
		{strings.Replace(full, "cgroup2 ", "cgroup ", 1), false},
		{strings.Replace(full, "/sys/fs/cgroup", "/elsewhere", 1), false},
		{strings.Replace(full, " - ", " ", 1), false},
	} {
		if fullCgroupMount(tc.mounts) != tc.valid {
			t.Fatalf("unexpected mount attestation for %q", tc.mounts)
		}
	}
}

func TestProcessReadsCannotEscapeTrustedRoots(t *testing.T) {
	e := Exact{identity(t, "probe", "session").Workspace(), Generation(strings.Repeat("b", 64))}
	p, _, f := fixturePodman(t, e, ReferencePolicy())
	outside := filepath.Join(t.TempDir(), "stat")
	writeProbeFixture(t, outside, probeStat(123, 456))
	if err := os.Remove(filepath.Join(f.process, "stat")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.process, "stat")); err != nil {
		t.Fatal(err)
	}
	v, err := p.ProbeProcess(context.Background(), e)
	if !errors.Is(err, ErrProcessUnsafe) || v != (ProcessSnapshot{}) {
		t.Fatal("probe escaped host roots", v, err)
	}
}
