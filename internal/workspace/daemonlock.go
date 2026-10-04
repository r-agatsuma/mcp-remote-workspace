package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var ErrDaemonActive = errors.New("another mcp-workspaced daemon is active for this service account/runtime profile")

// Production runners acquire exclusivity before New issues its first command.
// In-memory runners used by unit tests do not access the shared Podman registry.
type daemonLocker interface {
	acquireDaemonLock() (int, error)
}

func (p *podman) acquireDaemonLock() (int, error) {
	return acquireDaemonLock(p.dir)
}

func acquireDaemonLock(dir string) (int, error) {
	if err := privateDirectory(dir); err != nil {
		return -1, err
	}
	var directory unix.Stat_t
	if err := unix.Lstat(dir, &directory); err != nil {
		return -1, err
	}
	if directory.Uid != uint32(os.Geteuid()) {
		return -1, errors.New("daemon runtime directory must be owned by the service user")
	}
	// Never unlink or replace this file: all contenders must lock the same inode.
	// CLOEXEC prevents Podman/conmon descendants from retaining daemon ownership.
	fd, err := unix.Open(filepath.Join(dir, "daemon.lock"), unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return -1, fmt.Errorf("open daemon lock: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0077 != 0 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		unix.Close(fd)
		return -1, errors.New("daemon lock must be a private service-owned regular file with one link")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) {
			return -1, ErrDaemonActive
		}
		return -1, fmt.Errorf("acquire daemon lock: %w", err)
	}
	return fd, nil
}
