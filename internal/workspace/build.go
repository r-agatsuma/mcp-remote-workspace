package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// GenerationRecord extends the lifecycle journal, not a resumable job database.
// A non-PRESENT record retains resource/admission ownership until exact cleanup.
type GenerationRecord struct {
	Exact  Exact  `json:"exact"`
	Phase  State  `json:"phase"`
	Image  string `json:"image"`
	Custom bool   `json:"custom,omitempty"`
}

func validateGenerationRecords(values []GenerationRecord, entries map[WorkspaceKey]Exact, version int) (map[Exact]GenerationRecord, error) {
	result := map[Exact]GenerationRecord{}
	for _, r := range values {
		if version != 3 || entries[r.Exact.Workspace] != r.Exact || r.Exact.validate() != nil || !validRuntimeImage(r.Exact, r.Image) {
			return nil, errors.New("invalid generation record")
		}
		if r.Custom {
			if r.Image != buildImageName(r.Exact) {
				return nil, errors.New("invalid candidate image")
			}
		} else if r.Phase != Present || !validImage(r.Image) {
			return nil, errors.New("invalid canonical generation")
		}
		switch r.Phase {
		case Building, Finalizing, Present, Failed, Deleting:
		default:
			return nil, errors.New("invalid candidate phase")
		}
		if _, duplicate := result[r.Exact]; duplicate {
			return nil, errors.New("duplicate generation record")
		}
		result[r.Exact] = r
	}
	if version == 3 && len(result) != len(entries) {
		return nil, errors.New("generation state missing")
	}
	return result, nil
}
func publicPhase(phase State) State {
	switch phase {
	case Building, Finalizing:
		return Processing
	case Deleting:
		return Failed
	default:
		return phase
	}
}

type CreateResult struct {
	Status State `json:"status"`
	Exact  Exact `json:"-"`
}

// BuildRuntime is a trusted host-side capability. Build returns only after its
// worker and stdout/stderr drain loops have exited; no writer may be used after
// return. Definite completion additionally attests no surviving engine work.
// The service supplies pre-opened bounded Data log writers and finalizes them
// before releasing worker ownership. Unknown results retain admission until
// trusted recovery.
// KnownBuildImages includes all service-owned current and historical aliases.
// BuildGenerations is a synchronous, read-only ownership inventory used to reject
// orphan image intent on startup. Neither probe leaves backend work on return.
// CleanupBuild must remove and independently confirm ONLY exact-G image,
// intermediate container/image, bounded build cgroup and temporary backend residue. It may fail closed.
// Caller Dockerfile never supplies any of these host paths or runtime options.
type BuildRuntime interface {
	KnownBuildImages(context.Context) ([]string, error)
	BuildGenerations(context.Context) ([]Exact, error)
	Build(context.Context, Exact, BuildPlan, string, io.Writer, io.Writer) BackendResult
	CleanupBuild(context.Context, Exact, string) error
}
type activeBuild struct {
	cancel  context.CancelFunc
	permit  *BackendPermit
	running bool
}

func (s *Service) buildPath(e Exact) string { return filepath.Join(s.dataDir, "builds", objectName(e)) }

// CreateDockerfile observes an existing candidate before examining new contents.
// Accepted work is owned by the service, independently of the polling request.
func (s *Service) CreateDockerfile(ctx context.Context, identity Identity, dockerfile string) (CreateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admitted(identity); err != nil {
		return CreateResult{}, err
	}
	if s.storageBlocked {
		return CreateResult{}, ErrStorageRecovery
	}
	if e, exists := s.entries[identity.key]; exists {
		if err := s.touch(e); err != nil {
			return CreateResult{}, err
		}
		return CreateResult{publicPhase(s.records[e].Phase), e}, nil
	}
	if err := ctx.Err(); err != nil {
		return CreateResult{}, err
	}
	runtime, ok := s.runtime.(BuildRuntime)
	if !ok {
		return CreateResult{}, ErrNotReady
	}
	// Parse before any runtime call. No external consumption happens in discovery.
	if _, err := compileDockerfile(dockerfile, nil); err != nil {
		return CreateResult{}, err
	}
	known, err := runtime.KnownBuildImages(ctx)
	if err != nil {
		return CreateResult{}, err
	}
	plan, err := compileDockerfile(dockerfile, known)
	if err != nil {
		return CreateResult{}, err
	}
	if _, err = s.discover(ctx); err != nil {
		return CreateResult{}, err
	}
	if err = s.storageAdmission(ctx); err != nil {
		return CreateResult{}, err
	}
	if len(s.entries) >= s.policy.MaxGenerations {
		return CreateResult{}, ErrGenerationLimit
	}
	g, err := freshGeneration()
	if err != nil {
		return CreateResult{}, err
	}
	e := Exact{identity.key, g}
	permit, err := s.backend.Acquire(e, "build")
	if err != nil {
		return CreateResult{}, err
	}
	next := clone(s.entries)
	next[e.Workspace] = e
	now := s.now()
	s.lifetimes[e] = Lifetime{e, now, now}
	s.records[e] = GenerationRecord{e, Building, buildImageName(e), true}
	if err = s.commit(next); err != nil {
		_ = permit.Resolve(e, DefinitelyCompleted)
		return CreateResult{}, err
	}
	// Persist intent before allocating Data state, so crashes leave fenced intent.
	buildCtx, cancel := context.WithCancel(context.Background())
	logs, err := s.prepareBuild(e, plan, cancel)
	if err != nil {
		cancel()
		_ = permit.Resolve(e, DefinitelyCompleted)
		r := s.records[e]
		r.Phase = Failed
		s.records[e] = r
		return CreateResult{Failed, e}, errors.Join(err, s.commit(clone(s.entries)))
	}
	s.builds[e] = &activeBuild{cancel: cancel, permit: permit, running: true}
	s.buildWG.Add(1)
	go s.completeBuild(buildCtx, runtime, e, plan, permit, logs)
	return CreateResult{Processing, e}, nil
}
func (s *Service) prepareBuild(e Exact, plan BuildPlan, cancel context.CancelFunc) (*buildLogs, error) {
	if err := privateDir(s.dataDir); err != nil {
		return nil, err
	}
	if err := privateDir(filepath.Join(s.dataDir, "builds")); err != nil {
		return nil, err
	}
	path := s.buildPath(e)
	if err := os.Mkdir(path, 0700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(path, "context"), 0700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(path, "tmp"), 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(path, "context", "Dockerfile"), []byte(plan.dockerfile), 0600); err != nil {
		return nil, err
	}
	return s.prepareLogs(path, cancel)
}
func (s *Service) completeBuild(ctx context.Context, runtime BuildRuntime, e Exact, plan BuildPlan, permit *BackendPermit, logs *buildLogs) {
	defer s.buildWG.Done()
	result := func() (result BackendResult) {
		defer func() {
			if recover() != nil {
				result = BackendResult{Err: ErrOutcomeUnknown}
			}
		}()
		return runtime.Build(ctx, e, plan, s.buildPath(e), logs.stdout, logs.stderr)
	}()
	// Build joins the subprocess and its drain loops. Sync/close the exact Data
	// descriptors before publication, permit release, cleanup or singleton exit.
	result.Err = errors.Join(result.Err, logs.finish())
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.builds[e]; b != nil {
		b.running = false
	}
	defer func() {
		if b := s.builds[e]; b != nil {
			b.cancel()
			delete(s.builds, e)
		}
	}()
	if result.Completion == DefinitelyCompleted && !errors.Is(result.Err, ErrOutcomeUnknown) {
		_ = permit.Resolve(e, DefinitelyCompleted)
	}
	// No Data or journal writes after singleton ownership has been relinquished.
	if !s.ready || s.entries[e.Workspace] != e {
		return
	}
	r := s.records[e]
	if r.Phase == Deleting {
		if err := s.commit(clone(s.entries)); err != nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), s.toolTimeout)
		defer cancel()
		_ = s.destroyCustom(cleanupCtx, e)
		return
	}
	if r.Phase != Building {
		return
	}
	if result.Err != nil || result.Completion != DefinitelyCompleted || s.expired(e, s.now()) || s.storageBlocked {
		r.Phase = Failed
		s.records[e] = r
		_ = s.commit(clone(s.entries))
		return
	}
	r.Phase = Finalizing
	s.records[e] = r
	// Reuse the shared backend gate for container publication.
	createPermit, err := s.backend.Acquire(e, "create")
	if err != nil {
		r.Phase = Failed
		s.records[e] = r
		_ = s.commit(clone(s.entries))
		return
	}
	if err = s.commit(clone(s.entries)); err != nil {
		_ = createPermit.Resolve(e, DefinitelyCompleted)
		return
	}
	finalCtx, cancel := context.WithTimeout(ctx, s.toolTimeout)
	defer cancel()
	if b := s.builds[e]; b != nil {
		b.running = true
	}
	s.mu.Unlock()
	err = s.runtime.Create(finalCtx, e, r.Image)
	s.mu.Lock()
	if b := s.builds[e]; b != nil {
		b.running = false
	}
	// Backend evidence belongs to G even if publication was revoked meanwhile.
	if !s.ready {
		_ = createPermit.Resolve(e, completionOf(err))
		return
	}
	if saveErr := s.accountRuntime(createPermit, err); saveErr != nil {
		return
	}
	if err == nil {
		err = finalCtx.Err()
	}
	if err == nil {
		objects, discoverErr := s.discover(finalCtx)
		err = discoverErr
		if err == nil {
			if _, ok := objects[e]; !ok {
				err = ErrNotReady
			}
		}
	}
	if s.records[e].Phase == Deleting {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), s.toolTimeout)
		defer cancel()
		_ = s.destroyCustom(cleanupCtx, e)
		return
	}
	// Authority and absolute/idle TTL are rechecked under lifecycle serialization.
	if !s.ready || s.entries[e.Workspace] != e || s.records[e].Phase != Finalizing {
		return
	}
	if err != nil || s.expired(e, s.now()) {
		r.Phase = Failed
	} else {
		r.Phase = Present
	}
	s.records[e] = r
	_ = s.commit(clone(s.entries))
}

func (s *Service) destroyCustom(ctx context.Context, e Exact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r := s.records[e]
	// Revoke publication before cancellation, discovery or backend accounting.
	if r.Phase != Deleting {
		r.Phase = Deleting
		s.records[e] = r
		if err := s.commit(clone(s.entries)); err != nil {
			return err
		}
	}
	if b := s.builds[e]; b != nil {
		b.cancel()
		if b.running {
			return ErrOutcomeUnknown
		}
	}
	if s.backend.has(e) {
		return ErrOutcomeUnknown
	}
	if _, err := s.discover(ctx); err != nil {
		return err
	}
	runtime, ok := s.runtime.(BuildRuntime)
	if !ok {
		return ErrNotReady
	}
	permit, err := s.backend.Acquire(e, "build-cleanup")
	if err != nil {
		return err
	}
	if err = s.commit(clone(s.entries)); err != nil {
		_ = permit.Resolve(e, DefinitelyCompleted)
		return err
	}
	err = s.runtime.Destroy(ctx, e)
	if err == nil {
		err = runtime.CleanupBuild(ctx, e, s.buildPath(e))
	}
	if saveErr := s.accountRuntime(permit, err); saveErr != nil {
		return errors.Join(err, saveErr)
	}
	if err != nil {
		return err
	}
	objects, err := s.discover(ctx)
	if err != nil {
		return err
	}
	if _, exists := objects[e]; exists {
		return ErrOutcomeUnknown
	}
	if err := s.removeBuildData(ctx, e); err != nil {
		return err
	}
	next := clone(s.entries)
	delete(next, e.Workspace)
	delete(s.lifetimes, e)
	delete(s.records, e)
	return s.commit(next)
}

// Startup never adopts orphan Data/image intent, including after a StateDir
// override. Every resource-bearing custom generation must have journal authority.
func (s *Service) reconcileBuildState(ctx context.Context) error {
	if runtime, ok := s.runtime.(BuildRuntime); ok {
		generations, err := runtime.BuildGenerations(ctx)
		if err != nil {
			return err
		}
		for _, e := range generations {
			if e.validate() != nil || s.entries[e.Workspace] != e || !s.records[e].Custom {
				return ErrStaleGeneration
			}
		}
	}
	path := filepath.Join(s.dataDir, "builds")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ownedByService(info) {
		return ErrNotReady
	}
	dirs, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if !strings.HasPrefix(dir.Name(), "mcpws-") {
			continue
		}
		suffix, ok := strings.CutPrefix(dir.Name(), namePrefix)
		if !ok {
			return ErrNotReady
		}
		w, g, ok := strings.Cut(suffix, "-")
		e := Exact{WorkspaceKey(w), Generation(g)}
		if !ok || e.validate() != nil || dir.Name() != objectName(e) || s.entries[e.Workspace] != e || !s.records[e].Custom {
			return ErrStaleGeneration
		}
		if !dir.IsDir() || dir.Type()&os.ModeSymlink != 0 {
			return ErrNotReady
		}
	}
	return nil
}
