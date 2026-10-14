package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type localBuildahCLI struct{ path string }

func (c localBuildahCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	if os.Geteuid() == 0 || !filepath.IsAbs(c.path) {
		return nil, ErrNotReady
	}
	cmd := exec.CommandContext(ctx, c.path, args...)
	for _, key := range []string{"HOME", "PATH", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.WaitDelay = time.Second
	var stdout, stderr boundedOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	return stdout.Bytes(), nil
}

const (
	buildPIDFile       = "pasta.pid"
	buildNetworkPrefix = "pasta:--pid,"
	buildPastaOptions  = ",--no-map-gw,-t,none,-u,none,-T,none,-U,none"
	buildNetworkModule = "[network]\npasta_options = [{append = false}]\n"
)

func (p *Podman) candidateBuildPath(e Exact) string {
	return filepath.Join(p.buildConfig.DataDir, "builds", objectName(e))
}

// Buildah saves NamespaceOptions in NewBuilder before executing any RUN, and
// exposes them through buildah inspect. The exact service-owned PID-file path
// is operational metadata for pasta as well as a candidate attribution token.
// Never use the PID's value, container name, image labels or inventory deltas
// as deletion authority. All stages (including external COPY) inherit this CLI
// setting; Dockerfile RUN network overrides are rejected by the compiler.
func (p *Podman) buildNetwork(e Exact) string {
	return buildNetworkPrefix + filepath.Join(p.candidateBuildPath(e), buildPIDFile) + buildPastaOptions
}

func (p *Podman) prepareBuildAttribution(e Exact, path string) (string, error) {
	if path != p.candidateBuildPath(e) {
		return "", ErrNotReady
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ownedByService(info) {
		return "", ErrNotReady
	}
	module := filepath.Join(path, "build-container.conf")
	if _, err := os.Lstat(filepath.Join(path, buildPIDFile)); !errors.Is(err, os.ErrNotExist) {
		return "", ErrNotReady
	}
	// Reset inherited pasta options so deployment defaults cannot enable host
	// mapping or port forwarding, even if an earlier file enabled array append.
	// The fixed CLI supplies all network options.
	// Exclusive creation refuses stale or substituted metadata, including symlinks.
	f, err := os.OpenFile(module, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	_, writeErr := f.WriteString(buildNetworkModule)
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return "", err
	}
	return module, nil
}

type externalBuildContainer struct {
	ID    string
	Exact Exact
}

// List storage objects separately from ordinary libpod containers. External
// objects must be inspected through Buildah, never podman container inspect.
func (p *Podman) externalBuildContainers(ctx context.Context) ([]externalBuildContainer, error) {
	list := func(external bool) ([]string, error) {
		args := []string{"container", "list", "--all"}
		if external {
			args = append(args, "--external")
		}
		data, err := p.runner.run(ctx, append(args, "--no-trunc", "--format=json")...)
		if err != nil {
			return nil, err
		}
		var values []struct {
			ID string `json:"Id"`
		}
		if json.Unmarshal(data, &values) != nil || values == nil || len(values) > 4096 {
			return nil, ErrOutcomeUnknown
		}
		ids, seen := []string{}, map[string]bool{}
		for _, v := range values {
			if !validHex(v.ID) || seen[v.ID] {
				return nil, ErrOutcomeUnknown
			}
			ids, seen[v.ID] = append(ids, v.ID), true
		}
		return ids, nil
	}
	// Read external inventory first: races/disappearing objects fail closed on
	// exact inspection instead of becoming deletion authority for another object.
	all, err := list(true)
	if err != nil {
		return nil, err
	}
	ordinary, err := list(false)
	if err != nil {
		return nil, err
	}
	regular := map[string]bool{}
	for _, id := range ordinary {
		regular[id] = true
	}
	result := []externalBuildContainer{}
	for _, id := range all {
		if regular[id] {
			continue
		}
		if p.buildah == nil {
			return nil, ErrOutcomeUnknown
		}
		data, err := p.buildah.run(ctx, "inspect", "--type=container", "--", id)
		if err != nil {
			return nil, errors.Join(ErrOutcomeUnknown, err)
		}
		var v struct {
			Type, ContainerID string
			NamespaceOptions  []struct {
				Name, Path string
				Host       *bool
			}
		}
		if json.Unmarshal(data, &v) != nil || v.Type != "buildah 0.0.1" || v.ContainerID != id {
			return nil, ErrOutcomeUnknown
		}
		network := ""
		for _, option := range v.NamespaceOptions {
			if option.Name != "network" {
				continue
			}
			if network != "" || option.Host == nil || *option.Host || option.Path == "" {
				return nil, ErrOutcomeUnknown
			}
			network = option.Path
		}
		marker, ok := strings.CutPrefix(network, buildNetworkPrefix)
		if !ok {
			return nil, ErrOutcomeUnknown
		}
		marker, _, _ = strings.Cut(marker, ",")
		name := filepath.Base(filepath.Dir(marker))
		suffix, ok := strings.CutPrefix(name, namePrefix)
		w, g, split := strings.Cut(suffix, "-")
		e := Exact{WorkspaceKey(w), Generation(g)}
		if !ok || !split || e.validate() != nil || p.buildConfig.DataDir == "" || network != p.buildNetwork(e) {
			return nil, ErrOutcomeUnknown
		}
		result = append(result, externalBuildContainer{id, e})
	}
	return result, nil
}
