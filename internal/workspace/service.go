package workspace

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

var ErrNotReady = errors.New("lifecycle admission is closed")
var ErrStaleGeneration = errors.New("stale workspace generation")

type State string

const (
	Absent  State = "ABSENT"
	Present State = "PRESENT"
)

type Config struct {
	StateDir    string
	Image       string           // digest-pinned canonical image, supplied by the operator
	PodmanPath  string           // absolute host-side executable path
	DataDir     string           // shared disposable pool; defaults to StateDir
	Policy      *Policy          // nil selects reference-host defaults; copied at Open
	Storage     StorageProbe     // nil selects normal-user statfs
	Clock       func() time.Time // nil selects time.Now; trusted host clock only
	ToolTimeout time.Duration    // zero selects 30s; operator-owned control-plane lifetime
}

func DefaultStateDir() (string, error) {
	account, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(account.HomeDir) {
		return "", errors.New("service account requires an absolute home directory")
	}
	return filepath.Join(account.HomeDir, ".local", "state", "mcp-workspaced"), nil
}

func (c Config) Validate() error {
	if c.ToolTimeout < 0 {
		return errors.New("tool timeout must be positive or zero for default")
	}
	if !filepath.IsAbs(c.StateDir) || !filepath.IsAbs(c.PodmanPath) || !validImage(c.Image) {
		return errors.New("absolute state directory/Podman path and digest-pinned canonical image required")
	}
	if c.DataDir != "" && !filepath.IsAbs(c.DataDir) {
		return errors.New("absolute Data pool path required")
	}
	if c.Policy != nil {
		return c.Policy.Validate()
	}
	return ReferencePolicy().Validate()
}

type Service struct {
	mu      sync.Mutex
	runtime Runtime
	image   string
	path    string
	lock    *os.File
	ready   bool
	entries map[WorkspaceKey]Exact
	// Fault injection remains private; production uses the durable writer.
	save           func(string, string, map[WorkspaceKey]Exact, map[Exact]Lifetime, bool, BackendState) error
	policy         Policy
	dataDir        string
	storage        StorageProbe
	now            func() time.Time
	lifetimes      map[Exact]Lifetime
	storageBlocked bool
	backend        *BackendLedger
	injections     map[Exact]*BackendPermit
	toolTimeout    time.Duration
	workers        map[Exact]context.CancelFunc
	workerDone     sync.WaitGroup
}

// Open obtains the account-wide singleton before constructing or discovering
// runtime state. StateDir overrides cannot bypass the account-wide lock.
func Open(ctx context.Context, cfg Config, factory func() Runtime) (*Service, error) {
	lockDir, err := DefaultStateDir()
	if err != nil {
		return nil, err
	}
	return open(ctx, cfg, factory, filepath.Join(lockDir, "daemon.lock"))
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ownedByService(info) {
		return errors.New("state/lock directory must be private and service-owned")
	}
	return nil
}

func open(ctx context.Context, cfg Config, factory func() Runtime, lockPath string) (_ *Service, err error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := privateDir(filepath.Dir(lockPath)); err != nil {
		return nil, err
	}
	lock, err := acquireLock(lockPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			lock.Close()
		}
	}()
	if err := privateDir(cfg.StateDir); err != nil {
		return nil, err
	}
	policy := ReferencePolicy()
	if cfg.Policy != nil {
		policy = *cfg.Policy
	}
	if cfg.DataDir == "" {
		cfg.DataDir = cfg.StateDir
	}
	if cfg.Storage == nil {
		cfg.Storage = localStorage{}
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.ToolTimeout == 0 {
		cfg.ToolTimeout = 30 * time.Second
	}
	backend, err := NewBackendLedger(policy.MaxBackendOperations, policy.MaxDiagnostics)
	if err != nil {
		return nil, err
	}
	s := &Service{runtime: factory(), image: cfg.Image, path: filepath.Join(cfg.StateDir, "journal.json"), lock: lock, save: writeAdmissionJournal,
		policy: policy, dataDir: cfg.DataDir, storage: cfg.Storage, now: cfg.Clock, backend: backend, injections: make(map[Exact]*BackendPermit), toolTimeout: cfg.ToolTimeout, workers: make(map[Exact]context.CancelFunc)}
	if s.runtime == nil {
		return nil, errors.New("runtime is required")
	}
	if podman, ok := s.runtime.(*Podman); ok {
		podman.buildDataDir = s.dataDir
	}
	if err := s.reconcile(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) reconcile(ctx context.Context) error {
	entries, lifetimes, blocked, backend, err := readAdmissionJournal(s.path, s.image)
	if err != nil {
		return err
	}
	if len(entries) > s.policy.MaxGenerations {
		return ErrGenerationLimit
	}
	for _, life := range lifetimes {
		if life.Activity.After(s.now()) {
			return errors.New("host clock predates durable activity; operator recovery required")
		}
	}
	if err := s.backend.restore(backend, entries); err != nil {
		return err
	}
	for _, op := range s.backend.Pending() {
		if op.Kind == "injection" {
			if s.injections[op.Exact] != nil {
				return errors.New("duplicate unfinished generation injection")
			}
			s.injections[op.Exact] = &BackendPermit{s.backend, op}
		}
	}
	if err := s.runtime.Initialize(ctx); err != nil {
		return err
	}
	if _, err := discover(ctx, s.runtime, entries, s.image, lifetimes); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Missing runtime objects retain PRESENT intent: startup does not repair,
	// restart, replace, or erase incomplete create/destroy operations.
	s.entries, s.lifetimes, s.storageBlocked, s.ready = entries, lifetimes, blocked, true
	changed := false
	for _, life := range lifetimes {
		if life.Build != nil && (life.Build.Phase == buildBuilding || life.Build.Phase == buildFinalizing) {
			life.Build.Phase = buildRecovery
			changed = true
		}
	}
	if changed {
		if err := s.commit(clone(entries)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Close() error {
	s.mu.Lock()
	s.ready = false
	for _, cancel := range s.workers {
		cancel()
	}
	s.mu.Unlock()
	s.workerDone.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

func (s *Service) admitted(identity Identity) error {
	if !s.ready {
		return ErrNotReady
	}
	if !validHex(string(identity.key)) {
		return errors.New("trusted identity is required")
	}
	return nil
}

func (s *Service) Status(identity Identity) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admitted(identity); err != nil {
		return "", err
	}
	if _, ok := s.entries[identity.key]; ok {
		if !s.present(s.entries[identity.key]) {
			return "", ErrNotReady
		}
		return Present, nil
	}
	return Absent, nil
}

// CurrentGeneration binds trusted ingress to the exact current PRESENT intent.
// It never creates, touches TTL, probes, or caches runtime state. Contention fails
// promptly instead of accumulating admission workers behind a runtime operation.
// The subsequent Inject must still recheck this Exact under lifecycle serialization;
// replacement between lookup and injection therefore fails rather than retargets.
func (s *Service) CurrentGeneration(ctx context.Context, identity Identity) (Exact, error) {
	if err := ctx.Err(); err != nil {
		return Exact{}, err
	}
	if !s.mu.TryLock() {
		return Exact{}, ErrNotReady
	}
	defer s.mu.Unlock()
	if err := s.admitted(identity); err != nil {
		return Exact{}, err
	}
	e, ok := s.entries[identity.key]
	if !ok {
		return Exact{}, ErrStaleGeneration
	}
	if !s.present(e) {
		return Exact{}, ErrNotReady
	}
	if s.backend.hasLifecycle(e) {
		return Exact{}, ErrOutcomeUnknown
	}
	return e, nil
}

func clone(entries map[WorkspaceKey]Exact) map[WorkspaceKey]Exact {
	next := make(map[WorkspaceKey]Exact, len(entries))
	for w, e := range entries {
		next[w] = e
	}
	return next
}

func (s *Service) commit(next map[WorkspaceKey]Exact) error {
	if err := s.save(s.path, s.image, next, s.lifetimes, s.storageBlocked, s.backend.state()); err != nil {
		s.ready = false
		return err
	}
	s.entries = next
	return nil
}

func (s *Service) Create(ctx context.Context, identity Identity) (Exact, error) {
	result, err := s.CreateWorkspace(ctx, identity, nil)
	return result.Exact, err
}

// CreateWorkspace accepts optional Dockerfile text. Exact is host-only; public
// responses expose only Status, never a workspace or build identifier.
func (s *Service) CreateWorkspace(ctx context.Context, identity Identity, dockerfile *string) (CreateResult, error) {
	e, status, err := s.createWorkspace(ctx, identity, dockerfile)
	return CreateResult{Exact: e, Status: status}, err
}

func (s *Service) createWorkspace(ctx context.Context, identity Identity, dockerfile *string) (Exact, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admitted(identity); err != nil {
		return Exact{}, "", err
	}
	if s.storageBlocked {
		return Exact{}, "", ErrStorageRecovery
	}
	if current, present := s.entries[identity.key]; present {
		if err := s.touch(current); err != nil {
			return Exact{}, "", err
		}
		return s.observeCreate(ctx, current)
	}
	if err := ctx.Err(); err != nil {
		return Exact{}, "", err
	}
	if _, err := discover(ctx, s.runtime, s.entries, s.image, s.lifetimes); err != nil {
		return Exact{}, "", err
	}
	if err := s.storageAdmission(ctx); err != nil {
		return Exact{}, "", err
	}
	if len(s.entries) >= s.policy.MaxGenerations {
		return Exact{}, "", ErrGenerationLimit
	}
	if dockerfile != nil {
		if _, err := dockerfileSources(*dockerfile); err != nil {
			return Exact{}, "", err
		}
		if _, ok := s.runtime.(BuildRuntime); !ok {
			return Exact{}, "", ErrNotReady
		}
	}
	generation, err := freshGeneration()
	if err != nil {
		return Exact{}, "", err
	}
	e := Exact{identity.key, generation}
	next := clone(s.entries)
	next[identity.key] = e
	permit, err := s.backend.Acquire(e, "create")
	if err != nil {
		return Exact{}, "", err
	}
	var control *BackendPermit
	if dockerfile != nil {
		// Reserve recovery capacity in the same ledger before submission. A stop
		// command may overlap an uncertain build; both count against the limit.
		control, err = s.backend.Acquire(e, "build-control")
		if err != nil {
			_ = permit.Resolve(e, DefinitelyCompleted)
			return Exact{}, "", err
		}
	}
	now := s.now()
	s.lifetimes[e] = Lifetime{Exact: e, Created: now, Activity: now}
	if dockerfile != nil {
		life := s.lifetimes[e]
		life.Build = &buildIntent{Phase: buildBuilding}
		s.lifetimes[e] = life
	}
	if err := s.commit(next); err != nil {
		_ = permit.Resolve(e, DefinitelyCompleted) // backend was never started
		if control != nil {
			_ = control.Resolve(e, DefinitelyCompleted)
		}
		return Exact{}, "", err
	}
	// Intent is durable before a possibly partial/unknown runtime operation.
	if dockerfile != nil {
		workerCtx, cancel := context.WithCancel(context.Background())
		s.workers[e] = cancel
		s.workerDone.Add(1)
		go s.buildCandidate(workerCtx, e, *dockerfile, permit, control)
		return e, "PROCESSING", nil
	}
	err = s.runtime.Create(ctx, e, s.image)
	if saveErr := s.accountRuntime(permit, err); saveErr != nil {
		return e, "", errors.Join(err, saveErr)
	}
	if err != nil {
		return e, "", err
	}
	objects, err := discover(ctx, s.runtime, s.entries, s.image, s.lifetimes)
	if err != nil {
		return e, "", err
	}
	if _, exists := objects[e]; !exists {
		return e, "", errors.New("created generation could not be confirmed")
	}
	return e, "PRESENT", nil
}

func (s *Service) Destroy(ctx context.Context, identity Identity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admitted(identity); err != nil {
		return err
	}
	if s.storageBlocked {
		return ErrStorageRecovery
	}
	e, present := s.entries[identity.key]
	if !present {
		return nil
	}
	return s.destroy(ctx, e)
}

// DestroyGeneration is for host-side delayed cleanup, never caller-supplied
// IDs. Fencing and runtime mutation share the same critical section.
func (s *Service) DestroyGeneration(ctx context.Context, e Exact) error {
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
	return s.destroy(ctx, e)
}

// WithGeneration fences delayed publication/callbacks. The host callback runs
// under the lifecycle lock and must not reenter this Service.
func (s *Service) WithGeneration(e Exact, callback func() error) error {
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
	return callback()
}

func (s *Service) destroy(ctx context.Context, e Exact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if life := s.lifetimes[e]; life.Build != nil {
		life.Build.Phase = buildRecovery
		if err := s.commit(clone(s.entries)); err != nil {
			return err
		}
		if cancel := s.workers[e]; cancel != nil {
			cancel()
			return ErrOutcomeUnknown
		}
	}
	if _, err := discover(ctx, s.runtime, s.entries, s.image, s.lifetimes); err != nil {
		return err
	}
	if s.backend.has(e) {
		return ErrOutcomeUnknown
	}
	permit, err := s.backend.Acquire(e, "destroy")
	if err != nil {
		return err
	}
	if err := s.commit(clone(s.entries)); err != nil {
		_ = permit.Resolve(e, DefinitelyCompleted)
		return err
	}
	err = s.runtime.Destroy(ctx, e)
	if err == nil && s.lifetimes[e].Build != nil {
		builder, ok := s.runtime.(BuildRuntime)
		if !ok {
			err = ErrNotReady
		} else {
			err = builder.CleanupBuild(ctx, e, s.dataDir, s.lifetimes[e].Build.Image)
		}
	}
	if saveErr := s.accountRuntime(permit, err); saveErr != nil {
		return errors.Join(err, saveErr)
	}
	if err != nil {
		if s.lifetimes[e].Build != nil && completionOf(err) == OutcomeUnknown {
			return errors.Join(ErrOutcomeUnknown, err)
		}
		return err
	}
	objects, err := discover(ctx, s.runtime, s.entries, s.image, s.lifetimes)
	if err != nil {
		return err
	}
	if _, exists := objects[e]; exists {
		return errors.New("destroy did not establish exact-generation absence")
	}
	next := clone(s.entries)
	delete(next, e.Workspace)
	delete(s.lifetimes, e)
	return s.commit(next)
}
