//go:build !linux

package workspace

import (
	"errors"
	"os"
)

func ownedByService(os.FileInfo) bool { return false }
func acquireLock(string) (*os.File, error) {
	return nil, errors.New("daemon singleton requires Linux")
}
