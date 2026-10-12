//go:build !linux

package workspace

import (
	"context"
	"errors"
)

type localStorage struct{}

func (localStorage) Sample(context.Context, string) (StorageSample, error) {
	return StorageSample{}, errors.New("normal-user storage sampling requires Linux")
}
