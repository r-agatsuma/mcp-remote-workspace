package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

func buildDirectory(dataDir string, e Exact) string {
	return filepath.Join(dataDir, "builds", objectName(e))
}

func buildImageName(e Exact) string { return "localhost/" + objectName(e) + ":latest" }

// The guest DHCP hostname carries an exact ownership token in Buildah's saved
// NamespaceOptions. It selects no host namespace, path, or destination exception.
func buildNetwork(e Exact) string {
	return "pasta:--no-map-gw,-t,none,-u,none,-T,none,-U,none,--hostname," + objectName(e)
}

type imageInspection struct {
	ID     string `json:"Id"`
	Config struct {
		Labels  map[string]string
		OnBuild []string
	}
}

func (p *Podman) inspectImage(ctx context.Context, ref string) (imageInspection, error) {
	data, err := p.runner.run(ctx, "image", "inspect", "--", ref)
	var images []imageInspection
	if err != nil || json.Unmarshal(data, &images) != nil || len(images) != 1 || !validHex(strings.TrimPrefix(images[0].ID, "sha256:")) {
		return imageInspection{}, errors.New("image identity could not be inspected")
	}
	images[0].ID = "sha256:" + strings.TrimPrefix(images[0].ID, "sha256:")
	return images[0], nil
}

func ownedImage(v imageInspection, e Exact) bool {
	labels := v.Config.Labels
	return immutableImage(v.ID) && labels[ownerLabel] == ownerValue &&
		labels[workspaceLabel] == string(e.Workspace) && labels[generationLabel] == string(e.Generation) && labels[imageLabel] == buildImageName(e)
}

func (p *Podman) confirmBuiltImage(ctx context.Context, e Exact, id string) error {
	v, err := p.inspectImage(ctx, id)
	if err != nil || v.ID != id || !ownedImage(v, e) {
		return errors.New("image is not bound to the exact candidate")
	}
	return nil
}

func externalImage(v imageInspection) bool {
	for label := range v.Config.Labels {
		if strings.HasPrefix(label, labelPrefix) {
			return false
		}
	}
	// Inherited ONBUILD executes additional instructions invisible in the
	// supplied text. Refuse it rather than consuming unclassified image inputs
	// or allowing an inherited RUN isolation override.
	return len(v.Config.OnBuild) == 0
}

func (p *Podman) buildArgs(e Exact, directory, parent string) []string {
	policy := p.resourcePolicy()
	args := []string{"build", "--file=" + filepath.Join(directory, "context", "Dockerfile"),
		"--tag=" + buildImageName(e), "--iidfile=" + filepath.Join(directory, "image-id"),
		"--network=" + buildNetwork(e), "--cgroup-parent=" + parent,
		"--isolation=oci", "--cgroupns=private", "--pid=private", "--ipc=private", "--uts=private",
		"--pull=never", "--layers=false", "--no-cache", "--force-rm", "--jobs=1",
		"--http-proxy=false", "--no-hosts", "--dns-search=.",
		"--cap-drop=ALL", "--security-opt=no-new-privileges", "--hooks-dir=" + filepath.Join(directory, "hooks"),
		"--default-mounts-file=" + filepath.Join(directory, "mounts.conf"),
		"--memory=" + strconv.FormatInt(policy.MemoryBytes, 10),
		"--memory-swap=" + strconv.FormatInt(policy.MemoryBytes+policy.SwapBytes, 10),
		"--cpu-quota=" + strconv.FormatInt(policy.CPUQuota, 10),
		"--ulimit=nofile=" + strconv.FormatUint(policy.NoFile, 10) + ":" + strconv.FormatUint(policy.NoFile, 10)}
	for _, dns := range p.publicDNS {
		args = append(args, "--dns="+dns)
	}
	labels := objectLabels(e, buildImageName(e))
	for _, key := range []string{ownerLabel, workspaceLabel, generationLabel, imageLabel} {
		args = append(args, "--label="+key+"="+labels[key], "--layer-label="+key+"="+labels[key])
	}
	return append(args, "--", filepath.Join(directory, "context"))
}

func (p *Podman) Build(ctx context.Context, e Exact, text, dataDir string) (image string, err error) {
	if !p.initialized || e.validate() != nil || !filepath.IsAbs(dataDir) || len(p.publicDNS) == 0 {
		return "", CompletedBackendError(ErrNotReady)
	}
	cli, ok := p.runner.(localCLI)
	if !ok {
		return "", CompletedBackendError(ErrNotReady)
	}
	sources, err := dockerfileSources(text)
	if err != nil {
		return "", CompletedBackendError(err)
	}
	if err := p.buildStorage(ctx, dataDir); err != nil {
		return "", CompletedBackendError(err)
	}
	if err := buildHostDNS(); err != nil {
		return "", CompletedBackendError(err)
	}
	directory := buildDirectory(dataDir, e)
	if err := privateDir(filepath.Dir(directory)); err != nil {
		return "", CompletedBackendError(err)
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return "", CompletedBackendError(err)
	}
	for _, sub := range []string{"context", "tmp", "hooks"} {
		if err := os.Mkdir(filepath.Join(directory, sub), 0700); err != nil {
			return "", CompletedBackendError(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "context", "Dockerfile"), []byte(text), 0600); err != nil {
		return "", CompletedBackendError(err)
	}
	// No host credentials are passed to image acquisition or untrusted RUN.
	if err := os.WriteFile(filepath.Join(directory, "auth.json"), []byte(`{"auths":{}}`), 0600); err != nil {
		return "", CompletedBackendError(err)
	}
	// An explicit empty policy replaces ambient user/system mounts.conf discovery.
	if err := os.WriteFile(filepath.Join(directory, "mounts.conf"), nil, 0600); err != nil {
		return "", CompletedBackendError(err)
	}
	f, err := os.OpenFile(filepath.Join(directory, "diagnostics"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", CompletedBackendError(err)
	}
	log := &buildLog{file: f}
	defer func() { err = errors.Join(err, CompletedBackendError(f.Close())) }()
	parent, err := p.startBuildParent(ctx, e)
	// An attempted parent creation can have partially succeeded. Regardless of
	// caller cancellation, use bounded trusted control outside the parent to stop
	// it and attest terminal work. A failed stop retains the backend permit.
	defer func() {
		control, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if stopErr := p.stopBuildParent(control, e); stopErr != nil {
			err = errors.Join(ErrOutcomeUnknown, stopErr, err)
		} else if err != nil {
			err = CompletedBackendError(err)
		}
	}()
	if err != nil {
		return "", err
	}
	run := func(unit string, args ...string) error {
		args = append([]string{"--remote=false", "--cgroup-manager=systemd", "--tmpdir=" + filepath.Join(directory, "tmp")}, args...)
		command := buildUnitCommand(ctx, e, unit, directory, p.resourcePolicy(), cli.path, args...)
		command.Stdout, command.Stderr = log, log
		return command.Run() // joins both continuous bounded pipe drains
	}
	for index, source := range sources {
		_, existsErr := p.runner.run(ctx, "image", "exists", "--", source)
		if existsErr != nil {
			var exit *exec.ExitError
			if !errors.As(existsErr, &exit) || exit.ExitCode() != 1 {
				return "", existsErr
			}
			if err := run("pull"+strconv.Itoa(index), "pull", "--authfile="+filepath.Join(directory, "auth.json"), "--", source); err != nil {
				return "", err
			}
		}
		v, err := p.inspectImage(ctx, source)
		if err != nil || !externalImage(v) {
			return "", ErrDockerfile
		}
	}
	args := p.buildArgs(e, directory, parent)
	args = append(args[:1], append([]string{"--authfile=" + filepath.Join(directory, "auth.json")}, args[1:]...)...)
	if err := run("build", args...); err != nil {
		return "", err
	}
	if log.err != nil || f.Sync() != nil {
		return "", errors.New("build diagnostics drain failed")
	}
	id, err := os.ReadFile(filepath.Join(directory, "image-id"))
	if err != nil || len(id) > 128 {
		return "", errors.New("missing exact build image identity")
	}
	image = strings.TrimSpace(string(id))
	if err := p.confirmBuiltImage(ctx, e, image); err != nil {
		return "", err
	}
	return image, nil
}

type buildahContainer struct {
	ID      string
	Builder bool
}

func buildahInventory(data []byte) ([]buildahContainer, error) {
	var items []buildahContainer
	// Buildah marshals its nil slice as null when storage has no containers.
	if json.Unmarshal(data, &items) != nil {
		return nil, ErrOutcomeUnknown
	}
	return items, nil
}

// Buildah working containers are deleted solely by immutable IDs whose saved
// network NamespaceOption exactly equals this generation's trusted pasta string.
// Inventory only discovers candidates; it is never deletion authority.
func (p *Podman) buildResidue(ctx context.Context, e Exact) ([]string, error) {
	data, err := runHostCommand(ctx, "/usr/bin/buildah", "containers", "--all", "--json")
	if err != nil {
		return nil, err
	}
	items, err := buildahInventory(data)
	if err != nil {
		return nil, err
	}
	var ids []string
	regular, err := p.List(ctx)
	if err != nil {
		return nil, err
	}
	regularIDs := make(map[string]bool)
	for _, object := range regular {
		regularIDs[object.ID] = true
	}
	for _, item := range items {
		if !validHex(item.ID) {
			return nil, ErrOutcomeUnknown
		}
		if !item.Builder {
			if !regularIDs[item.ID] {
				return nil, ErrOutcomeUnknown
			}
			continue
		}
		data, err := runHostCommand(ctx, "/usr/bin/buildah", "inspect", "--type=container", "--", item.ID)
		if err != nil {
			return nil, err
		}
		owner, err := buildResidueOwner(item.ID, data)
		if err != nil {
			return nil, err
		}
		if owner == e {
			ids = append(ids, item.ID)
		}
	}
	return ids, nil
}

func buildResidueOwner(id string, data []byte) (Exact, error) {
	var saved struct {
		ContainerID      string
		NamespaceOptions []struct {
			Name string
			Host bool
			Path string
		}
	}
	if !validHex(id) || json.Unmarshal(data, &saved) != nil || saved.ContainerID != id {
		return Exact{}, ErrOutcomeUnknown
	}
	var owner Exact
	for _, ns := range saved.NamespaceOptions {
		if ns.Name != "network" {
			continue
		}
		if owner != (Exact{}) || ns.Host {
			return Exact{}, ErrOutcomeUnknown
		}
		_, suffix, ok := strings.Cut(ns.Path, "--hostname,"+namePrefix)
		workspace, generation, split := strings.Cut(suffix, "-")
		candidate := Exact{WorkspaceKey(workspace), Generation(generation)}
		if !ok || !split || candidate.validate() != nil || ns.Path != buildNetwork(candidate) {
			return Exact{}, ErrOutcomeUnknown
		}
		owner = candidate
	}
	if owner == (Exact{}) {
		return Exact{}, ErrOutcomeUnknown
	}
	return owner, nil
}

func (p *Podman) generationImages(ctx context.Context, e Exact, recorded string) ([]string, error) {
	data, err := p.runner.run(ctx, "image", "list", "--all", "--no-trunc", "--format=json")
	if err != nil {
		return nil, err
	}
	var items []struct {
		ID     string `json:"Id"`
		Parent string `json:"ParentId"`
		Labels map[string]string
		Names  []string
	}
	if json.Unmarshal(data, &items) != nil || items == nil {
		return nil, ErrOutcomeUnknown
	}
	var ids []string
	parents := make(map[string]string)
	for _, item := range items {
		if !validHex(strings.TrimPrefix(item.ID, "sha256:")) {
			return nil, ErrOutcomeUnknown
		}
		id := "sha256:" + strings.TrimPrefix(item.ID, "sha256:")
		claimed := id == recorded || item.Labels[workspaceLabel] == string(e.Workspace) && item.Labels[generationLabel] == string(e.Generation)
		for _, name := range item.Names {
			claimed = claimed || name == buildImageName(e)
		}
		if !claimed {
			continue
		}
		v, err := p.inspectImage(ctx, id)
		if err != nil || v.ID != id || !ownedImage(v, e) {
			return nil, ErrOutcomeUnknown
		}
		ids = append(ids, id)
		parents[id] = "sha256:" + strings.TrimPrefix(item.Parent, "sha256:")
	}
	// Podman lists timestamps at second precision, so creation order cannot
	// reliably put children before parents. Order only already-authorized IDs;
	// parent metadata supplies no additional deletion authority.
	var ordered []string
	visited := make(map[string]uint8)
	var visit func(string) bool
	visit = func(id string) bool {
		parent, owned := parents[id]
		if !owned || visited[id] == 2 {
			return true
		}
		if visited[id] == 1 {
			return false
		}
		visited[id] = 1
		if !visit(parent) {
			return false
		}
		visited[id] = 2
		ordered = append(ordered, id)
		return true
	}
	for _, id := range ids {
		if !visit(id) {
			return nil, ErrOutcomeUnknown
		}
	}
	slices.Reverse(ordered)
	return ordered, nil
}

func (p *Podman) CleanupBuild(ctx context.Context, e Exact, dataDir, image string) error {
	if e.validate() != nil {
		return ErrStaleGeneration
	}
	if err := p.stopBuildParent(ctx, e); err != nil {
		return err
	}
	ids, err := p.buildResidue(ctx, e)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := runHostCommand(ctx, "/usr/bin/buildah", "rm", "--", id); err != nil {
			return err
		}
	}
	if ids, err = p.buildResidue(ctx, e); err != nil || len(ids) != 0 {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	images, err := p.generationImages(ctx, e, image)
	if err != nil {
		return err
	}
	for _, id := range images {
		if _, err := p.runner.run(ctx, "image", "rm", "--no-prune", "--", id); err != nil {
			return err
		}
	}
	if images, err = p.generationImages(ctx, e, image); err != nil || len(images) != 0 {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if err := removeBuildDirectory(dataDir, e); err != nil {
		return err
	}
	if _, err := os.Lstat(buildDirectory(dataDir, e)); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("exact build Data absence unconfirmed: %w", err)
	}
	return nil
}
