//go:build !linux

package workspace

import "context"

func (s *Service) removeBuildData(context.Context, Exact) error { return ErrNotReady }
