package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var _ BuildRuntime = (*Podman)(nil)

type imageInspection struct {
	ID                                  string `json:"Id"`
	Digest                              string
	RepoTags, RepoDigests, NamesHistory []string
	Labels                              map[string]string
	Config                              *struct {
		Labels  map[string]string
		OnBuild []string
	}
}

func (v imageInspection) labels() (map[string]string, error) {
	labels := map[string]string{}
	for k, value := range v.Labels {
		labels[k] = value
	}
	if v.Config == nil {
		return nil, errors.New("image config missing")
	}
	for k, value := range v.Config.Labels {
		if prior, ok := labels[k]; ok && prior != value {
			return nil, ErrDockerfilePolicy
		}
		labels[k] = value
	}
	return labels, nil
}
func imageID(value string) (string, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "sha256:")
	if !validHex(value) {
		return "", ErrDockerfilePolicy
	}
	return value, nil
}
func (p *Podman) inspectImage(ctx context.Context, id string) (imageInspection, error) {
	data, err := p.runner.run(ctx, "image", "inspect", "--", id)
	if err != nil {
		return imageInspection{}, err
	}
	var values []imageInspection
	if json.Unmarshal(data, &values) != nil || len(values) != 1 {
		return imageInspection{}, ErrDockerfilePolicy
	}
	actual, err := imageID(values[0].ID)
	if err != nil {
		return imageInspection{}, err
	}
	if expected, err := imageID(id); err == nil && expected != actual {
		return imageInspection{}, ErrDockerfilePolicy
	}
	values[0].ID = actual
	if _, err := values[0].labels(); err != nil {
		return imageInspection{}, err
	}
	return values[0], nil
}
func (p *Podman) allImages(ctx context.Context) ([]imageInspection, error) {
	if !p.initialized {
		return nil, ErrNotReady
	}
	data, err := p.runner.run(ctx, "image", "list", "--all", "--no-trunc", "--format=json")
	if err != nil {
		return nil, err
	}
	var listed []struct {
		ID string `json:"Id"`
	}
	if json.Unmarshal(data, &listed) != nil || listed == nil || len(listed) > 4096 {
		return nil, ErrDockerfilePolicy
	}
	result := []imageInspection{}
	seen := map[string]bool{}
	for _, item := range listed {
		id, err := imageID(item.ID)
		if err != nil {
			return nil, err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		v, err := p.inspectImage(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, nil
}
func buildLabelOwner(labels map[string]string) (Exact, bool, error) {
	claimed := false
	for k := range labels {
		claimed = claimed || strings.HasPrefix(k, labelPrefix)
	}
	if !claimed {
		return Exact{}, false, nil
	}
	e := Exact{WorkspaceKey(labels[workspaceLabel]), Generation(labels[generationLabel])}
	if e.validate() != nil || labels[ownerLabel] != ownerValue || labels[imageLabel] != buildImageName(e) {
		return Exact{}, false, ErrDockerfilePolicy
	}
	for k := range labels {
		if strings.HasPrefix(k, labelPrefix) && k != ownerLabel && k != workspaceLabel && k != generationLabel && k != imageLabel {
			return Exact{}, false, ErrDockerfilePolicy
		}
	}
	return e, true, nil
}
func ownedBuildImage(v imageInspection) (Exact, bool, error) {
	labels, err := v.labels()
	if err != nil {
		return Exact{}, false, err
	}
	e, owned, err := buildLabelOwner(labels)
	if err != nil {
		return Exact{}, false, err
	}
	for _, tag := range append(append(append([]string{}, v.RepoTags...), v.RepoDigests...), v.NamesHistory...) {
		if strings.Contains(tag, "/"+generationImageNamespace+"/") && !owned {
			return Exact{}, false, ErrDockerfilePolicy
		}
	}
	return e, owned, nil
}

func (p *Podman) KnownBuildImages(ctx context.Context) ([]string, error) {
	values, err := p.allImages(ctx)
	if err != nil {
		return nil, err
	}
	known := []string{}
	for _, v := range values {
		_, owned, err := ownedBuildImage(v)
		if err != nil {
			return nil, err
		}
		if owned {
			known = append(known, "sha256:"+v.ID)
			known = append(known, v.RepoTags...)
			known = append(known, v.RepoDigests...)
			known = append(known, v.NamesHistory...)
			if v.Digest != "" {
				known = append(known, "sha256:"+strings.TrimPrefix(v.Digest, "sha256:"))
			}
		}
	}
	return known, nil
}

func (p *Podman) BuildGenerations(ctx context.Context) ([]Exact, error) {
	values, err := p.allImages(ctx)
	if err != nil {
		return nil, err
	}
	containers, err := p.externalBuildContainers(ctx)
	if err != nil {
		return nil, err
	}
	generations := []Exact{}
	seen := map[Exact]bool{}
	for _, v := range values {
		e, owned, err := ownedBuildImage(v)
		if err != nil {
			return nil, err
		}
		if owned && !seen[e] {
			seen[e] = true
			generations = append(generations, e)
		}
	}
	for _, v := range containers {
		if !seen[v.Exact] {
			seen[v.Exact] = true
			generations = append(generations, v.Exact)
		}
	}
	return generations, nil
}

// Build uses the existing rootless host adapter. Network connections (registry
// fetches and RUN's rootless user-mode networking) inherit the root-managed host
// OUTPUT policy for the numeric mcp-runner UID. No proxy, DNS authority or new
// privilege is introduced. Effective enforcement is real-host #72 evidence.
func (p *Podman) Build(ctx context.Context, e Exact, plan BuildPlan, path string, stdout, stderr io.Writer) BackendResult {
	fail := func(err error) BackendResult { return BackendResult{DefinitelyCompleted, err} }
	if !p.initialized || e.validate() != nil || p.buildConfig.DataDir == "" || path != p.candidateBuildPath(e) || p.buildah == nil {
		return fail(ErrNotReady)
	}
	if err := p.buildConfig.Validate(); err != nil {
		return fail(err)
	}
	// Recheck known aliases immediately before any external consumption.
	known, err := p.KnownBuildImages(ctx)
	if err != nil {
		return fail(err)
	}
	checked, err := compileDockerfile(plan.dockerfile, known)
	if err != nil {
		return fail(err)
	}
	pinned := map[string]string{}
	for _, source := range checked.sources {
		if _, ok := pinned[source]; ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		// Pull only a checked explicit-registry distribution reference. Building
		// never falls back to local names, mirrors of service images, or short search.
		data, err := p.runner.run(ctx, "pull", "--quiet", "--tls-verify=true", "--", source)
		if err != nil {
			return fail(err)
		}
		id, err := imageID(string(data))
		if err != nil {
			return fail(err)
		}
		v, err := p.inspectImage(ctx, id)
		if err != nil {
			return fail(err)
		}
		_, owned, err := ownedBuildImage(v)
		if err != nil || owned {
			return fail(ErrDockerfilePolicy)
		}
		for _, old := range known {
			if old == "sha256:"+id {
				return fail(ErrDockerfilePolicy)
			}
		}
		// Base-image triggers execute inside the build frontend. Reject them before
		// build execution instead of allowing hidden external COPY/RUN mounts.
		if len(v.Config.OnBuild) > 0 {
			return fail(ErrDockerfilePolicy)
		}
		pinned[source] = "sha256:" + id
	}
	lines := strings.Split(checked.dockerfile, "\n")
	for n, line := range lines {
		if strings.HasPrefix(line, "FROM ") {
			parts := strings.Fields(line)
			if ref, ok := pinned[parts[1]]; ok {
				parts[1] = ref
			}
			lines[n] = strings.Join(parts, " ")
		}
		if strings.HasPrefix(line, "COPY --from=") {
			parts := strings.SplitN(line, " ", 3)
			if ref, ok := pinned[strings.TrimPrefix(parts[1], "--from=")]; ok {
				parts[1] = "--from=" + ref
			}
			lines[n] = strings.Join(parts, " ")
		}
		// --from may follow --chown or --chmod. Replace only compiler-created flags.
		if strings.HasPrefix(line, "COPY ") {
			for source, id := range pinned {
				lines[n] = strings.ReplaceAll(lines[n], "--from="+source+" ", "--from="+id+" ")
			}
		}
	}
	dockerfile := strings.Join(lines, "\n")
	runner, ok := p.runner.(buildCommandRunner)
	if !ok {
		return fail(ErrNotReady)
	}
	// Context is service-created and contains ONLY Dockerfile. Logs and temp state
	// live outside context and can never become arbitrary caller COPY inputs.
	contextPath := filepath.Join(path, "context")
	entries, err := os.ReadDir(contextPath)
	if err != nil || len(entries) != 1 || entries[0].Name() != "Dockerfile" || entries[0].Type()&os.ModeSymlink != 0 || entries[0].IsDir() {
		return fail(ErrDockerfilePolicy)
	}
	// Persist exact attribution in Buildah's namespace options at allocation,
	// before RUN. The module clears inherited pasta mapping/forwarding options.
	module, err := p.prepareBuildAttribution(e, path)
	if err != nil {
		return fail(err)
	}
	args := []string{"--module=" + module, "build", "--file=-", "--pull=never", "--layers=false", "--no-cache", "--rm=true", "--force-rm=true", "--jobs=1", "--isolation=rootless", "--network=" + p.buildNetwork(e), "--http-proxy=false", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--tag=" + buildImageName(e)}
	for _, resolver := range p.buildConfig.DNS {
		ip, _ := netip.ParseAddr(resolver)
		args = append(args, "--dns="+ip.Unmap().String())
	}
	policy := p.resourcePolicy()
	args = append(args, "--memory="+strconv.FormatInt(policy.MemoryBytes, 10), "--memory-swap="+strconv.FormatInt(policy.MemoryBytes+policy.SwapBytes, 10), "--cpu-quota="+strconv.FormatInt(policy.CPUQuota, 10), "--ulimit=nofile="+strconv.FormatUint(policy.NoFile, 10)+":"+strconv.FormatUint(policy.NoFile, 10))
	labels := objectLabels(e, buildImageName(e))
	keys := []string{}
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--label="+key+"="+labels[key], "--layer-label="+key+"="+labels[key])
	}
	args = append(args, "--", contextPath)
	result := runner.runBuild(ctx, dockerfile, filepath.Join(path, "tmp"), stdout, stderr, args...)
	if result.Err != nil || result.Completion != DefinitelyCompleted {
		return result
	}
	v, err := p.inspectImage(ctx, buildImageName(e))
	if err != nil {
		return fail(err)
	}
	actual, owned, err := ownedBuildImage(v)
	if err != nil || !owned || actual != e {
		return fail(ErrDockerfilePolicy)
	}
	return result
}

type buildCommandRunner interface {
	runBuild(context.Context, string, string, io.Writer, io.Writer, ...string) BackendResult
}

func (c localCLI) runBuild(ctx context.Context, dockerfile, temp string, stdout, stderr io.Writer, args ...string) BackendResult {
	cmd := exec.CommandContext(ctx, c.path, append([]string{"--remote=false"}, args...)...)
	for _, key := range []string{"HOME", "PATH", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "TMPDIR="+temp)
	cmd.Stdin = strings.NewReader(dockerfile)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return BackendResult{DefinitelyCompleted, err}
	}
	err := cmd.Wait()
	// CLI exit/cancellation alone cannot attest that Buildah's backend containers
	// disappeared. Definite success is followed by exact cleanup on destroy;
	// abnormal exit remains uncertain until trusted host recovery.
	if ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay) {
		return BackendResult{OutcomeUnknown, ErrOutcomeUnknown}
	}
	if err != nil {
		return BackendResult{OutcomeUnknown, ErrOutcomeUnknown}
	}
	return BackendResult{DefinitelyCompleted, nil}
}

// An unowned dangling image may be unfinished build residue. Its absence of
// names/ownership is not evidence that it belongs to someone else.
func unidentifiedBuildImage(v imageInspection, owned bool) bool {
	if owned {
		return false
	}
	for _, name := range append(append([]string{}, v.RepoTags...), v.RepoDigests...) {
		if name != "" && name != "<none>" && name != "<none>:<none>" {
			return false
		}
	}
	return true
}

// CleanupBuild is intentionally conservative: unidentified external Buildah
// containers prevent an absence claim. It never guesses ownership or prunes.
func (p *Podman) CleanupBuild(ctx context.Context, e Exact, path string) (err error) {
	defer func() { err = localRuntimeError(ctx, err) }()
	if !p.initialized || e.validate() != nil {
		return ErrNotReady
	}
	if p.buildConfig.DataDir == "" || path != p.candidateBuildPath(e) {
		return ErrNotReady
	}
	objects, err := p.externalBuildContainers(ctx)
	if err != nil {
		return err
	}
	for _, item := range objects {
		if item.Exact == e {
			if _, err := p.buildah.run(ctx, "rm", "--", item.ID); err != nil {
				return err
			}
		}
	}
	images, err := p.allImages(ctx)
	if err != nil {
		return err
	}
	for _, v := range images {
		actual, owned, err := ownedBuildImage(v)
		if err != nil {
			return err
		}
		if unidentifiedBuildImage(v, owned) {
			return ErrOutcomeUnknown
		}
		if owned && actual == e {
			if _, err := p.runner.run(ctx, "image", "rm", "--no-prune", "--", v.ID); err != nil {
				return err
			}
		}
	}
	// Independent readback establishes absence. A mutation's exit status alone
	// cannot remove generation intent, including failed/stale image residue.
	images, err = p.allImages(ctx)
	if err != nil {
		return err
	}
	for _, v := range images {
		actual, owned, err := ownedBuildImage(v)
		if err != nil {
			return err
		}
		if unidentifiedBuildImage(v, owned) {
			return ErrOutcomeUnknown
		}
		if owned && actual == e {
			return ErrOutcomeUnknown
		}
	}
	objects, err = p.externalBuildContainers(ctx)
	if err != nil {
		return err
	}
	for _, item := range objects {
		if item.Exact == e {
			return ErrOutcomeUnknown
		}
	}
	return nil
}
