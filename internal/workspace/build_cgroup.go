package workspace

import "context"

// The adapter owns this capability; neither Dockerfile nor ingress supplies it.
// Release is called only after terminal backend handling and exact engine cleanup.
type buildCgroupManager interface {
	Prepare(context.Context, Exact, uint64) (string, error)
	Check(context.Context, Exact) error
	Release(context.Context, Exact) error
	Generations(context.Context) ([]Exact, error)
}

const buildSlicePrefix = "mcpwsbuild"

// No hyphens: a flat user-manager slice, without implicit intermediate slices.
func buildSlice(e Exact) string {
	return buildSlicePrefix + string(e.Workspace) + string(e.Generation) + ".slice"
}

// Buildah passes CgroupParent to OCI linux.cgroupsPath. With systemd the format
// is slice:scope-prefix:scope-name. The scope name is exact across all workspaces.
func buildCgroupParent(e Exact) string {
	return buildSlice(e) + ":buildah:" + string(e.Workspace) + string(e.Generation)
}
