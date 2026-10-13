package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

const buildMountsFile = "default-mounts.conf"

func (p *Podman) candidateBuildPath(e Exact) string {
	return filepath.Join(p.buildConfig.DataDir, "builds", objectName(e))
}

func (p *Podman) prepareBuildAttribution(e Exact, path string) (string, error) {
	if path != p.candidateBuildPath(e) {
		return "", ErrNotReady
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ownedByService(info) {
		return "", ErrNotReady
	}
	mounts := filepath.Join(path, buildMountsFile)
	module := filepath.Join(path, "build-container.conf")
	// Exclusive creation refuses stale or substituted metadata, including symlinks.
	for _, file := range []struct{ path, text string }{
		{mounts, ""},
		{module, "[containers]\ndefault_mounts_file = " + strconv.Quote(mounts) + "\n"},
	} {
		f, err := os.OpenFile(file.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", err
		}
		_, writeErr := f.WriteString(file.text)
		err = errors.Join(writeErr, f.Sync(), f.Close())
		if err != nil {
			return "", err
		}
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
		var v struct{ Type, ContainerID, DefaultMountsFilePath string }
		if json.Unmarshal(data, &v) != nil || v.Type != "buildah 0.0.1" || v.ContainerID != id {
			return nil, ErrOutcomeUnknown
		}
		marker := v.DefaultMountsFilePath
		name := filepath.Base(filepath.Dir(marker))
		suffix, ok := strings.CutPrefix(name, namePrefix)
		w, g, split := strings.Cut(suffix, "-")
		e := Exact{WorkspaceKey(w), Generation(g)}
		if !ok || !split || e.validate() != nil || p.buildConfig.DataDir == "" || marker != filepath.Join(p.candidateBuildPath(e), buildMountsFile) {
			return nil, ErrOutcomeUnknown
		}
		result = append(result, externalBuildContainer{id, e})
	}
	return result, nil
}
