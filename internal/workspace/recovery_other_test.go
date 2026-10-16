//go:build !linux

package workspace

import (
	"context"
	"errors"
	"testing"
)

func TestRecoveryUnsupportedPlatform(t *testing.T) {
	if _, err := kernelBootID(); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if err := new(Podman).recoverPressureScope(context.Background(), Exact{}, ""); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
}
