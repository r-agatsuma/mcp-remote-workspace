package workspace

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type journal struct {
	Version        int                `json:"version"`
	Image          string             `json:"image"`
	Entries        []Exact            `json:"entries"`
	Lifetimes      []Lifetime         `json:"lifetimes,omitempty"`
	StorageBlocked bool               `json:"storage_blocked,omitempty"`
	Backend        BackendState       `json:"backend"`
	Generations    []GenerationRecord `json:"generations,omitempty"`
}

func readJournal(path, image string) (map[WorkspaceKey]Exact, error) {
	entries, _, _, _, err := readAdmissionJournal(path, image)
	return entries, err
}

// Lifetime is durable host-clock activity; absolute age never resets on restart.
type Lifetime struct {
	Exact    Exact     `json:"exact"`
	Created  time.Time `json:"created"`
	Activity time.Time `json:"activity"`
}

func readAdmissionJournal(path, image string) (map[WorkspaceKey]Exact, map[Exact]Lifetime, bool, BackendState, error) {
	entries := make(map[WorkspaceKey]Exact)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return entries, make(map[Exact]Lifetime), false, BackendState{}, nil
	}
	if err != nil {
		return nil, nil, false, BackendState{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ownedByService(info) {
		return nil, nil, false, BackendState{}, errors.New("journal must be a private service-owned regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, false, BackendState{}, err
	}
	defer f.Close()
	var state journal
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, nil, false, BackendState{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, nil, false, BackendState{}, errors.New("trailing journal data")
	}
	if (state.Version != 1 && state.Version != 2 && state.Version != 3) || state.Image != image {
		return nil, nil, false, BackendState{}, errors.New("journal version or canonical image mismatch")
	}
	for _, e := range state.Entries {
		if err := e.validate(); err != nil {
			return nil, nil, false, BackendState{}, err
		}
		if _, exists := entries[e.Workspace]; exists {
			return nil, nil, false, BackendState{}, errors.New("duplicate workspace journal entry")
		}
		entries[e.Workspace] = e
	}
	lifetimes := make(map[Exact]Lifetime)
	for _, life := range state.Lifetimes {
		if life.Exact.validate() != nil || entries[life.Exact.Workspace] != life.Exact || life.Activity.IsZero() || life.Created.IsZero() || life.Activity.Before(life.Created) {
			return nil, nil, false, BackendState{}, errors.New("invalid exact-generation lifetime")
		}
		if _, duplicate := lifetimes[life.Exact]; duplicate {
			return nil, nil, false, BackendState{}, errors.New("duplicate lifetime")
		}
		lifetimes[life.Exact] = life
	}
	if len(lifetimes) != len(entries) {
		return nil, nil, false, BackendState{}, errors.New("generation chronology missing; operator recovery required")
	}
	if _, err := validateGenerationRecords(state.Generations, entries, state.Version); err != nil {
		return nil, nil, false, BackendState{}, err
	}
	return entries, lifetimes, state.StorageBlocked, state.Backend, nil
}

// writeJournal commits intent before runtime mutation. A failure, including an
// uncertain directory sync, poisons admission until a new startup reconciles.
func writeJournal(path, image string, entries map[WorkspaceKey]Exact) (err error) {
	return writeJournalState(path, image, entries, nil, false, BackendState{}, nil)
}

func writeAdmissionJournal(path, image string, entries map[WorkspaceKey]Exact, lifetimes map[Exact]Lifetime, blocked bool, backend BackendState) error {
	return writeJournalState(path, image, entries, lifetimes, blocked, backend, nil)
}

func writeJournalState(path, image string, entries map[WorkspaceKey]Exact, lifetimes map[Exact]Lifetime, blocked bool, backend BackendState, records map[Exact]GenerationRecord) (err error) {
	state := journal{Version: 1, Image: image, Entries: make([]Exact, 0, len(entries)), StorageBlocked: blocked, Backend: backend}
	if lifetimes != nil {
		state.Version = 2
		for _, life := range lifetimes {
			state.Lifetimes = append(state.Lifetimes, life)
		}
		sort.Slice(state.Lifetimes, func(i, j int) bool { return state.Lifetimes[i].Exact.Workspace < state.Lifetimes[j].Exact.Workspace })
	}
	if records != nil {
		state.Version = 3
		for _, record := range records {
			state.Generations = append(state.Generations, record)
		}
		sort.Slice(state.Generations, func(i, j int) bool {
			return state.Generations[i].Exact.Workspace < state.Generations[j].Exact.Workspace
		})
	}
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

func writeLifecycleJournal(path, image string, entries map[WorkspaceKey]Exact, lifetimes map[Exact]Lifetime, blocked bool, backend BackendState, records map[Exact]GenerationRecord) error {
	return writeJournalState(path, image, entries, lifetimes, blocked, backend, records)
}

func readGenerationRecords(path string, entries map[WorkspaceKey]Exact, image string) (map[Exact]GenerationRecord, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[Exact]GenerationRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	var state journal
	if err = json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	records, err := validateGenerationRecords(state.Generations, entries, state.Version)
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		if !r.Custom && r.Image != image {
			return nil, errors.New("generation canonical image mismatch")
		}
	}
	if state.Version < 3 {
		for _, e := range entries {
			records[e] = GenerationRecord{Exact: e, Phase: Present, Image: image}
		}
	}
	return records, nil
}
