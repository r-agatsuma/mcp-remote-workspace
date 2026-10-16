package workspace

import (
	"context"
	"errors"
	"os/exec"
	"sort"
	"sync"
)

type Completion uint8

const (
	OutcomeUnknown Completion = iota
	DefinitelyCompleted
)

type BackendResult struct {
	Completion Completion
	Err        error
}

// BackendOperation is bounded accounting metadata, never workload output.
type BackendOperation struct {
	ID     uint64
	Exact  Exact
	Kind   string
	BootID string // trusted kernel epoch at admission; empty only for historical records
}

// BackendLedger accounts for live work independently of caller lifetime.
// Only trusted completion/recovery evidence may resolve a permit. Keeping a
// permit or diagnostics after completion does not keep the live slot occupied.
type BackendLedger struct {
	mu                     sync.Mutex
	limit, diagnosticLimit int
	next                   uint64
	bootID                 string
	live                   map[uint64]BackendOperation
	diagnostics            []BackendOperation
}

type BackendPermit struct {
	ledger    *BackendLedger
	operation BackendOperation
}

func NewBackendLedger(limit, diagnostics int) (*BackendLedger, error) {
	if limit <= 0 || diagnostics < 0 {
		return nil, errors.New("invalid backend accounting limits")
	}
	bootID, err := kernelBootID()
	if err != nil {
		return nil, err
	}
	return &BackendLedger{limit: limit, diagnosticLimit: diagnostics, bootID: bootID, live: make(map[uint64]BackendOperation)}, nil
}

func (b *BackendLedger) Acquire(e Exact, kind string) (*BackendPermit, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	if kind == "" || len(kind) > 64 {
		return nil, errors.New("bounded backend operation kind required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.live) >= b.limit || b.next == ^uint64(0) {
		return nil, ErrBackendLimit
	}
	b.next++
	op := BackendOperation{ID: b.next, Exact: e, Kind: kind, BootID: b.bootID}
	b.live[op.ID] = op
	return &BackendPermit{b, op}, nil
}

func (p *BackendPermit) Operation() BackendOperation { return p.operation }

// Resolve does not check the current generation: old exact work remains
// recoverable, but can never release accounting for another exact generation.
func (p *BackendPermit) Resolve(e Exact, completion Completion) error {
	if p == nil || p.ledger == nil || p.operation.Exact != e {
		return ErrStaleGeneration
	}
	if completion != OutcomeUnknown && completion != DefinitelyCompleted {
		return errors.New("invalid backend completion")
	}
	b := p.ledger
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.live[p.operation.ID]; !exists {
		return nil
	}
	if completion == OutcomeUnknown {
		return ErrOutcomeUnknown
	}
	delete(b.live, p.operation.ID)
	if b.diagnosticLimit > 0 {
		if len(b.diagnostics) == b.diagnosticLimit {
			b.diagnostics = b.diagnostics[1:]
		}
		b.diagnostics = append(b.diagnostics, p.operation)
	}
	return nil
}

func (b *BackendLedger) Pending() []BackendOperation {
	b.mu.Lock()
	defer b.mu.Unlock()
	ops := make([]BackendOperation, 0, len(b.live))
	for _, op := range b.live {
		ops = append(ops, op)
	}
	return ops
}

func (b *BackendLedger) Diagnostics() []BackendOperation {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]BackendOperation(nil), b.diagnostics...)
}

// Recover requires exact operation identity and trusted proof of completion.
func (b *BackendLedger) Recover(op BackendOperation, completion Completion) error {
	b.mu.Lock()
	actual, exists := b.live[op.ID]
	b.mu.Unlock()
	if !exists || actual != op {
		return ErrStaleGeneration
	}
	return (&BackendPermit{b, op}).Resolve(op.Exact, completion)
}

func (b *BackendLedger) has(e Exact) bool {
	for _, op := range b.Pending() {
		if op.Exact == e {
			return true
		}
	}
	return false
}

// BackendState is persisted with lifecycle intent before backend submission.
// Restored operations are outcome-uncertain until trusted recovery resolves them.
type BackendState struct {
	Next       uint64             `json:"next"`
	Operations []BackendOperation `json:"operations,omitempty"`
}

func (b *BackendLedger) state() BackendState {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := BackendState{Next: b.next}
	for _, op := range b.live {
		state.Operations = append(state.Operations, op)
	}
	sort.Slice(state.Operations, func(i, j int) bool { return state.Operations[i].ID < state.Operations[j].ID })
	return state
}

func (b *BackendLedger) restore(state BackendState, entries map[WorkspaceKey]Exact) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(entries) > 0 && state.Next == 0 {
		return errors.New("durable backend operation counter missing")
	}
	if len(state.Operations) > b.limit {
		return ErrBackendLimit
	}
	for _, op := range state.Operations {
		if op.ID == 0 || op.ID > state.Next || op.Exact.validate() != nil || entries[op.Exact.Workspace] != op.Exact || op.Kind == "" || len(op.Kind) > 64 || op.BootID != "" && !validBootID(op.BootID) {
			return errors.New("invalid durable backend accounting")
		}
		if _, exists := b.live[op.ID]; exists {
			return errors.New("duplicate durable backend operation")
		}
		b.live[op.ID] = op
	}
	b.next = state.Next
	return nil
}

// CompletedBackendError is trusted runtime evidence of a terminal failure.
// An ordinary error provides no such evidence and retains the live permit.
func CompletedBackendError(err error) error {
	if err == nil {
		return nil
	}
	return completedBackendError{err}
}

type completedBackendError struct{ error }

func (e completedBackendError) Unwrap() error { return e.error }

func completionOf(err error) Completion {
	if err == nil {
		return DefinitelyCompleted
	}
	var terminal completedBackendError
	if errors.As(err, &terminal) && !errors.Is(err, ErrOutcomeUnknown) {
		return DefinitelyCompleted
	}
	return OutcomeUnknown
}

// Local CLI adapters attest terminal failure only after the synchronous command
// has returned without caller cancellation or explicit uncertain completion.
func localRuntimeError(ctx context.Context, err error) error {
	if errors.Is(err, exec.ErrWaitDelay) {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrOutcomeUnknown) {
		return err
	}
	return CompletedBackendError(err)
}

func (b *BackendLedger) hasLifecycle(e Exact) bool {
	for _, op := range b.Pending() {
		if op.Exact == e && (op.Kind == "create" || op.Kind == "destroy") {
			return true
		}
	}
	return false
}
