package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

type RecoveryOperation struct {
	Operation BackendOperation `json:"operation"`
	Selection string           `json:"selection"`
	Evidence  string           `json:"evidence"`
}

type RecoveryGeneration struct {
	Exact     Exact  `json:"exact"`
	Selection string `json:"selection"`
}

type RecoveryState struct {
	StorageBlocked bool                 `json:"storage_blocked"`
	Operations     []RecoveryOperation  `json:"operations"`
	Generations    []RecoveryGeneration `json:"generations"`
}

// Selections bind displayed product-owned state; they are not terminal evidence
// or public credentials. Domain separation prevents cross-action selection.
func recoverySelection(kind string, value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(append([]byte("mcp-workspaced/recovery/"+kind+"\x00"), data...))
	return hex.EncodeToString(sum[:])
}

func bootEvidence(op BackendOperation, current string) string {
	if !validBootID(op.BootID) {
		return "historical_boot_unknown"
	}
	if op.BootID == current {
		return "current_boot_unknown"
	}
	return "prior_boot_terminal"
}

// RecoverySnapshot enumerates only bounded journal-owned state, never arbitrary
// runtime inventory, raw OpenAI identity, paths or workload output.
func (s *Service) RecoverySnapshot(ctx context.Context) (RecoveryState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return RecoveryState{}, ErrNotReady
	}
	boot, err := s.bootID()
	if err != nil || !validBootID(boot) {
		return RecoveryState{}, ErrNotReady
	}
	if err := ctx.Err(); err != nil {
		return RecoveryState{}, err
	}
	state := RecoveryState{StorageBlocked: s.storageBlocked,
		Operations: []RecoveryOperation{}, Generations: []RecoveryGeneration{}}
	for _, op := range s.backend.state().Operations {
		state.Operations = append(state.Operations, RecoveryOperation{op, recoverySelection("backend", op), bootEvidence(op, boot)})
	}
	for _, e := range s.entries {
		state.Generations = append(state.Generations, RecoveryGeneration{e, recoverySelection("generation", e)})
	}
	sort.Slice(state.Generations, func(i, j int) bool {
		return state.Generations[i].Exact.Workspace < state.Generations[j].Exact.Workspace
	})
	return state, nil
}

// RecoverBackendAfterReboot gathers evidence itself. The existing live runtime
// completion path remains separate. Reboot proves prior execution terminal, not
// successful, failed, or residue-free. It never changes generation/build intent.
func (s *Service) RecoverBackendAfterReboot(ctx context.Context, op BackendOperation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recoverBackendAfterReboot(ctx, op)
}

func (s *Service) recoverBackendAfterReboot(ctx context.Context, op BackendOperation) error {
	if !s.ready {
		return ErrNotReady
	}
	found := false
	for _, actual := range s.backend.Pending() {
		found = found || actual == op
	}
	if !found {
		return ErrStaleGeneration
	}
	boot, err := s.bootID()
	if err != nil || !validBootID(boot) {
		return ErrNotReady
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if bootEvidence(op, boot) != "prior_boot_terminal" {
		return ErrOutcomeUnknown
	}
	// Exact ledger equality, injection bookkeeping and durable commit happen
	// under lifecycle serialization. commit failure closes all normal admission.
	return s.recoverBackend(op, DefinitelyCompleted)
}

func (s *Service) RecoverPendingBackend(ctx context.Context, selection string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return ErrNotReady
	}
	for _, op := range s.backend.Pending() {
		if recoverySelection("backend", op) == selection {
			return s.recoverBackendAfterReboot(ctx, op)
		}
	}
	return ErrStaleGeneration
}

// CleanupRecoveryGeneration is a separate explicit action after permit/storage
// recovery. It uses normal exact destruction and cannot admit a replacement.
func (s *Service) CleanupRecoveryGeneration(ctx context.Context, selection string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return ErrNotReady
	}
	if s.storageBlocked {
		return ErrStorageRecovery
	}
	for _, e := range s.entries {
		if recoverySelection("generation", e) == selection {
			return s.destroy(ctx, e)
		}
	}
	return ErrStaleGeneration
}
