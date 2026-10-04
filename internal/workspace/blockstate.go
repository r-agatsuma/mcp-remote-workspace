package workspace

import (
	"errors"
	"os"
	"path/filepath"
)

// BlockState is service-owned and inaccessible to container code. Markers
// survive daemon restarts; only successful explicit destruction clears them.
type BlockState interface {
	Blocked(string) (bool, error)
	Block(string) error
	UnblockDestroyed(string) error
}

func (p *podman) Blocked(id string) (bool, error) {
	if !workspaceIDPattern.MatchString(id) {
		return false, errors.New("invalid block marker ID")
	}
	info, err := os.Lstat(filepath.Join(p.stateDir, id))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return false, errors.New("invalid workspace block marker")
	}
	return true, nil
}

func (p *podman) Block(id string) error {
	if !workspaceIDPattern.MatchString(id) {
		return errors.New("invalid block marker ID")
	}
	file, err := os.OpenFile(filepath.Join(p.stateDir, id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		_, err = p.Blocked(id)
		return err
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return err
	}
	return syncDirectory(p.stateDir)
}

func (p *podman) UnblockDestroyed(id string) error {
	if !workspaceIDPattern.MatchString(id) {
		return errors.New("invalid block marker ID")
	}
	if err := os.Remove(filepath.Join(p.stateDir, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(p.stateDir)
}

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func (m *Manager) block(e *entry) error {
	e.blocked = true
	if state, ok := m.runner.(BlockState); ok {
		return state.Block(e.ID)
	}
	return nil // In-memory runners are used only by tests.
}
