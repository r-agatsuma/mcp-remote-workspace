package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/r-agatsuma/mcp-remote-workspace/internal/control"
)

const (
	Image          = "localhost/mcp-remote-workspace:v0"
	labelPrefix    = "io.github.r-agatsuma.mcp-remote-workspace."
	managedLabel   = labelPrefix + "managed"
	idLabel        = labelPrefix + "workspace-id"
	createdLabel   = labelPrefix + "created-at"
	profileLabel   = labelPrefix + "profile"
	profileVersion = "v0"
)

var (
	ErrNotFound        = errors.New("workspace not found")
	workspaceIDPattern = regexp.MustCompile(`^ws_[0-9a-f]{64}$`)
	containerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Workspace struct {
	ID        string
	CreatedAt time.Time
}

type entry struct {
	Workspace
	container string     // Only set after inspecting matching ownership labels.
	op        sync.Mutex // Covers operations, destruction and all failure cleanup.
	blocked   bool
	deleted   bool
}

type Manager struct {
	mu           sync.Mutex
	runner       Runner
	imageID      string
	entries      map[string]*entry
	destroyed    map[string]bool
	maxTextBytes int64
}

// New is also the restart boundary: invalid labels, duplicate IDs, or drifted
// profiles fail startup without mutating any existing containers.
func New(ctx context.Context, runner Runner) (*Manager, error) {
	m := &Manager{runner: runner, entries: make(map[string]*entry), destroyed: make(map[string]bool), maxTextBytes: control.DefaultTextLimit}
	data, err := runner.Run(ctx, "info", "--format=json")
	if err != nil {
		return nil, err
	}
	var info struct {
		Host struct {
			Security          struct{ Rootless bool }
			CgroupVersion     string
			CgroupControllers []string
		}
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("podman info: %w", err)
	}
	if !info.Host.Security.Rootless || info.Host.CgroupVersion != "v2" {
		return nil, errors.New("local rootless Podman with cgroup v2 is required")
	}
	for _, needed := range []string{"cpu", "memory", "pids"} {
		if !contains(info.Host.CgroupControllers, needed) {
			return nil, fmt.Errorf("required cgroup controller %s is unavailable", needed)
		}
	}
	data, err = runner.Run(ctx, "image", "inspect", Image)
	if err != nil {
		return nil, fmt.Errorf("fixed development image must be built locally: %w", err)
	}
	var images []struct {
		ID     string
		Config struct {
			Labels  map[string]string
			Volumes map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &images); err != nil {
		return nil, fmt.Errorf("image inspection: %w", err)
	}
	if len(images) != 1 || !containerIDPattern.MatchString(imageID(images[0].ID)) || images[0].Config.Labels[labelPrefix+"image"] != "v0" || len(images[0].Config.Volumes) != 0 {
		return nil, errors.New("invalid fixed development image")
	}
	m.imageID = imageID(images[0].ID)
	ids, err := m.discover(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		c, err := m.inspect(ctx, id)
		if err != nil {
			return nil, err
		}
		w, err := c.identity()
		if err != nil {
			return nil, err
		}
		if m.entries[w.ID] != nil {
			return nil, fmt.Errorf("duplicate workspace ID %s", w.ID)
		}
		if err := c.verify(m.imageID); err != nil {
			return nil, fmt.Errorf("recovered workspace %s: %w", w.ID, err)
		}
		m.entries[w.ID] = &entry{Workspace: w, container: id}
	}
	// Validate the complete registry before performing any recovery mutation.
	// Orphan volumes from interrupted creation remain destroyable and blocked.
	volumes, err := m.discoverVolumes(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range volumes {
		w, _ := v.identity()
		if e := m.entries[w.ID]; e != nil {
			if !e.CreatedAt.Equal(w.CreatedAt) {
				return nil, errors.New("recovered volume ownership disagrees")
			}
		} else {
			m.entries[w.ID] = &entry{Workspace: w, blocked: true}
		}
	}
	for _, e := range m.entries {
		if e.container != "" {
			if err := m.volume(ctx, e.Workspace); err != nil {
				return nil, err
			}
		}
	}
	for _, e := range m.entries {
		if e.container == "" {
			continue
		}
		if state, ok := runner.(BlockState); ok {
			blocked, err := state.Blocked(e.ID)
			if err != nil {
				return nil, err
			}
			if blocked {
				e.blocked = true
				continue
			}
		}
		// Never trust processes left by an earlier daemon instance. A failed
		// reset blocks only this workspace so explicit destroy stays available.
		if err := m.reset(e); err != nil {
			if err := m.block(e); err != nil {
				return nil, err
			}
		}
	}
	return m, nil
}

func (m *Manager) Create(ctx context.Context) (Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Workspace{}, err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Workspace{}, err
	}
	w := Workspace{ID: "ws_" + hex.EncodeToString(random[:]), CreatedAt: time.Now().UTC()}
	if m.entries[w.ID] != nil || m.destroyed[w.ID] {
		return Workspace{}, errors.New("workspace ID collision")
	}
	e := &entry{Workspace: w, blocked: true}
	// Preserve identity even when create fails without returning a usable ID.
	m.entries[w.ID] = e
	err := m.createVolume(ctx, w)
	var data []byte
	if err == nil {
		data, err = m.runner.Run(ctx, createArgs(w, m.imageID)...)
	}
	if err == nil {
		id := strings.TrimSpace(string(data))
		if !containerIDPattern.MatchString(id) {
			err = errors.New("podman create returned an invalid container ID")
		} else {
			var c *containerInspection
			c, err = m.inspect(ctx, id)
			if err == nil {
				var actual Workspace
				actual, err = c.identity()
				if err == nil && (actual.ID != w.ID || !actual.CreatedAt.Equal(w.CreatedAt)) {
					err = errors.New("created container ownership labels disagree")
				}
				if err == nil {
					e.container = id
					err = c.verify(m.imageID)
				}
			}
		}
	}
	if err == nil {
		_, err = m.runner.Run(ctx, "start", e.container)
	}
	if err == nil {
		var c *containerInspection
		c, err = m.inspect(ctx, e.container)
		if err == nil {
			err = c.verify(m.imageID)
		}
		if err == nil && (c.State == nil || !c.State.Running) {
			err = errors.New("workspace container did not remain running")
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		e.blocked = false
		return w, nil
	}
	// Request cancellation must never cancel rollback. Do not retry failed
	// commands; retain the registry and labels if this cleanup attempt fails.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if cleanupErr := m.remove(cleanupCtx, e); cleanupErr != nil {
		return Workspace{}, fmt.Errorf("create failed: %w; cleanup failed: %v; retry workspace_destroy with workspace_id=%s", err, cleanupErr, w.ID)
	}
	delete(m.entries, w.ID)
	return Workspace{}, fmt.Errorf("create failed: %w", err)
}

func (m *Manager) Destroy(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if e == nil {
		if m.destroyed[id] {
			return nil
		}
		return ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	if err := m.remove(ctx, e); err != nil {
		if blockErr := m.block(e); blockErr != nil {
			return fmt.Errorf("destroy failed: %w; block marker failed: %v", err, blockErr)
		}
		return err
	}
	if state, ok := m.runner.(BlockState); ok {
		if err := state.UnblockDestroyed(id); err != nil {
			e.blocked = true
			return err
		}
	}
	e.deleted = true
	delete(m.entries, id)
	m.destroyed[id] = true
	return nil
}

func (m *Manager) remove(ctx context.Context, e *entry) error {
	ids := []string{e.container}
	if e.container == "" {
		var err error
		ids, err = m.discover(ctx, e.ID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			c, err := m.inspect(ctx, id)
			if err != nil {
				return err
			}
			w, err := c.identity()
			if err != nil {
				return err
			}
			if w.ID != e.ID || !w.CreatedAt.Equal(e.CreatedAt) {
				return errors.New("cleanup ownership labels disagree")
			}
		}
	}
	for _, id := range ids {
		// Force removal stops compute and removes its writable layer and any
		// anonymous runtime volumes. Ignore handles a prior uncertain removal.
		if _, err := m.runner.Run(ctx, "rm", "--force", "--volumes", "--ignore", "--time=0", id); err != nil {
			return err
		}
	}
	return m.removeVolume(ctx, e)
}

func (m *Manager) discover(ctx context.Context, workspaceID string) ([]string, error) {
	args := []string{"ps", "--all", "--no-trunc", "--format=json", "--filter=label=" + managedLabel + "=true"}
	if workspaceID != "" {
		args = append(args, "--filter=label="+idLabel+"="+workspaceID)
	}
	data, err := m.runner.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var containers []struct{ ID string }
	if err := json.Unmarshal(data, &containers); err != nil {
		return nil, fmt.Errorf("container discovery: %w", err)
	}
	ids := make([]string, 0, len(containers))
	seen := make(map[string]bool)
	for _, c := range containers {
		if !containerIDPattern.MatchString(c.ID) || seen[c.ID] {
			return nil, errors.New("container discovery returned an invalid or duplicate container ID")
		}
		seen[c.ID] = true
		ids = append(ids, c.ID)
	}
	return ids, nil
}

func (m *Manager) inspect(ctx context.Context, id string) (*containerInspection, error) {
	data, err := m.runner.Run(ctx, "container", "inspect", id)
	if err != nil {
		return nil, err
	}
	var containers []containerInspection
	if err := json.Unmarshal(data, &containers); err != nil {
		return nil, fmt.Errorf("container inspection: %w", err)
	}
	if len(containers) != 1 || containers[0].ID != id {
		return nil, errors.New("container inspection returned an unexpected container ID")
	}
	return &containers[0], nil
}

func imageID(value string) string { return strings.TrimPrefix(value, "sha256:") }
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
