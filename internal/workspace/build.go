package workspace

import (
	"context"
	"errors"
)

const (
	buildBuilding   = "building"
	buildFinalizing = "finalizing"
	buildPresent    = "present"
	buildFailed     = "failed"
	buildRecovery   = "recovery"
)

var ErrBuildFailed = errors.New("workspace image build or finalization failed; exact candidate cleanup is required before another create")

// Stored inside the existing exact-generation lifetime, not a second registry.
// Paths, image names and transient unit names are derived from Exact.
type buildIntent struct {
	Phase string `json:"phase"`
	Image string `json:"image,omitempty"`
}

func (b *buildIntent) valid() bool {
	if b.Image != "" && !immutableImage(b.Image) {
		return false
	}
	switch b.Phase {
	case buildBuilding, buildFailed, buildRecovery:
		return true
	case buildFinalizing, buildPresent:
		return immutableImage(b.Image)
	}
	return false
}

type CreateResult struct {
	Exact  Exact  `json:"-"`
	Status string `json:"status"`
}

// BuildRuntime is a synchronous adapter used by the service's asynchronous
// create worker. Success attests exact image ownership, terminal build processes
// and terminal log drains. Ordinary errors retain the existing backend permit.
// CleanupBuild must independently confirm absence of every exact build object.
type BuildRuntime interface {
	Build(context.Context, Exact, string, string) (string, error)
	CleanupBuild(context.Context, Exact, string, string) error
}

func (s *Service) present(e Exact) bool {
	b := s.lifetimes[e].Build
	return b == nil || b.Phase == buildPresent
}

func (s *Service) observeCreate(ctx context.Context, e Exact) (Exact, string, error) {
	b := s.lifetimes[e].Build
	if b == nil || b.Phase == buildPresent {
		return e, "PRESENT", nil
	}
	switch b.Phase {
	case buildBuilding, buildFinalizing:
		return e, "PROCESSING", nil
	case buildFailed:
		// This observation never admits a replacement, even if cleanup succeeds.
		diagnostic := s.buildDiagnostic(ctx, e)
		return e, "", &buildFailure{exact: e, diagnostic: diagnostic, cause: s.destroy(ctx, e)}
	default:
		return e, "", ErrOutcomeUnknown
	}
}

func (s *Service) buildCandidate(ctx context.Context, e Exact, dockerfile string, permit, control *BackendPermit) {
	defer s.workerDone.Done()
	image, err := s.runtime.(BuildRuntime).Build(ctx, e, dockerfile, s.dataDir)
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.lifetimes[e].Build
	// Ownership cannot be released while the worker or its backend is live.
	if err == nil && !immutableImage(image) {
		err = ErrOutcomeUnknown
	}
	_ = control.Resolve(e, completionOf(err))
	if err == nil && s.candidateAuthority(e, buildBuilding) {
		b.Phase, b.Image = buildFinalizing, image
		if saveErr := s.commit(clone(s.entries)); saveErr != nil {
			err = CompletedBackendError(saveErr) // no finalization submitted
		} else {
			// The original reservation covers this sequential finalization too.
			s.mu.Unlock()
			err = s.runtime.Create(ctx, e, image)
			s.mu.Lock()
		}
	}
	if err == nil && s.candidateAuthority(e, buildFinalizing) {
		objects, probeErr := discover(ctx, s.runtime, s.entries, s.image, s.lifetimes)
		if probeErr != nil {
			err = CompletedBackendError(probeErr)
		} else if _, ok := objects[e]; !ok {
			err = CompletedBackendError(ErrBuildFailed)
		}
	}
	_ = permit.Resolve(e, completionOf(err))
	delete(s.workers, e)
	if err == nil && s.candidateAuthority(e, buildFinalizing) {
		// Final authority recheck and durable publication share destroy's lock.
		b.Phase = buildPresent
	} else if ctx.Err() == nil && completionOf(err) == DefinitelyCompleted && s.candidateAuthority(e, b.Phase) {
		b.Phase = buildFailed
	} else {
		b.Phase = buildRecovery
	}
	if saveErr := s.commit(clone(s.entries)); saveErr != nil {
		return
	}
	if b.Phase == buildRecovery && !s.backend.has(e) && s.ready {
		// Revocation/TTL owns cleanup; background progress never touches activity.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), s.toolTimeout)
		defer cancel()
		if s.expired(e, s.now()) {
			sample, err := s.storage.Sample(cleanupCtx, s.dataDir)
			if s.storageBlocked {
				return
			}
			if err != nil || sample.AvailableBytes == 0 {
				_ = s.blockStorage(err)
				return
			}
		}
		_ = s.destroy(cleanupCtx, e)
	}
}

func (s *Service) candidateAuthority(e Exact, phase string) bool {
	b := s.lifetimes[e].Build
	return s.ready && !s.storageBlocked && s.entries[e.Workspace] == e &&
		b != nil && b.Phase == phase && phase != buildRecovery &&
		!s.expired(e, s.now())
}
