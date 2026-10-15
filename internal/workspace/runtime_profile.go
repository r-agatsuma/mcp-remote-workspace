package workspace

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Unlike buildNetwork, the final profile carries no Buildah ownership token.
func workspaceNetwork() string {
	return "pasta:--no-map-gw,-t,none,-u,none,-T,none,-U,none"
}

// This bounded service state is never mounted into a workspace. Refuse stale
// nonempty configuration rather than executing hooks or inheriting host mounts.
func prepareRuntimeProfile(directory string) error {
	if !filepath.IsAbs(directory) {
		return errors.New("private runtime profile directory required")
	}
	if err := privateDir(directory); err != nil {
		return err
	}
	hooks := filepath.Join(directory, "hooks")
	if err := privateDir(hooks); err != nil {
		return err
	}
	f, err := os.Open(hooks)
	if err != nil {
		return err
	}
	entries, readErr := f.Readdirnames(1)
	closeErr := f.Close()
	if len(entries) != 0 || readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil {
		return errors.New("runtime hooks directory must be empty")
	}
	mounts := filepath.Join(directory, "mounts.conf")
	f, err = os.OpenFile(mounts, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		err = f.Close()
	}
	if err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(mounts)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ownedByService(info) || info.Size() != 0 {
		return errors.New("runtime default mounts file must be empty, private and service-owned")
	}
	return nil
}
