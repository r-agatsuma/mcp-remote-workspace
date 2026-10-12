package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type commandRunner interface {
	run(context.Context, ...string) ([]byte, error)
}

type localCLI struct{ path string }

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8<<20 {
		return 0, errors.New("Podman output exceeds adapter bound")
	}
	return b.Buffer.Write(p)
}

func (c localCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.path, append([]string{"--remote=false"}, args...)...)
	// No caller-controlled connection settings, engine socket or shell. HOME
	// and runtime directory belong to the service account's launch environment.
	for _, key := range []string{"HOME", "PATH", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.WaitDelay = time.Second
	var stdout, stderr boundedOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// Runtime diagnostics may contain sensitive workload output; do not
		// expose them through the lifecycle API.
		return nil, fmt.Errorf("local Podman %s failed: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}

type Podman struct {
	runner      commandRunner
	image       string
	initialized bool
}

func NewPodman(path, image string) *Podman {
	return &Podman{runner: localCLI{path}, image: image}
}

func (p *Podman) Initialize(ctx context.Context) error {
	p.initialized = false
	if !validImage(p.image) {
		return errors.New("digest-pinned canonical image required")
	}
	if cli, ok := p.runner.(localCLI); ok {
		if os.Geteuid() == 0 || !filepath.IsAbs(cli.path) {
			return errors.New("host-side rootless Podman under a non-root service account required")
		}
	}
	data, err := p.runner.run(ctx, "info", "--format=json")
	if err != nil {
		return err
	}
	var info struct {
		Host struct {
			Security struct {
				Rootless bool `json:"rootless"`
			} `json:"security"`
		} `json:"host"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return err
	}
	if !info.Host.Security.Rootless {
		return errors.New("runtime did not attest rootless mode")
	}
	p.initialized = true
	return nil
}

func (p *Podman) inspect(ctx context.Context, id string) (Object, error) {
	if !validHex(id) {
		return Object{}, errors.New("invalid runtime object ID")
	}
	data, err := p.runner.run(ctx, "container", "inspect", "--", id)
	if err != nil {
		return Object{}, err
	}
	var inspected []struct {
		ID     string `json:"Id"`
		Name   string
		Config struct {
			Image  string
			Labels map[string]string
		}
	}
	if err := json.Unmarshal(data, &inspected); err != nil {
		return Object{}, err
	}
	if len(inspected) != 1 || inspected[0].ID != id {
		return Object{}, errors.New("unexpected exact container inspection")
	}
	o := inspected[0]
	return Object{ID: o.ID, Name: o.Name, Labels: o.Config.Labels, Image: o.Config.Image}, nil
}

func (p *Podman) List(ctx context.Context) ([]Object, error) {
	if !p.initialized {
		return nil, ErrNotReady
	}
	data, err := p.runner.run(ctx, "container", "list", "--all", "--no-trunc", "--format=json")
	if err != nil {
		return nil, err
	}
	var listed []struct {
		ID     string `json:"Id"`
		Names  []string
		Labels map[string]string
	}
	if err := json.Unmarshal(data, &listed); err != nil {
		return nil, err
	}
	if listed == nil {
		return nil, errors.New("runtime list must be a JSON array")
	}
	objects := make([]Object, 0, len(listed))
	for _, item := range listed {
		if !validHex(item.ID) || len(item.Names) == 0 {
			return nil, errors.New("incomplete runtime list entry")
		}
		for _, name := range item.Names {
			if name == "" {
				return nil, errors.New("empty runtime object name")
			}
		}
		claimed := false
		for _, name := range item.Names {
			claimed = claimed || strings.HasPrefix(name, "mcpws-")
		}
		for label := range item.Labels {
			claimed = claimed || strings.HasPrefix(label, labelPrefix)
		}
		if !claimed {
			continue
		}
		if len(item.Names) != 1 {
			return nil, errors.New("ambiguous service object names")
		}
		o, err := p.inspect(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		if o.Name != item.Names[0] {
			return nil, errors.New("service object changed during discovery")
		}
		if _, owned, err := classify(o); err != nil || !owned {
			return nil, errors.New("unrecognized service runtime object")
		}
		if o.Image != p.image {
			return nil, errors.New("runtime object has a different canonical image")
		}
		objects = append(objects, o)
	}
	return objects, nil
}

func (p *Podman) Create(ctx context.Context, e Exact, image string) error {
	if !p.initialized {
		return ErrNotReady
	}
	if err := e.validate(); err != nil {
		return err
	}
	if !validImage(image) || image != p.image {
		return errors.New("digest-pinned canonical image required")
	}
	args := []string{"container", "create", "--pull=never", "--name", objectName(e),
		"--network=none", "--image-volume=ignore", "--cap-drop=ALL", "--security-opt=no-new-privileges"}
	labels := objectLabels(e, image)
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--label", key+"="+labels[key])
	}
	args = append(args, "--", image)
	data, err := p.runner.run(ctx, args...)
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(data))
	o, err := p.inspect(ctx, id)
	if err != nil {
		return err
	}
	actual, owned, err := classify(o)
	if err != nil || !owned || actual != e || o.Image != image {
		return errors.New("created object did not match exact canonical generation")
	}
	_, err = p.runner.run(ctx, "container", "start", "--", id)
	return err
}

func (p *Podman) Destroy(ctx context.Context, e Exact) error {
	if err := e.validate(); err != nil {
		return err
	}
	objects, err := p.List(ctx)
	if err != nil {
		return err
	}
	for _, o := range objects {
		actual, _, err := classify(o)
		if err != nil {
			return err
		}
		if actual == e {
			// Full immutable ID was resolved from exact W/G ownership. Name
			// reuse cannot redirect this command to a later generation.
			_, err := p.runner.run(ctx, "container", "rm", "--force", "--", o.ID)
			return err
		}
	}
	return nil
}
