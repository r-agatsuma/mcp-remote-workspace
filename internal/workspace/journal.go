package workspace

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
)

type journal struct {
	Version int     `json:"version"`
	Image   string  `json:"image"`
	Entries []Exact `json:"entries"`
}

func readJournal(path, image string) (map[WorkspaceKey]Exact, error) {
	entries := make(map[WorkspaceKey]Exact)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return entries, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ownedByService(info) {
		return nil, errors.New("journal must be a private service-owned regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var state journal
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing journal data")
	}
	if state.Version != 1 || state.Image != image {
		return nil, errors.New("journal version or canonical image mismatch")
	}
	for _, e := range state.Entries {
		if err := e.validate(); err != nil {
			return nil, err
		}
		if _, exists := entries[e.Workspace]; exists {
			return nil, errors.New("duplicate workspace journal entry")
		}
		entries[e.Workspace] = e
	}
	return entries, nil
}

// writeJournal commits intent before runtime mutation. A failure, including an
// uncertain directory sync, poisons admission until a new startup reconciles.
func writeJournal(path, image string, entries map[WorkspaceKey]Exact) (err error) {
	state := journal{Version: 1, Image: image, Entries: make([]Exact, 0, len(entries))}
	for _, e := range entries {
		state.Entries = append(state.Entries, e)
	}
	sort.Slice(state.Entries, func(i, j int) bool { return state.Entries[i].Workspace < state.Entries[j].Workspace })
	f, err := os.CreateTemp(filepath.Dir(path), ".journal-*")
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, f.Close())
		os.Remove(f.Name())
	}()
	if err = json.NewEncoder(f).Encode(state); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	return dir.Sync()
}
