//go:build !linux

package workspace

func newBuildCgroups(string) buildCgroupManager { return nil }
