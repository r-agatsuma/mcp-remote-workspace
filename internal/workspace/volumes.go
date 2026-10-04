package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func volumeName(id string) string { return "mcp-workspace-" + id }

type volumeInspection struct {
	Name    string
	Driver  string
	Labels  map[string]string
	Options map[string]string
}

func (v *volumeInspection) identity() (Workspace, error) {
	w, err := identityFromLabels(v.Labels)
	if err != nil {
		return Workspace{}, err
	}
	if v.Name != volumeName(w.ID) || v.Driver != "local" || len(v.Options) != 0 {
		return Workspace{}, errors.New("invalid workspace volume profile")
	}
	return w, nil
}

func (m *Manager) volume(ctx context.Context, w Workspace) error {
	data, err := m.runner.Run(ctx, "volume", "inspect", volumeName(w.ID))
	if err != nil {
		return err
	}
	var volumes []volumeInspection
	if err := json.Unmarshal(data, &volumes); err != nil {
		return err
	}
	if len(volumes) != 1 {
		return errors.New("expected one workspace volume")
	}
	actual, err := volumes[0].identity()
	if err != nil {
		return err
	}
	if actual.ID != w.ID || !actual.CreatedAt.Equal(w.CreatedAt) {
		return errors.New("workspace volume ownership disagrees")
	}
	return nil
}

func (m *Manager) createVolume(ctx context.Context, w Workspace) error {
	_, err := m.runner.Run(ctx, "volume", "create", "--driver=local",
		"--label="+managedLabel+"=true", "--label="+idLabel+"="+w.ID,
		"--label="+createdLabel+"="+w.CreatedAt.Format(time.RFC3339Nano),
		"--label="+profileLabel+"="+profileVersion, volumeName(w.ID))
	if err != nil {
		return err
	}
	return m.volume(ctx, w)
}

func (m *Manager) discoverVolumes(ctx context.Context) ([]volumeInspection, error) {
	data, err := m.runner.Run(ctx, "volume", "ls", "--format=json", "--filter=label="+managedLabel+"=true")
	if err != nil {
		return nil, err
	}
	var listed []struct{ Name string }
	if err := json.Unmarshal(data, &listed); err != nil {
		return nil, err
	}
	volumes := make([]volumeInspection, 0, len(listed))
	seen := map[string]bool{}
	for _, v := range listed {
		// Validate the name before allowing it to become a command argument.
		if len(v.Name) <= len("mcp-workspace-") || !workspaceIDPattern.MatchString(v.Name[len("mcp-workspace-"):]) || v.Name[:len("mcp-workspace-")] != "mcp-workspace-" || seen[v.Name] {
			return nil, errors.New("invalid or duplicate managed volume name")
		}
		seen[v.Name] = true
		data, err := m.runner.Run(ctx, "volume", "inspect", v.Name)
		if err != nil {
			return nil, err
		}
		var inspected []volumeInspection
		if err := json.Unmarshal(data, &inspected); err != nil {
			return nil, err
		}
		if len(inspected) != 1 || inspected[0].Name != v.Name {
			return nil, errors.New("unexpected volume inspection")
		}
		if _, err := inspected[0].identity(); err != nil {
			return nil, err
		}
		volumes = append(volumes, inspected[0])
	}
	return volumes, nil
}

func (m *Manager) removeVolume(ctx context.Context, e *entry) error {
	volumes, err := m.discoverVolumes(ctx)
	if err != nil {
		return err
	}
	for _, v := range volumes {
		if v.Name != volumeName(e.ID) {
			continue
		}
		w, err := v.identity()
		if err != nil || w.ID != e.ID || !w.CreatedAt.Equal(e.CreatedAt) {
			return errors.New("cleanup volume ownership disagrees")
		}
		if _, err := m.runner.Run(ctx, "volume", "rm", v.Name); err != nil {
			return fmt.Errorf("remove workspace data: %w", err)
		}
	}
	return nil
}
