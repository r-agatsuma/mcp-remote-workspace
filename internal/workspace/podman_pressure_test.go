//go:build linux

package workspace

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type pressureCLI struct {
	*processCLI
	removes     int
	timeout     bool
	removeErr   error
	afterFirst  func()
	beforeRetry func()
	retain      bool
	events      *[]string
}

func (c *pressureCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) > 1 && args[0] == "container" && args[1] == "rm" {
		c.removes++
		*c.events = append(*c.events, "remove")
		if !reflect.DeepEqual(args, []string{"container", "rm", "--force", "--", strings.Repeat("a", 64)}) {
			c.t.Fatal("non-exact removal", args)
		}
		if c.removes == 1 && c.timeout {
			<-ctx.Done()
			if c.afterFirst != nil {
				c.afterFirst()
			}
			return nil, ctx.Err()
		}
		if c.removeErr != nil {
			return nil, c.removeErr
		}
		if c.removes > 1 && c.beforeRetry != nil {
			c.beforeRetry()
		}
		if !c.retain {
			c.listed = nil
		}
		return nil, nil
	}
	if len(args) > 1 && args[1] == "list" {
		*c.events = append(*c.events, "list")
	}
	return c.processCLI.run(ctx, args...)
}

func pressureFixture(t *testing.T) (*Podman, *pressureCLI, processFixture, Exact, *[]string) {
	t.Helper()
	e := Exact{identity(t, "pressure", "c").Workspace(), Generation(strings.Repeat("b", 64))}
	p, original, f := fixturePodman(t, e, ReferencePolicy())
	old := f.group
	f.cgroupPath = libpodScopePath(original.object.ID)
	f.group = filepath.Join(f.cgroups, f.cgroupPath)
	if err := os.MkdirAll(filepath.Dir(f.group), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(old, f.group); err != nil {
		t.Fatal(err)
	}
	for group := path.Dir(f.cgroupPath); group != "/"; group = path.Dir(group) {
		for _, file := range []string{"memory.max", "memory.swap.max", "pids.max", "cpu.max"} {
			value := "max"
			if file == "cpu.max" {
				value = "max 100000"
			}
			writeProbeFixture(t, filepath.Join(f.cgroups, group, file), value)
		}
	}
	writeProbeFixture(t, filepath.Join(f.process, "cgroup"), "0::"+f.cgroupPath+"\n")
	writeProbeFixture(t, filepath.Join(f.group, "pids.current"), "512")
	original.state["CgroupPath"] = f.cgroupPath
	events := []string{}
	cli := &pressureCLI{processCLI: original, timeout: true, events: &events}
	p.runner = cli
	p.removeTimeout = time.Millisecond
	return p, cli, f, e, &events
}

func TestDestroyPressureFallbackExactScopeAndAbsence(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "removed"}[removed], func(t *testing.T) {
			p, c, f, e, events := pressureFixture(t)
			unit := path.Base(f.cgroupPath)
			p.hostRunner = func(_ context.Context, exe string, args ...string) ([]byte, error) {
				if exe != "/usr/bin/systemctl" {
					t.Fatal(exe)
				}
				if reflect.DeepEqual(args, []string{"--user", "show", "--property=ControlGroup", "--value", "--", unit}) {
					*events = append(*events, "show")
					return []byte(f.cgroupPath + "\n"), nil
				}
				if !reflect.DeepEqual(args, []string{"--user", "kill", "--kill-whom=all", "--signal=SIGKILL", "--", unit}) {
					t.Fatal("wrong kill authority", args)
				}
				if c.inspectCalls != 7 {
					t.Fatal("did not independently rediscover and revalidate", c.inspectCalls)
				}
				*events = append(*events, "kill")
				if removed {
					return nil, os.RemoveAll(f.group) // only this test-owned cgroup fixture
				}
				writeProbeFixture(t, filepath.Join(f.group, "cgroup.events"), "populated 0\nfrozen 0\n")
				writeProbeFixture(t, filepath.Join(f.group, "pids.current"), "0")
				return nil, nil
			}
			c.beforeRetry = func() {
				if removed {
					if _, err := os.Stat(f.group); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("retry before removed-scope evidence", err)
					}
					return
				}
				data, _ := os.ReadFile(filepath.Join(f.group, "pids.current"))
				if string(data) != "0" {
					t.Fatal("retry before terminal evidence")
				}
			}
			if err := p.Destroy(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*events, []string{"list", "remove", "list", "show", "list", "kill", "remove", "list"}) {
				t.Fatal(*events)
			}
		})
	}
}

func TestDestroyOrdinarySuccessRequiresIndependentAbsence(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "residue"}[retained], func(t *testing.T) {
			p, c, _, e, events := pressureFixture(t)
			c.timeout = false
			c.retain = retained
			p.hostRunner = func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("ordinary success invoked fallback")
				return nil, nil
			}
			err := p.Destroy(context.Background(), e)
			if retained && !errors.Is(err, ErrOutcomeUnknown) || !retained && err != nil {
				t.Fatal(err)
			}
			if c.removes != 1 || len(*events) != 3 || (*events)[2] != "list" {
				t.Fatal(*events)
			}
		})
	}
}

func TestDestroyPressureRefusesAmbiguityAndNonPressure(t *testing.T) {
	cases := []string{"ordinary-error", "caller-canceled", "caller-deadline", "not-saturated", "stale-generation", "replaced-generation", "malformed-scope", "wrong-id", "sibling-scope", "user-slice", "build-parent", "wrong-manager", "pid-reused", "scope-replaced", "kill-failed", "kill-unconfirmed", "kill-scope-renamed", "retry-failed", "absence-unconfirmed", "discovery-failed"}
	for _, mode := range cases {
		t.Run(mode, func(t *testing.T) {
			p, c, f, e, _ := pressureFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			kills := 0
			c.afterFirst = func() {
				switch mode {
				case "caller-canceled":
					cancel()
				case "not-saturated":
					writeProbeFixture(t, filepath.Join(f.group, "pids.current"), "511")
				case "stale-generation", "replaced-generation":
					other := e
					other.Generation = Generation(strings.Repeat("c", 64))
					c.object = fakeObject(other)
					c.listed = []Object{c.object}
				case "malformed-scope":
					c.state["CgroupPath"] = f.cgroupPath + "/../bad"
				case "wrong-id":
					c.object.ID = strings.Repeat("d", 64)
					c.listed = []Object{c.object}
				case "sibling-scope":
					c.state["CgroupPath"] = libpodScopePath(strings.Repeat("e", 64))
				case "user-slice":
					c.state["CgroupPath"] = path.Dir(f.cgroupPath)
				case "build-parent":
					c.state["CgroupPath"] = buildParentPath(e)
				case "discovery-failed":
					c.object.Labels[ownerLabel] = "unknown"
				}
			}
			switch mode {
			case "ordinary-error":
				c.timeout = false
				c.removeErr = injected
			case "caller-deadline":
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, time.Millisecond)
				defer stop()
				p.removeTimeout = time.Second
			case "stale-generation":
				e.Generation = Generation(strings.Repeat("c", 64)) // no exact original object: no rm/kill
			case "absence-unconfirmed":
				c.retain = true
			}
			p.hostRunner = func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if args[1] == "show" {
					switch mode {
					case "wrong-manager":
						return []byte("/user.slice"), nil
					case "pid-reused":
						writeProbeFixture(t, filepath.Join(f.process, "stat"), probeStat(123, 999))
					case "scope-replaced":
						if err := os.Rename(f.group, f.group+"-old"); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(f.group, 0700); err != nil {
							t.Fatal(err)
						}
					}
					return []byte(f.cgroupPath), nil
				}
				kills++
				if args[len(args)-1] != path.Base(f.cgroupPath) {
					t.Fatal("broad or wrong kill", args)
				}
				switch mode {
				case "kill-failed":
					return nil, injected
				case "kill-unconfirmed":
					cancel()
					return nil, nil
				case "kill-scope-renamed":
					return nil, os.Rename(f.group, f.group+"-moved")
				}
				writeProbeFixture(t, filepath.Join(f.group, "cgroup.events"), "populated 0\nfrozen 0")
				writeProbeFixture(t, filepath.Join(f.group, "pids.current"), "0")
				if mode == "retry-failed" {
					c.removeErr = injected
				}
				return nil, nil
			}
			err := p.Destroy(ctx, e)
			if mode == "stale-generation" {
				if err != nil || c.removes != 0 || kills != 0 {
					t.Fatal(err, c.removes, kills)
				}
				return
			}
			if !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatal("ambiguity became terminal", err)
			}
			allowedKill := mode == "kill-failed" || mode == "kill-unconfirmed" || mode == "kill-scope-renamed" || mode == "retry-failed" || mode == "absence-unconfirmed"
			if !allowedKill && kills != 0 {
				t.Fatal("unsafe kill", mode)
			}
			if mode != "retry-failed" && mode != "absence-unconfirmed" && c.removes > 1 {
				t.Fatal("retried without proof")
			}
		})
	}
}
