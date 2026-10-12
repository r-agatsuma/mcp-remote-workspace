package workspace

import (
	"context"
	"errors"
	"time"
)

var ErrAbsoluteTTL = errors.New("generation absolute TTL has expired")

func (s *Service) expired(e Exact, now time.Time) bool {
	life, ok := s.lifetimes[e]
	return ok && (!now.Before(life.Created.Add(s.policy.AbsoluteTTL)) || !now.Before(life.Activity.Add(s.policy.IdleTTL)))
}

func (s *Service) touch(e Exact) error {
	life, ok := s.lifetimes[e]
	if !ok {
		return ErrNotReady
	}
	now := s.now()
	if now.Before(life.Activity) {
		s.ready = false
		return ErrNotReady
	}
	if !now.Before(life.Created.Add(s.policy.AbsoluteTTL)) {
		return ErrAbsoluteTTL
	}
	life.Activity = now
	s.lifetimes[e] = life
	return s.commit(clone(s.entries))
}

// AdmitActivity is for admitted MCP requests only. Detached workload output,
// probes and backend completion/recovery must not call this method.
func (s *Service) AdmitActivity(identity Identity, e Exact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admitted(identity); err != nil {
		return err
	}
	if s.storageBlocked {
		return ErrStorageRecovery
	}
	if identity.key != e.Workspace || s.entries[e.Workspace] != e {
		return ErrStaleGeneration
	}
	return s.touch(e)
}

// ExpiredGenerations is only a hint. CleanupExpired rechecks activity and exact
// generation under lifecycle serialization immediately before cleanup.
func (s *Service) ExpiredGenerations() []Exact {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return nil
	}
	var result []Exact
	now := s.now()
	for _, e := range s.entries {
		if s.expired(e, now) {
			result = append(result, e)
		}
	}
	return result
}

func (s *Service) CleanupExpired(ctx context.Context, e Exact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return ErrNotReady
	}
	if err := e.validate(); err != nil {
		return err
	}
	if s.entries[e.Workspace] != e {
		return ErrStaleGeneration
	}
	if !s.expired(e, s.now()) {
		return ErrNotExpired
	}
	// Zero available blocks cannot safely perform journal mutations. Automatic
	// cleanup remains closed until an operator establishes a safe margin.
	if s.storageBlocked {
		return ErrStorageRecovery
	}
	sample, err := s.storage.Sample(ctx, s.dataDir)
	if err != nil || sample.AvailableBytes == 0 {
		return s.blockStorage(err)
	}
	// Sampling can take time; revalidate directly before destroy under this lock.
	if !s.expired(e, s.now()) {
		return ErrNotExpired
	}
	return s.destroy(ctx, e)
}

func (s *Service) blockStorage(cause error) error {
	s.storageBlocked = true
	if err := s.commit(clone(s.entries)); err != nil {
		return errors.Join(ErrStorageRecovery, cause, err)
	}
	return errors.Join(ErrStorageRecovery, cause)
}

func (s *Service) storageAdmission(ctx context.Context) error {
	if s.storageBlocked {
		return ErrStorageRecovery
	}
	sample, err := s.storage.Sample(ctx, s.dataDir)
	if err != nil {
		return s.blockStorage(err)
	}
	safe := func(v StorageSample) bool {
		return v.AvailableBytes >= s.policy.MinAvailableBytes && v.FreeInodes >= s.policy.MinFreeInodes
	}
	if safe(sample) {
		return nil
	}
	if sample.AvailableBytes == 0 {
		return s.blockStorage(nil)
	}
	// Pressure supplies no eligibility of its own. Only independently expired
	// exact generations may be removed; no arbitrary file deletion or reservation.
	for _, e := range s.entries {
		if s.expired(e, s.now()) {
			if err := s.destroy(ctx, e); err != nil {
				return s.blockStorage(err)
			}
			sample, err = s.storage.Sample(ctx, s.dataDir)
			if err != nil {
				return s.blockStorage(err)
			}
			if safe(sample) {
				return nil
			}
			if sample.AvailableBytes == 0 {
				return s.blockStorage(nil)
			}
		}
	}
	return s.blockStorage(nil)
}

// RecoverStorage is an operator-only API; it never deletes files. Once latched,
// safe samples alone do not reopen admission without this explicit action.
func (s *Service) RecoverStorage(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return ErrNotReady
	}
	sample, err := s.storage.Sample(ctx, s.dataDir)
	if err != nil || sample.AvailableBytes < s.policy.MinAvailableBytes || sample.FreeInodes < s.policy.MinFreeInodes {
		return errors.Join(ErrStorageRecovery, err)
	}
	s.storageBlocked = false
	return s.commit(clone(s.entries))
}

// Only explicit completion evidence frees a live permit. Partial side effects
// retain journal intent even when the backend itself has definitely finished.
func (s *Service) accountRuntime(permit *BackendPermit, err error) error {
	_ = permit.Resolve(permit.operation.Exact, completionOf(err))
	return s.commit(clone(s.entries))
}

// BeginBackend supplies the same global budget to later exec/build paths.
// The caller must resolve only on definite backend completion, independently
// of request cancellation. No activity is inferred from backend lifetime.
func (s *Service) BeginBackend(e Exact, kind string) (BackendOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return BackendOperation{}, ErrNotReady
	}
	if s.storageBlocked {
		return BackendOperation{}, ErrStorageRecovery
	}
	if err := e.validate(); err != nil {
		return BackendOperation{}, err
	}
	if s.entries[e.Workspace] != e {
		return BackendOperation{}, ErrStaleGeneration
	}
	if !s.now().Before(s.lifetimes[e].Created.Add(s.policy.AbsoluteTTL)) {
		return BackendOperation{}, ErrAbsoluteTTL
	}
	if s.backend.hasLifecycle(e) {
		return BackendOperation{}, ErrOutcomeUnknown
	}
	if kind == "injection" || kind == "create" || kind == "destroy" {
		return BackendOperation{}, errors.New("use the lifecycle or process injection gate")
	}
	permit, err := s.backend.Acquire(e, kind)
	if err != nil {
		return BackendOperation{}, err
	}
	if err := s.commit(clone(s.entries)); err != nil {
		_ = permit.Resolve(e, DefinitelyCompleted)
		return BackendOperation{}, err
	}
	return permit.operation, nil
}

func (s *Service) PendingBackend() []BackendOperation { return s.backend.Pending() }

// RecoverBackend is trusted host recovery. Unknown work is never released by
// a timeout, a caller, a stale generation or a discovery of momentary absence.
func (s *Service) RecoverBackend(op BackendOperation, completion Completion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return ErrNotReady
	}
	if err := s.backend.Recover(op, completion); err != nil {
		return err
	}
	if permit := s.injections[op.Exact]; permit != nil && permit.operation == op && completion == DefinitelyCompleted {
		delete(s.injections, op.Exact)
	}
	return s.commit(clone(s.entries))
}

// Inject runs a host runtime submission callback immediately after a fresh
// exact-generation PID/cgroup probe, under lifecycle serialization. The callback
// must use this snapshot's immutable target, must not reenter Service, and must
// return promptly after submission; its backend may finish asynchronously.
// OutcomeUnknown (including a callback panic) retains both permits. Complete
// asynchronous work with RecoverBackend using the returned operation identity.
func (s *Service) Inject(ctx context.Context, identity Identity, e Exact, submit func(context.Context, ProcessSnapshot) BackendResult) (BackendOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admitted(identity); err != nil {
		return BackendOperation{}, err
	}
	if err := e.validate(); err != nil {
		return BackendOperation{}, err
	}
	if identity.key != e.Workspace || s.entries[e.Workspace] != e {
		return BackendOperation{}, ErrStaleGeneration
	}
	if s.storageBlocked {
		return BackendOperation{}, ErrStorageRecovery
	}
	if s.injections[e] != nil {
		return BackendOperation{}, ErrInjectionBusy
	}
	if s.backend.hasLifecycle(e) {
		return BackendOperation{}, ErrOutcomeUnknown
	}
	if err := ctx.Err(); err != nil {
		return BackendOperation{}, err
	}
	probe, ok := s.runtime.(ProcessProbe)
	if !ok || submit == nil {
		return BackendOperation{}, ErrProcessUnsafe
	}
	permit, err := s.backend.Acquire(e, "injection")
	if err != nil {
		return BackendOperation{}, err
	}
	// Persist admitted MCP activity before the probe, leaving no journal I/O
	// between exact process validation and the runtime injection callback.
	if err := s.touch(e); err != nil {
		_ = permit.Resolve(e, DefinitelyCompleted)
		return BackendOperation{}, err
	}
	s.injections[e] = permit
	snapshot, err := probe.ProbeProcess(ctx, e)
	if err != nil && completionOf(err) == OutcomeUnknown {
		return permit.operation, errors.Join(ErrProcessUnsafe, ErrOutcomeUnknown, err)
	}
	if err == nil {
		err = s.policy.validateProcess(e, snapshot)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && !s.now().Before(s.lifetimes[e].Created.Add(s.policy.AbsoluteTTL)) {
		err = ErrAbsoluteTTL
	}
	if err != nil {
		_ = permit.Resolve(e, DefinitelyCompleted)
		delete(s.injections, e)
		saveErr := s.commit(clone(s.entries))
		return BackendOperation{}, errors.Join(ErrProcessUnsafe, err, saveErr)
	}
	result := submit(ctx, snapshot)
	if result.Completion == DefinitelyCompleted {
		_ = permit.Resolve(e, DefinitelyCompleted)
		delete(s.injections, e)
		return permit.operation, errors.Join(result.Err, s.commit(clone(s.entries)))
	}
	return permit.operation, errors.Join(ErrOutcomeUnknown, result.Err)
}
