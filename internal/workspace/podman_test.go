//go:build linux

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type commandStep struct {
	args   []string
	output string
	err    error
}
type stubCLI struct {
	t     *testing.T
	steps []commandStep
	calls int
}

func (s *stubCLI) run(_ context.Context, args ...string) ([]byte, error) {
	s.t.Helper()
	if s.calls >= len(s.steps) {
		s.t.Fatalf("unexpected command: %v", args)
	}
	step := s.steps[s.calls]
	s.calls++
	if !reflect.DeepEqual(step.args, args) {
		s.t.Fatalf("command=%v; want %v", args, step.args)
	}
	return []byte(step.output), step.err
}
func podmanStub(t *testing.T, steps ...commandStep) (*Podman, *stubCLI) {
	t.Helper()
	stub := &stubCLI{t: t, steps: steps}
	t.Cleanup(func() {
		if stub.calls != len(steps) {
			t.Errorf("executed %d of %d expected commands", stub.calls, len(steps))
		}
	})
	return &Podman{runner: stub, image: testImage, publicDNS: []string{"9.9.9.9"}, profileDir: filepath.Join(t.TempDir(), "profile")}, stub
}
func jsonText(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func inspectJSON(t *testing.T, o Object) string {
	return jsonText(t, []any{map[string]any{"Id": o.ID, "Name": o.Name,
		"Image": strings.Repeat("e", 64), "ImageName": o.Image,
		"Config": map[string]any{"Labels": o.Labels}}})
}
func listJSON(t *testing.T, objects ...Object) string {
	listed := make([]any, 0, len(objects))
	for _, o := range objects {
		listed = append(listed, map[string]any{"Id": o.ID, "Names": []string{o.Name}, "Labels": o.Labels})
	}
	return jsonText(t, listed)
}
func infoStep() commandStep {
	return commandStep{args: []string{"info", "--format=json"}, output: `{"host":{"security":{"rootless":true}}}`}
}
func listStep(output string) commandStep {
	return commandStep{args: []string{"container", "list", "--all", "--no-trunc", "--format=json"}, output: output}
}
func inspectStep(t *testing.T, o Object) commandStep {
	return commandStep{args: []string{"container", "inspect", "--", o.ID}, output: inspectJSON(t, o)}
}

func TestPodmanDiscoveryRequiresTopLevelImageName(t *testing.T) {
	e := Exact{identity(t, "s", "c").Workspace(), Generation(strings.Repeat("b", 64))}
	o := fakeObject(e)
	for _, mode := range []string{"canonical", "missing", "empty", "different"} {
		t.Run(mode, func(t *testing.T) {
			// Config.Image must not substitute for the top-level reference,
			// and the top-level Image is an image ID, not that reference.
			inspected := map[string]any{"Id": o.ID, "Name": o.Name,
				"Image":  strings.Repeat("e", 64),
				"Config": map[string]any{"Image": testImage, "Labels": o.Labels}}
			switch mode {
			case "canonical":
				inspected["ImageName"] = testImage
				inspected["Config"].(map[string]any)["Image"] = "different:latest"
			case "empty":
				inspected["ImageName"] = ""
			case "different":
				inspected["ImageName"] = "other@sha256:" + strings.Repeat("c", 64)
			}
			p, _ := podmanStub(t, infoStep(), listStep(listJSON(t, o)),
				commandStep{args: []string{"container", "inspect", "--", o.ID}, output: jsonText(t, []any{inspected})})
			if err := p.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			objects, err := p.List(context.Background())
			if mode == "canonical" {
				if err != nil || len(objects) != 1 || !reflect.DeepEqual(objects[0], o) {
					t.Fatalf("canonical object not discovered: %v, %v", objects, err)
				}
			} else if err == nil {
				t.Fatal("missing or mismatched ImageName accepted via another image field")
			}
		})
	}
}

func TestPodmanCreateUsesCanonicalExactObject(t *testing.T) {
	e := Exact{identity(t, "s", "c").Workspace(), Generation(strings.Repeat("b", 64))}
	o := fakeObject(e)
	create := []string{"container", "create", "--pull=never", "--name", objectName(e),
		"--network=pasta:--no-map-gw,-t,none,-u,none,-T,none,-U,none", "--image-volume=ignore", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--no-healthcheck", "--hooks-dir=PROFILE/hooks", "--default-mounts-file=PROFILE/mounts.conf",
		"--http-proxy=false", "--no-hosts", "--dns-search=.", "--publish-all=false",
		"--privileged=false", "--pid=private", "--ipc=private", "--uts=private", "--cgroupns=private",
		"--systemd=false", "--sdnotify=ignore", "--seccomp-policy=default", "--dns=9.9.9.9",
		"--memory=4294967296", "--memory-swap=4294967296", "--cpu-quota=-1", "--cgroup-conf=cpu.weight=100", "--pids-limit=512", "--ulimit=nofile=1024:1024",
		"--label", generationLabel + "=" + string(e.Generation),
		"--label", imageLabel + "=" + testImage,
		"--label", ownerLabel + "=" + ownerValue,
		"--label", workspaceLabel + "=" + string(e.Workspace), "--", testImage}
	for _, mode := range []string{"success", "custom-policy", "create-error", "invalid-create-id", "mismatched-created-object", "start-error"} {
		t.Run(mode, func(t *testing.T) {
			args := append([]string(nil), create...)
			policy := ReferencePolicy()
			if mode == "custom-policy" {
				policy.MemoryBytes = 2 << 30
				policy.SwapBytes = 1 << 30
				policy.CPUWeight = 200
				policy.CPUQuota = 50000
				policy.PIDsMax = 64
				policy.MigrationTasksMax = 65
				policy.NoFile = 256
				for n, arg := range args {
					switch arg {
					case "--memory=4294967296":
						args[n] = "--memory=2147483648"
					case "--memory-swap=4294967296":
						args[n] = "--memory-swap=3221225472"
					case "--cpu-quota=-1":
						args[n] = "--cpu-quota=50000"
					case "--cgroup-conf=cpu.weight=100":
						args[n] = "--cgroup-conf=cpu.weight=200"
					case "--pids-limit=512":
						args[n] = "--pids-limit=64"
					case "--ulimit=nofile=1024:1024":
						args[n] = "--ulimit=nofile=256:256"
					}
				}
			}
			steps := []commandStep{infoStep(), {args: args, output: o.ID + "\n"}}
			switch mode {
			case "create-error":
				steps[1].err = injected
			case "invalid-create-id":
				steps[1].output = "unknown"
			default:
				inspected := fakeObject(e)
				if mode == "mismatched-created-object" {
					inspected.Labels[generationLabel] = strings.Repeat("d", 64)
				}
				steps = append(steps, inspectStep(t, inspected))
				if mode != "mismatched-created-object" {
					start := commandStep{args: []string{"container", "start", "--", o.ID}, output: o.ID}
					if mode == "start-error" {
						start.err = injected
					}
					steps = append(steps, start)
				}
			}
			p, _ := podmanStub(t, steps...)
			for i := range args {
				args[i] = strings.ReplaceAll(args[i], "PROFILE", p.profileDir)
			}
			p.policy = &policy
			if err := p.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			err := p.Create(context.Background(), e, testImage)
			if (mode == "success" || mode == "custom-policy") && err != nil {
				t.Fatal(err)
			}
			if mode != "success" && mode != "custom-policy" && err == nil {
				t.Fatal("partial/unrecognized create succeeded")
			}
		})
	}
}

func TestPodmanDestroyCannotRemoveLaterGeneration(t *testing.T) {
	w := identity(t, "s", "c").Workspace()
	g1, g2 := Exact{w, Generation(strings.Repeat("b", 64))}, Exact{w, Generation(strings.Repeat("c", 64))}
	o1, o2 := fakeObject(g1), fakeObject(g2)
	p, _ := podmanStub(t, infoStep(), listStep(listJSON(t, o1, o2)), inspectStep(t, o1), inspectStep(t, o2),
		commandStep{args: []string{"container", "rm", "--force", "--", o1.ID}})
	if err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(context.Background(), g1); err != nil {
		t.Fatal(err)
	}
	// Old G1 is absent while G2 remains: no delete command is issued.
	p, _ = podmanStub(t, infoStep(), listStep(listJSON(t, o2)), inspectStep(t, o2))
	if err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(context.Background(), g1); err != nil {
		t.Fatal(err)
	}
}

func TestPodmanFailsClosedOnInspectionOrMutationFault(t *testing.T) {
	e := Exact{identity(t, "s", "c").Workspace(), Generation(strings.Repeat("b", 64))}
	o := fakeObject(e)
	for _, mode := range []string{"lookalike", "wrong-image", "inspect-error", "wrong-id", "rm-error"} {
		t.Run(mode, func(t *testing.T) {
			listed, inspected := fakeObject(e), fakeObject(e)
			switch mode {
			case "lookalike":
				delete(inspected.Labels, ownerLabel)
			case "wrong-image":
				inspected.Image = "other@sha256:" + strings.Repeat("c", 64)
				inspected.Labels[imageLabel] = inspected.Image
			case "wrong-id":
				inspected.ID = strings.Repeat("d", 64)
			}
			steps := []commandStep{infoStep(), listStep(listJSON(t, listed)), {args: []string{"container", "inspect", "--", o.ID}, output: inspectJSON(t, inspected)}}
			if mode == "inspect-error" {
				steps[2].err = injected
			}
			if mode == "rm-error" {
				steps = append(steps, commandStep{args: []string{"container", "rm", "--force", "--", o.ID}, err: injected})
			}
			p, _ := podmanStub(t, steps...)
			if err := p.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := p.Destroy(context.Background(), e); err == nil {
				t.Fatal("uncertain/unrecognized runtime mutation succeeded")
			}
		})
	}
}

func TestPodmanMachineOutputAndInitializationFailClosed(t *testing.T) {
	for _, output := range []string{"null", `{}`, `[{}]`, `[`, `[{"Id":"short","Names":["unrelated"]}]`} {
		t.Run(output, func(t *testing.T) {
			p, _ := podmanStub(t, infoStep(), listStep(output))
			if err := p.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := p.List(context.Background()); err == nil {
				t.Fatal("invalid discovery established absence")
			}
		})
	}
	for _, output := range []string{`{"host":{"security":{"rootless":false}}}`, `{}`, `null`, `invalid`} {
		p, _ := podmanStub(t, commandStep{args: []string{"info", "--format=json"}, output: output})
		if err := p.Initialize(context.Background()); err == nil {
			t.Fatal("non-rootless/unknown runtime accepted")
		}
		if _, err := p.List(context.Background()); !errors.Is(err, ErrNotReady) {
			t.Fatal("runtime admitted after failed initialization", err)
		}
	}
	p, _ := podmanStub(t, infoStep(), listStep(listJSON(t, Object{ID: strings.Repeat("d", 64), Name: "unrelated"})))
	if err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	objects, err := p.List(context.Background())
	if err != nil || len(objects) != 0 {
		t.Fatal("unrelated container adopted", err)
	}
	if err := p.Destroy(context.Background(), Exact{}); err == nil {
		t.Fatal("invalid destructive authority accepted")
	}
	if err := p.Create(context.Background(), Exact{}, testImage); err == nil {
		t.Fatal("invalid creation authority accepted")
	}
}

func TestLocalCLINeverSelectsRemoteEngine(t *testing.T) {
	t.Setenv("CONTAINER_HOST", "ssh://must-not-be-used")
	t.Setenv("CONTAINER_CONNECTION", "must-not-be-used")
	t.Setenv("DOCKER_HOST", "unix:///must-not-be-used")
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE"} {
		t.Setenv(key, "must-not-be-used")
	}
	for _, value := range hostCommand(context.Background(), "/unused").Env {
		if strings.Contains(value, "must-not-be-used") {
			t.Fatal("ambient runtime/proxy environment escaped", value)
		}
	}
	path := filepath.Join(t.TempDir(), "stub-podman")
	script := "#!/bin/sh\n[ -z \"$CONTAINER_HOST$CONTAINER_CONNECTION$DOCKER_HOST\" ] || exit 1\nprintf '%s\\n' \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := (localCLI{path}).run(context.Background(), "info", "--format=json")
	if err != nil || string(data) != "--remote=false\ninfo\n--format=json\n" {
		t.Fatal("remote engine boundary violated", string(data), err)
	}
}
