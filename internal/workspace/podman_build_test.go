//go:build linux

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDockerfileSourcePolicy(t *testing.T) {
	reserved := "localhost/" + namePrefix + strings.Repeat("a", 64) + "-" + strings.Repeat("b", 64)
	for _, tc := range []struct {
		text    string
		sources []string
		reject  bool
	}{
		{"FROM busybox AS base\nFROM base\nCOPY --from=0 /bin/sh /sh\nCOPY --from=alpine /etc/os-release /release", []string{"busybox", "alpine"}, false},
		{"FROM registry.example:5000/team/base@sha256:" + strings.Repeat("c", 64), []string{"registry.example:5000/team/base@sha256:" + strings.Repeat("c", 64)}, false},
		{"FROM busybox AS " + namePrefix + "stage\nCOPY --from=" + namePrefix + "stage /x /y", []string{"busybox"}, false},
		{"FROM busybox\nRUN <<EOF\necho FROM " + reserved + "\nEOF", []string{"busybox"}, false},
		{"FROM " + reserved, nil, true},
		{"FROM busybox\nCOPY --from=" + reserved + " /x /y", nil, true},
		{"FROM busybox\nONBUILD COPY --from=" + reserved + " /x /y", nil, true},
		{"ARG BASE=busybox\nFROM ${BASE}", nil, true},
		{"FROM busybox\nRUN --network=host true", nil, true},
		{"FROM busybox\nRUN --mount=type=bind,from=" + reserved + ",target=/mnt true", nil, true},
		{"FROM busybox\nARG MOUNT=type=bind,from=" + reserved + ",target=/mnt\nRUN --mount=$MOUNT true", nil, true},
		{"FROM busybox\nARG FLAG=from\nCOPY --${FLAG}=alpine /x /y", nil, true},
		{"FROM busybox AS ${STAGE}\nCOPY --from=${STAGE} /x /y", nil, true},
		{"FROM --platform=linux/arm64 busybox", nil, true},
		{"FROM busybox\nLABEL ordinary=value", []string{"busybox"}, false},
		{"FROM busybox\nLABEL \"" + ownerLabel + "\"=forged", nil, true},
	} {
		sources, err := dockerfileSources(tc.text)
		if (err != nil) != tc.reject || !tc.reject && !reflect.DeepEqual(sources, tc.sources) {
			t.Errorf("source policy: %q: %v %v", tc.text, sources, err)
		}
	}
}

func TestBuildCommandsPreserveExactParentResourcesAndNetwork(t *testing.T) {
	e := Exact{WorkspaceKey(strings.Repeat("a", 64)), Generation(strings.Repeat("b", 64))}
	p := NewPodman("/usr/bin/podman", testImage)
	parent := buildUnit(e) + ".slice:mcpws:" + buildUnit(e)
	args := p.buildArgs(e, "/data/exact", parent)
	for _, required := range []string{"--network=" + buildNetwork(e), "--cgroup-parent=" + parent,
		"--dns=1.1.1.1", "--dns=8.8.8.8", "--memory=4294967296", "--memory-swap=4294967296",
		"--ulimit=nofile=1024:1024", "--pull=never", "--http-proxy=false", "--hooks-dir=/data/exact/hooks",
		"--default-mounts-file=/data/exact/mounts.conf",
		"--label=" + generationLabel + "=" + string(e.Generation), "--layer-label=" + generationLabel + "=" + string(e.Generation)} {
		if !slices.Contains(args, required) {
			t.Errorf("missing %s", required)
		}
	}
	props := buildParentProperties(ReferencePolicy())
	for _, required := range []string{"TasksMax=512", "MemoryMax=4294967296", "MemorySwapMax=0", "CPUWeight=100"} {
		if !slices.Contains(props, required) {
			t.Errorf("missing parent %s", required)
		}
	}
	if slices.Contains(props, "TasksMax=513") {
		t.Fatal("migration tolerance became configured PID limit")
	}
	command := buildUnitCommand(context.Background(), e, "build", "/data/exact", ReferencePolicy(), "/usr/bin/podman", args...)
	for _, required := range []string{"--user", "--pipe", "--wait", "--slice=" + buildUnit(e) + ".slice", "--property=LimitNOFILE=1024", "--setenv=TMPDIR=/data/exact/tmp"} {
		if !slices.Contains(command.Args, required) {
			t.Errorf("missing worker %s", required)
		}
	}
}

func TestBuildStreamDrainsBeyondRetentionAndAfterWriteFailure(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		f, err := os.CreateTemp(t.TempDir(), "diagnostics")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		log := &buildLog{file: f}
		if failWrite {
			f.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		command := hostCommand(ctx, "/bin/sh", "-c", "head -c 4194304 /dev/zero; head -c 4194304 /dev/zero >&2")
		command.Stdout, command.Stderr = log, log
		err = command.Run()
		cancel()
		if err != nil {
			t.Fatal("pipe did not drain", err)
		}
		info, err := os.Stat(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if failWrite {
			if log.err == nil || info.Size() != 0 {
				t.Fatal("drain write failure hidden")
			}
		} else if log.err != nil || info.Size() != retainedBuildBytes {
			t.Fatal("retention bound", info.Size(), log.err)
		}
	}
}

func TestSavedBuildahOwnershipRequiresExactNetworkAndImmutableID(t *testing.T) {
	e := Exact{WorkspaceKey(strings.Repeat("a", 64)), Generation(strings.Repeat("b", 64))}
	id := strings.Repeat("c", 64)
	for _, mode := range []string{"exact", "different-generation", "wrong-id", "missing", "duplicate", "host", "malformed-network"} {
		t.Run(mode, func(t *testing.T) {
			owner := e
			if mode == "different-generation" {
				owner.Generation = Generation(strings.Repeat("d", 64))
			}
			ns := map[string]any{"Name": "network", "Host": false, "Path": buildNetwork(owner)}
			namespaces := []any{ns}
			containerID := id
			switch mode {
			case "wrong-id":
				containerID = strings.Repeat("e", 64)
			case "missing":
				namespaces = nil
			case "duplicate":
				namespaces = append(namespaces, ns)
			case "host":
				ns["Host"] = true
			case "malformed-network":
				ns["Path"] = "pasta:unknown"
			}
			data, _ := json.Marshal(map[string]any{"ContainerID": containerID, "NamespaceOptions": namespaces})
			got, err := buildResidueOwner(id, data)
			if mode == "exact" || mode == "different-generation" {
				if err != nil || got != owner {
					t.Fatal(got, err)
				}
			} else if !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatal("ambiguous residue authorized", got, err)
			}
		})
	}
}

func TestEmptyBuildahInventory(t *testing.T) {
	for _, data := range []string{"null\n", "[]"} {
		items, err := buildahInventory([]byte(data))
		if err != nil || len(items) != 0 {
			t.Fatal("empty engine inventory rejected", err)
		}
	}
	if _, err := buildahInventory([]byte(`{}`)); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal("invalid inventory accepted", err)
	}
}

func TestExactDataCleanupRepairsOnlyOwnedGeneration(t *testing.T) {
	e := Exact{WorkspaceKey(strings.Repeat("a", 64)), Generation(strings.Repeat("b", 64))}
	pool := t.TempDir()
	root := buildDirectory(pool, e)
	if err := os.MkdirAll(filepath.Join(root, "restricted", "sealed"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "restricted", "sealed", "file")
	if err := os.WriteFile(file, []byte("exact"), 0600); err != nil {
		t.Fatal(err)
	}
	neighbor := filepath.Join(pool, "neighbor")
	if err := os.WriteFile(neighbor, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(neighbor, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{filepath.Join(root, "restricted"): 0100, filepath.Join(root, "restricted", "sealed"): 0000} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeBuildDirectory(pool, e); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("exact root remains", err)
	}
	if content, err := os.ReadFile(neighbor); err != nil || string(content) != "untouched" {
		t.Fatal("neighbor changed", err)
	}
}

func TestExternalAndFinalImageOwnershipAreDistinct(t *testing.T) {
	e := Exact{WorkspaceKey(strings.Repeat("a", 64)), Generation(strings.Repeat("b", 64))}
	v := imageInspection{ID: "sha256:" + strings.Repeat("c", 64)}
	if !externalImage(v) || ownedImage(v, e) {
		t.Fatal("external image classification")
	}
	v.Config.Labels = objectLabels(e, buildImageName(e))
	if externalImage(v) || !ownedImage(v, e) {
		t.Fatal("generation image accepted as input or lost ownership")
	}
	other := e
	other.Generation = Generation(strings.Repeat("d", 64))
	if ownedImage(v, other) {
		t.Fatal("image adopted by another generation")
	}
	v.Config.Labels = nil
	v.Config.OnBuild = []string{"COPY --from=unclassified /x /y"}
	if externalImage(v) {
		t.Fatal("inherited unclassified consumption")
	}
	container := containerInspection{ID: strings.Repeat("e", 64), Name: objectName(e), ImageName: v.ID, Image: v.ID}
	container.Config.Labels = objectLabels(e, v.ID)
	if _, owned, err := classify(container.object()); err != nil || !owned {
		t.Fatal("exact final image not recognized", err)
	}
	container.Image = "sha256:" + strings.Repeat("f", 64)
	if _, _, err := classify(container.object()); err == nil {
		t.Fatal("mutable image name substituted for actual image identity")
	}
}

func TestBuildHostDNSHasNoPrivateDestinationException(t *testing.T) {
	for _, tc := range []struct {
		text    string
		allowed bool
	}{
		{"nameserver 1.1.1.1\nnameserver 2606:4700:4700::1111\n", true},
		{"nameserver 127.0.0.53\n", false}, {"nameserver 169.254.169.254\n", false},
		{"nameserver 10.0.0.1\n", false}, {"nameserver fd00::1\n", false}, {"", false},
	} {
		if err := publicResolvers(strings.NewReader(tc.text)); (err == nil) != tc.allowed {
			t.Errorf("DNS %q: %v", tc.text, err)
		}
	}
}

func TestGenerationImagesRequireOwnershipAndRemoveChildrenFirst(t *testing.T) {
	e := Exact{WorkspaceKey(strings.Repeat("a", 64)), Generation(strings.Repeat("b", 64))}
	base, stage, final := strings.Repeat("c", 64), strings.Repeat("d", 64), strings.Repeat("e", 64)
	labels := objectLabels(e, buildImageName(e))
	imageJSON := func(id string, labels map[string]string) string {
		return jsonText(t, []any{map[string]any{"Id": id, "Config": map[string]any{"Labels": labels}}})
	}
	listArgs := []string{"image", "list", "--all", "--no-trunc", "--format=json"}
	p, _ := podmanStub(t,
		commandStep{args: listArgs, output: jsonText(t, []any{
			map[string]any{"Id": base}, // shared base is never deletion authority
			map[string]any{"Id": stage, "ParentId": base, "Labels": labels},
			map[string]any{"Id": final, "ParentId": stage, "Labels": labels},
		})},
		commandStep{args: []string{"image", "inspect", "--", "sha256:" + stage}, output: imageJSON(stage, labels)},
		commandStep{args: []string{"image", "inspect", "--", "sha256:" + final}, output: imageJSON(final, labels)})
	ids, err := p.generationImages(context.Background(), e, "sha256:"+final)
	if err != nil || !slices.Equal(ids, []string{"sha256:" + final, "sha256:" + stage}) {
		t.Fatal("exact image cleanup order", ids, err)
	}
	p, _ = podmanStub(t,
		commandStep{args: listArgs, output: jsonText(t, []any{map[string]any{"Id": final}})},
		commandStep{args: []string{"image", "inspect", "--", "sha256:" + final}, output: imageJSON(final, nil)})
	if _, err := p.generationImages(context.Background(), e, "sha256:"+final); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal("recorded image with lost ownership silently ignored", err)
	}
}
