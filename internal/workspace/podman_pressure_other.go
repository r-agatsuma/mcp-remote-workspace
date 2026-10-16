//go:build !linux

package workspace

import "context"

func (*Podman) recoverPressureScope(context.Context, Exact, string) error { return ErrNotReady }
