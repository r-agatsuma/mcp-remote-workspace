//go:build !linux

package workspace

import (
	"context"
	"os/exec"
)

func (*Podman) startBuildParent(context.Context, Exact) (string, error) { return "", ErrNotReady }
func (*Podman) stopBuildParent(context.Context, Exact) error            { return ErrNotReady }
func (*Podman) buildStorage(context.Context, string) error              { return ErrNotReady }
func removeBuildDirectory(string, Exact) error                          { return ErrNotReady }
func buildUnitCommand(context.Context, Exact, string, string, Policy, string, ...string) *exec.Cmd {
	panic("Linux build authority required")
}
