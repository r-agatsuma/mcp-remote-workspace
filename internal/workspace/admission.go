package workspace

import (
	"context"
	"errors"
	"path"
	"time"
)

var (
	ErrGenerationLimit = errors.New("resource-bearing generation limit reached")
	ErrBackendLimit    = errors.New("live backend operation limit reached")
	ErrOutcomeUnknown  = errors.New("backend completion is unknown")
	ErrInjectionBusy   = errors.New("generation has an unfinished process injection")
	ErrProcessUnsafe   = errors.New("exact-generation process/cgroup state is unsafe or uncertain")
	ErrStorageRecovery = errors.New("storage admission requires operator recovery")
	ErrNotExpired      = errors.New("generation is not TTL eligible")
)

// Policy is operator-owned. Callers never supply policy or expiry extensions.
// Zero values are invalid; use ReferencePolicy and override explicit fields.
type Policy struct {
	MemoryBytes          int64
	MaxGenerations       int
	SwapBytes            int64
	CPUQuota             int64 // -1 means no hard quota
	CPUWeight            uint64
	PIDsMax              uint64
	MigrationTasksMax    uint64
	NoFile               uint64 // soft == hard
	MaxBackendOperations int
	MaxDiagnostics       int
	IdleTTL              time.Duration
	AbsoluteTTL          time.Duration
	MinAvailableBytes    uint64
	MinFreeInodes        uint64
}

// ReferencePolicy contains deployment defaults, not architecture constants.
func ReferencePolicy() Policy {
	return Policy{
		MemoryBytes: 4 << 30, MaxGenerations: 2, SwapBytes: 0,
		CPUQuota: -1, CPUWeight: 100, PIDsMax: 512, MigrationTasksMax: 513,
		NoFile: 1024, MaxBackendOperations: 4, MaxDiagnostics: 16,
		IdleTTL: 30 * time.Minute, AbsoluteTTL: 24 * time.Hour,
		MinAvailableBytes: 1 << 30, MinFreeInodes: 1024,
	}
}

func (p Policy) Validate() error {
	if p.MemoryBytes <= 0 || p.MaxGenerations <= 0 || p.SwapBytes < 0 ||
		p.SwapBytes > int64(^uint64(0)>>1)-p.MemoryBytes || (p.CPUQuota != -1 && p.CPUQuota <= 0) ||
		p.CPUWeight < 1 || p.CPUWeight > 10000 || p.PIDsMax == 0 || p.MigrationTasksMax != p.PIDsMax+1 ||
		p.PIDsMax == ^uint64(0) || p.NoFile == 0 || p.MaxBackendOperations <= 0 || p.MaxDiagnostics < 0 ||
		p.IdleTTL <= 0 || p.AbsoluteTTL <= 0 || p.MinAvailableBytes == 0 || p.MinFreeInodes == 0 {
		return errors.New("invalid operator admission policy")
	}
	return nil
}

type StorageSample struct {
	AvailableBytes uint64 // available to normal users, excluding reserved blocks
	FreeInodes     uint64
}

type StorageProbe interface {
	Sample(context.Context, string) (StorageSample, error)
}

// ProcessSnapshot must be freshly attested by the trusted host runtime probe.
// PIDStart and CgroupID prevent PID/path reuse from authorizing another target.
// Tasks includes all tasks in the resource-bearing cgroup subtree.
type ProcessSnapshot struct {
	Exact                  Exact
	ObjectID               string
	PID                    int
	PIDStart               uint64
	CgroupPath             string
	CgroupID               uint64
	Tasks                  uint64
	MemoryBytes            int64
	SwapBytes              int64
	CPUQuota               int64
	CPUWeight              uint64
	PIDsMax                uint64
	NoFileSoft, NoFileHard uint64
}

// ProcessProbe is an optional runtime capability. Missing capability fails closed.
// ProbeProcess must validate live PID membership and immutable object/cgroup
// identity for Exact, and read effective limits, not cached configuration.
type ProcessProbe interface {
	ProbeProcess(context.Context, Exact) (ProcessSnapshot, error)
}

func (p Policy) validateProcess(e Exact, v ProcessSnapshot) error {
	if v.Exact != e || !validHex(v.ObjectID) || v.PID <= 0 || v.PIDStart == 0 ||
		v.CgroupPath == "/" || !path.IsAbs(v.CgroupPath) || path.Clean(v.CgroupPath) != v.CgroupPath || v.CgroupID == 0 || v.Tasks == 0 ||
		v.MemoryBytes != p.MemoryBytes || v.SwapBytes != p.SwapBytes ||
		v.CPUQuota != p.CPUQuota || v.CPUWeight != p.CPUWeight || v.PIDsMax != p.PIDsMax ||
		v.NoFileSoft != p.NoFile || v.NoFileHard != p.NoFile {
		return ErrProcessUnsafe
	}
	// 513 is a bounded migration observation, never permission to inject at 512+.
	if v.Tasks > p.MigrationTasksMax {
		return ErrProcessUnsafe
	}
	if v.Tasks >= p.PIDsMax {
		return ErrInjectionBusy
	}
	return nil
}
