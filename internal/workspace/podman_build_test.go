//go:build linux

package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type buildCLIStub struct {
	*stubCLI
	args        []string
	input, temp string
	builds      int
	result      BackendResult
}

func (f *buildCLIStub) runBuild(_ context.Context, input, temp string, out, diagnostic io.Writer, args ...string) BackendResult {
	f.builds++
	f.args = append([]string{}, args...)
	f.input, f.temp = input, temp
	_, _ = out.Write([]byte("build output"))
	return f.result
}
func imageListStep(t *testing.T, ids ...string) commandStep {
	list := []any{}
	for _, id := range ids {
		list = append(list, map[string]any{"Id": id})
	}
	return commandStep{args: []string{"image", "list", "--all", "--no-trunc", "--format=json"}, output: jsonText(t, list)}
}
func imageInspectStep(t *testing.T, ref, id string, labels map[string]string, tags, onbuild []string) commandStep {
	return commandStep{args: []string{"image", "inspect", "--", ref}, output: jsonText(t, []any{map[string]any{"Id": id, "RepoTags": tags, "Config": map[string]any{"Labels": labels, "OnBuild": onbuild}}})}
}
func externalListStep(t *testing.T, objects ...Object) commandStep {
	return commandStep{args: []string{"container", "list", "--all", "--external", "--no-trunc", "--format=json"}, output: listJSON(t, objects...)}
}
func buildFixture(t *testing.T, e Exact, plan BuildPlan) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "builds", objectName(e))
	if err := os.MkdirAll(filepath.Join(path, "context"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(path, "tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "context", "Dockerfile"), []byte(plan.dockerfile), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func configureBuildFixture(t *testing.T, p *Podman, e Exact, plan BuildPlan) string {
	path := buildFixture(t, e, plan)
	p.buildConfig.DataDir = filepath.Dir(filepath.Dir(path))
	p.buildah = &stubCLI{t: t}
	return path
}
func TestPodmanBuildPinnedSourcesAndFixedOptions(t *testing.T) {
	e := Exact{identity(t, "s", "build").Workspace(), Generation(strings.Repeat("d", 64))}
	base := strings.Repeat("b", 64)
	built := strings.Repeat("c", 64)
	source := "registry.example/base:v1"
	plan, err := compileDockerfile("FROM "+source+" AS base\nFROM scratch\nCOPY --chown=0:0 --from="+source+" /x /x\nCOPY --from=base /y /y\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	p, stub := podmanStub(t, infoStep(), imageListStep(t), commandStep{args: []string{"pull", "--quiet", "--tls-verify=true", "--", source}, output: base}, imageInspectStep(t, base, base, nil, []string{source}, nil), imageInspectStep(t, buildImageName(e), built, objectLabels(e, buildImageName(e)), []string{buildImageName(e)}, nil))
	cli := &buildCLIStub{stubCLI: stub, result: BackendResult{Completion: DefinitelyCompleted}}
	p.runner = cli
	if err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := configureBuildFixture(t, p, e, plan)
	result := p.Build(context.Background(), e, plan, path, io.Discard, io.Discard)
	if result.Err != nil || result.Completion != DefinitelyCompleted {
		t.Fatal(result)
	}
	if cli.builds != 1 || !strings.Contains(cli.input, "FROM sha256:"+base+" AS base") || !strings.Contains(cli.input, "--from=sha256:"+base+" ") || !strings.Contains(cli.input, "--from=base ") {
		t.Fatalf("unpinned image position: %s", cli.input)
	}
	if cli.temp != filepath.Join(path, "tmp") {
		t.Fatal("temp state escaped exact Data path")
	}
	args := strings.Join(cli.args, " ")
	for _, flag := range []string{"--file=-", "--pull=never", "--layers=false", "--rm=true", "--force-rm=true", "--jobs=1", "--network=private", "--http-proxy=false", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--memory=4294967296", "--ulimit=nofile=1024:1024", "--tag=" + buildImageName(e)} {
		if !strings.Contains(args, flag) {
			t.Fatalf("missing trusted option %s: %s", flag, args)
		}
	}
	for _, flag := range []string{"--volume", "--secret", "--ssh", "--device", "--privileged", "--build-context", "--cap-add", "--network=host"} {
		if strings.Contains(args, flag) {
			t.Fatal("caller authority added: " + args)
		}
	}
	if !reflect.DeepEqual(cli.args[len(cli.args)-2:], []string{"--", filepath.Join(path, "context")}) {
		t.Fatal(cli.args)
	}
}
func TestPodmanBuildRejectsTriggersAndOwnedPullResults(t *testing.T) {
	e := Exact{identity(t, "s", "build").Workspace(), Generation(strings.Repeat("d", 64))}
	id := strings.Repeat("b", 64)
	source := "registry.example/base:v1"
	for _, mode := range []string{"onbuild", "owned", "malformed-ownership", "network-denied"} {
		t.Run(mode, func(t *testing.T) {
			plan, err := compileDockerfile("FROM "+source, nil)
			if err != nil {
				t.Fatal(err)
			}
			steps := []commandStep{infoStep(), imageListStep(t)}
			pull := commandStep{args: []string{"pull", "--quiet", "--tls-verify=true", "--", source}, output: id}
			if mode == "network-denied" {
				pull.err = injected
			}
			steps = append(steps, pull)
			labels := map[string]string{}
			triggers := []string{}
			if mode == "onbuild" {
				triggers = []string{"COPY --from=alpine /x /x"}
			}
			if mode == "owned" {
				labels = objectLabels(e, buildImageName(e))
			}
			if mode == "malformed-ownership" {
				labels[ownerLabel] = ownerValue
			}
			if mode != "network-denied" {
				steps = append(steps, imageInspectStep(t, id, id, labels, []string{source}, triggers))
			}
			p, stub := podmanStub(t, steps...)
			cli := &buildCLIStub{stubCLI: stub}
			p.runner = cli
			if err := p.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			result := p.Build(context.Background(), e, plan, configureBuildFixture(t, p, e, plan), io.Discard, io.Discard)
			if result.Err == nil || cli.builds != 0 {
				t.Fatalf("consumed unchecked build input: %v", result)
			}
		})
	}
}
func TestPodmanKnownImagesIncludeHistoricalAliases(t *testing.T) {
	e := Exact{identity(t, "s", "old").Workspace(), Generation(strings.Repeat("d", 64))}
	id := strings.Repeat("b", 64)
	inspect := imageInspectStep(t, id, id, objectLabels(e, buildImageName(e)), []string{buildImageName(e), "registry.example/current:v1"}, nil)
	inspect.output = strings.Replace(inspect.output, `"Id":`, `"NamesHistory":["registry.example/historical:v1"],"Id":`, 1)
	p, _ := podmanStub(t, infoStep(), imageListStep(t, id), inspect)
	if err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	known, err := p.KnownBuildImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"registry.example/current:v1", "registry.example/historical:v1"} {
		if _, err := compileDockerfile("FROM "+source, known); err == nil {
			t.Fatal("historical alias accepted")
		}
	}
}
func buildahInspectStep(t *testing.T, id, marker string) commandStep {
	return commandStep{args: []string{"inspect", "--type=container", "--", id}, output: jsonText(t, map[string]any{"Type": "buildah 0.0.1", "ContainerID": id, "DefaultMountsFilePath": marker})}
}
func externalInventorySteps(t *testing.T, objects ...Object) []commandStep {
	return []commandStep{externalListStep(t, objects...), listStep(listJSON(t))}
}
func storageStub(t *testing.T, p *Podman, root string, steps ...commandStep) {
	t.Helper()
	stub := &stubCLI{t: t, steps: steps}
	p.buildah, p.buildConfig.DataDir = stub, root
	t.Cleanup(func() {
		if stub.calls != len(steps) {
			t.Errorf("Buildah executed %d of %d commands", stub.calls, len(steps))
		}
	})
}
func TestPodmanCleanupExactImagesContainersAndReadback(t *testing.T) {
	e := Exact{identity(t, "s", "one").Workspace(), Generation(strings.Repeat("d", 64))}
	other := Exact{identity(t, "s", "two").Workspace(), Generation(strings.Repeat("e", 64))}
	id, otherID := strings.Repeat("b", 64), strings.Repeat("c", 64)
	// Working containers have no labels, and both generations use a shared base.
	temp := Object{ID: strings.Repeat("f", 64), Name: "base-working-container"}
	otherTemp := Object{ID: strings.Repeat("a", 64), Name: "base-working-container-1"}
	steps := []commandStep{infoStep()}
	steps = append(steps, externalInventorySteps(t, temp, otherTemp)...)
	steps = append(steps, imageListStep(t, id, otherID), imageInspectStep(t, id, id, objectLabels(e, buildImageName(e)), []string{buildImageName(e)}, nil), imageInspectStep(t, otherID, otherID, objectLabels(other, buildImageName(other)), []string{buildImageName(other)}, nil), commandStep{args: []string{"image", "rm", "--no-prune", "--", id}}, imageListStep(t, otherID), imageInspectStep(t, otherID, otherID, objectLabels(other, buildImageName(other)), []string{buildImageName(other)}, nil))
	steps = append(steps, externalInventorySteps(t, otherTemp)...)
	p, _ := podmanStub(t, steps...)
	root := t.TempDir()
	marker := func(g Exact) string { return filepath.Join(root, "builds", objectName(g), buildMountsFile) }
	storageStub(t, p, root, buildahInspectStep(t, temp.ID, marker(e)), buildahInspectStep(t, otherTemp.ID, marker(other)), commandStep{args: []string{"rm", "--", temp.ID}}, buildahInspectStep(t, otherTemp.ID, marker(other)))
	if err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.CleanupBuild(context.Background(), e, p.candidateBuildPath(e)); err != nil {
		t.Fatal(err)
	}
}
func TestPodmanCleanupUnidentifiedResidueFailsClosed(t *testing.T) {
	e := Exact{identity(t, "s", "one").Workspace(), Generation(strings.Repeat("d", 64))}
	for _, mode := range []string{"unattributed", "wrong-root", "wrong-id", "inspect-failure", "remove-failure", "retained-container", "retained-image"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "builds", objectName(e))
			temp := Object{ID: strings.Repeat("f", 64), Name: "base-working-container"}
			steps := []commandStep{infoStep()}
			var storage []commandStep
			if mode == "retained-image" {
				id := strings.Repeat("b", 64)
				inspect := imageInspectStep(t, id, id, objectLabels(e, buildImageName(e)), []string{buildImageName(e)}, nil)
				steps = append(steps, externalInventorySteps(t)...)
				steps = append(steps, imageListStep(t, id), inspect, commandStep{args: []string{"image", "rm", "--no-prune", "--", id}}, imageListStep(t, id), inspect)
			} else {
				steps = append(steps, externalInventorySteps(t, temp)...)
				marker := filepath.Join(path, buildMountsFile)
				if mode == "unattributed" {
					marker = ""
				}
				if mode == "wrong-root" {
					marker = filepath.Join(root, "other", objectName(e), buildMountsFile)
				}
				inspect := buildahInspectStep(t, temp.ID, marker)
				if mode == "wrong-id" {
					inspect.output = strings.ReplaceAll(inspect.output, temp.ID, strings.Repeat("a", 64))
				}
				if mode == "inspect-failure" {
					inspect.err = injected
				}
				storage = append(storage, inspect)
				if mode == "remove-failure" || mode == "retained-container" {
					rm := commandStep{args: []string{"rm", "--", temp.ID}}
					if mode == "remove-failure" {
						rm.err = injected
					}
					storage = append(storage, rm)
				}
				if mode == "retained-container" {
					steps = append(steps, imageListStep(t), imageListStep(t))
					steps = append(steps, externalInventorySteps(t, temp)...)
					storage = append(storage, inspect)
				}
			}
			p, _ := podmanStub(t, steps...)
			storageStub(t, p, root, storage...)
			if err := p.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := p.CleanupBuild(context.Background(), e, path); err == nil || mode != "remove-failure" && !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("uncertain residue removed/ignored: %v", err)
			}
		})
	}
}

func TestLocalBuildCLIStreamsAndDropsConnectionMaterial(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "build-stub")
	source := `#!/bin/sh
[ "$1" = "--remote=false" ] || exit 9
printf '%s,%s,%s,%s,%s,%s\n' "$CONTAINER_HOST" "$SSH_AUTH_SOCK" "$HTTP_PROXY" "$REGISTRY_AUTH_FILE" "$CONTAINERS_CONF" "$CONTAINERS_CONF_OVERRIDE"
cat
printf 'diagnostic' >&2
`
	if err := os.WriteFile(script, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINER_HOST", "forbidden")
	t.Setenv("SSH_AUTH_SOCK", "forbidden")
	t.Setenv("HTTP_PROXY", "forbidden")
	t.Setenv("REGISTRY_AUTH_FILE", "forbidden")
	t.Setenv("CONTAINERS_CONF", "forbidden")
	t.Setenv("CONTAINERS_CONF_OVERRIDE", "forbidden")
	out, diagnostic := newStreamCapture(MaxStreamBytes), newStreamCapture(MaxStreamBytes)
	result := (localCLI{script}).runBuild(context.Background(), "FROM scratch\n", dir, out, diagnostic, "build", "--file=-")
	if result.Err != nil || result.Completion != DefinitelyCompleted || string(out.bytes()) != ",,,,,\nFROM scratch\n" || string(diagnostic.bytes()) != "diagnostic" {
		t.Fatalf("local adapter=%v stdout=%q stderr=%q", result, out.bytes(), diagnostic.bytes())
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 9\n"), 0700); err != nil {
		t.Fatal(err)
	}
	result = (localCLI{script}).runBuild(context.Background(), "FROM scratch", dir, io.Discard, io.Discard, "build")
	if result.Completion != OutcomeUnknown || !errors.Is(result.Err, ErrOutcomeUnknown) {
		t.Fatal("abnormal exit attested backend completion")
	}
}

func TestPodmanCleanupUnidentifiedDanglingImageRetainsIntent(t *testing.T) {
	e := Exact{identity(t, "s", "one").Workspace(), Generation(strings.Repeat("d", 64))}
	id := strings.Repeat("b", 64)
	steps := []commandStep{infoStep()}
	steps = append(steps, externalInventorySteps(t)...)
	steps = append(steps, imageListStep(t, id), imageInspectStep(t, id, id, nil, nil, nil))
	p, _ := podmanStub(t, steps...)
	storageStub(t, p, t.TempDir())
	if err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.CleanupBuild(context.Background(), e, p.candidateBuildPath(e)); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal("unidentified dangling residue attested absent")
	}
}

func TestPodmanCleanupPreservesOrdinaryUnrelatedContainers(t *testing.T) {
	e := Exact{identity(t, "s", "ordinary").Workspace(), Generation(strings.Repeat("d", 64))}
	ordinary := Object{ID: strings.Repeat("a", 64), Name: "unrelated-container"}
	steps := []commandStep{infoStep(), externalListStep(t, ordinary), listStep(listJSON(t, ordinary)), imageListStep(t), imageListStep(t), externalListStep(t, ordinary), listStep(listJSON(t, ordinary))}
	p, _ := podmanStub(t, steps...)
	storageStub(t, p, t.TempDir()) // No Buildah inspection or removal is allowed.
	if err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.CleanupBuild(context.Background(), e, p.candidateBuildPath(e)); err != nil {
		t.Fatal(err)
	}
}

func TestLocalBuildahCLIBoundsAndDropsConnectionMaterial(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("adapter requires a non-root service account")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "buildah-stub")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
printf '%s,%s,%s,%s\n' "$CONTAINER_HOST" "$SSH_AUTH_SOCK" "$HTTP_PROXY" "$CONTAINERS_CONF_OVERRIDE"
printf '%s\n' "$@"
`), 0700); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"CONTAINER_HOST", "SSH_AUTH_SOCK", "HTTP_PROXY", "CONTAINERS_CONF_OVERRIDE"} {
		t.Setenv(key, "forbidden")
	}
	id := strings.Repeat("a", 64)
	got, err := (localBuildahCLI{script}).run(context.Background(), "inspect", "--type=container", "--", id)
	if err != nil || string(got) != ",,,\ninspect\n--type=container\n--\n"+id+"\n" {
		t.Fatalf("storage adapter stdout=%q error=%v", got, err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 9\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := (localBuildahCLI{script}).run(context.Background(), "rm", "--", id); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal("failed storage removal attested absence", err)
	}
	output := &boundedOutput{}
	if _, err := output.Write(make([]byte, (8<<20)+1)); err == nil {
		t.Fatal("unbounded storage diagnostics")
	}
}
