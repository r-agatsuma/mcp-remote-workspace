//go:build linux

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func restrictiveBuildTemp(t *testing.T, path string) string {
	t.Helper()
	mnt := filepath.Join(path, "tmp", "buildah123", "mnt")
	rootfs := filepath.Join(mnt, "rootfs")
	if err := os.MkdirAll(rootfs, 0700); err != nil {
		t.Fatal(err)
	}
	// More than one enumeration batch, including read-only files.
	for n := range 130 {
		if err := os.WriteFile(filepath.Join(rootfs, strconv.Itoa(n)), []byte("residue"), 0400); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		// Test fixture recovery if a failed assertion leaves restrictive residue.
		_ = os.Chmod(mnt, 0700)
		_ = os.Chmod(rootfs, 0700)
	})
	if err := os.Chmod(rootfs, 0000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mnt, 0100); err != nil {
		t.Fatal(err)
	}
	return mnt
}

func assertUnchangedDirectory(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != mode {
		t.Fatalf("directory changed before cleanup authority: %s %v %v", path, info, err)
	}
}

func TestRestrictiveBuildDataCancellationTTLAndReopen(t *testing.T) {
	for _, mode := range []string{"destroy", "ttl", "restart", "late"} {
		t.Run(mode, func(t *testing.T) {
			f := newBuilder()
			if mode == "late" {
				f.cancelObserved = make(chan struct{})
			}
			s, cfg, lock := openBuilder(t, f, nil)
			i := identity(t, "s", "restrictive-temp")
			r, err := s.CreateDockerfile(context.Background(), i, "FROM scratch")
			if err != nil {
				t.Fatal(err)
			}
			receiveExact(t, f.started)
			mnt := restrictiveBuildTemp(t, s.buildPath(r.Exact))
			neighbor := filepath.Join(s.dataDir, "builds", "unrelated")
			if err := os.Mkdir(neighbor, 0700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(neighbor, "keep")
			if err := os.WriteFile(sentinel, []byte("untouched"), 0400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(neighbor, 0500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(neighbor, 0700) })
			switch mode {
			case "restart":
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = open(context.Background(), cfg, func() Runtime { return f }, lock)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				assertState(t, s, i, Failed)
			case "ttl":
				s.mu.Lock()
				now := s.lifetimes[r.Exact].Created.Add(s.policy.AbsoluteTTL)
				s.now = func() time.Time { return now }
				s.mu.Unlock()
				if err := s.CleanupExpired(context.Background(), r.Exact); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal(err)
				}
			default:
				if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal(err)
				}
			}
			assertUnchangedDirectory(t, mnt, 0100)
			assertUnchangedDirectory(t, filepath.Join(mnt, "rootfs"), 0000)
			if mode == "late" {
				waitStorageSignal(t, f.cancelObserved)
				// Late definite completion can clean only its original generation.
				f.finish <- BackendResult{Completion: DefinitelyCompleted}
			} else {
				s.buildWG.Wait()
				if err := s.Destroy(context.Background(), i); !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatal("unknown backend allowed Data cleanup", err)
				}
				assertUnchangedDirectory(t, mnt, 0100)
				for _, op := range s.PendingBackend() {
					if err := s.RecoverBackend(op, DefinitelyCompleted); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "ttl" {
					err = s.CleanupExpired(context.Background(), r.Exact)
				} else {
					err = s.Destroy(context.Background(), i)
				}
				if err != nil {
					t.Fatal("exact Data cleanup required permission repair", err)
				}
			}
			waitBuildState(t, s, i, Absent)
			if _, err := os.Lstat(s.buildPath(r.Exact)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("ABSENT retained exact build/log/temp state", err)
			}
			assertUnchangedDirectory(t, neighbor, 0500)
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "untouched" {
				t.Fatal("neighbor changed", err)
			}
			if f.creates != 0 {
				t.Fatal("canceled build published")
			}
		})
	}
}

func cleanupFixture(t *testing.T) (*Service, Exact) {
	t.Helper()
	s := &Service{dataDir: t.TempDir()}
	e := Exact{identity(t, "s", "cleanup").Workspace(), Generation(strings.Repeat("a", 64))}
	if err := os.MkdirAll(s.buildPath(e), 0700); err != nil {
		t.Fatal(err)
	}
	return s, e
}

func TestBuildDataCleanupRejectsSymlinksAndEscapes(t *testing.T) {
	for _, location := range []string{"data", "builds", "generation", "nested", "internal"} {
		t.Run(location, func(t *testing.T) {
			s, e := cleanupFixture(t)
			target := filepath.Join(t.TempDir(), "target")
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(target, "keep")
			if err := os.WriteFile(sentinel, []byte("untouched"), 0400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, 0500); err != nil {
				t.Fatal(err)
			}
			protectedTarget := target
			t.Cleanup(func() { _ = os.Chmod(protectedTarget, 0700) })
			link := filepath.Join(s.buildPath(e), "escape")
			switch location {
			case "data":
				link = s.dataDir
			case "builds":
				link = filepath.Join(s.dataDir, "builds")
			case "generation":
				link = s.buildPath(e)
			case "internal":
				target = "." // Even a non-escaping symlink is not cleanup authority.
			}
			if location == "data" || location == "builds" || location == "generation" {
				if err := os.Rename(link, link+"-saved"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = os.Remove(link)
					_ = os.Rename(link+"-saved", link)
				})
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if err := s.removeBuildData(context.Background(), e); !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatal("symlink authorized cleanup", err)
			}
			if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatal("ambiguous symlink removed", err)
			}
			assertUnchangedDirectory(t, filepath.Dir(sentinel), 0500)
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "untouched" {
				t.Fatal("escaped exact generation", err)
			}
		})
	}
}

func TestBuildDataCleanupRejectsUnexpectedEntries(t *testing.T) {
	for _, kind := range []string{"fifo", "hardlink", "public-root", "file-root", "invalid-generation", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			s, e := cleanupFixture(t)
			path := s.buildPath(e)
			ctx := context.Background()
			var err error
			switch kind {
			case "fifo":
				err = unix.Mkfifo(filepath.Join(path, "fifo"), 0600)
			case "hardlink":
				sentinel := filepath.Join(s.dataDir, "keep")
				if err := os.WriteFile(sentinel, []byte("untouched"), 0400); err != nil {
					t.Fatal(err)
				}
				err = os.Link(sentinel, filepath.Join(path, "hardlink"))
			case "public-root":
				err = os.Chmod(path, 0755)
			case "file-root":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				err = os.WriteFile(path, nil, 0600)
			case "invalid-generation":
				e.Generation = "../../escape"
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.removeBuildData(ctx, e); !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatal("unexpected entry allowed cleanup", err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("uncertain tree removed", err)
			}
		})
	}
}

func TestBuildCleanupMountAndOwnershipChecks(t *testing.T) {
	// Read-only use of existing host mounts: no mount or privilege is created.
	root, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(root)
	fd, err := openBuildCleanupEntry(root, "proc")
	if err == nil {
		unix.Close(fd)
		t.Fatal("cleanup traversal crossed a mount")
	}
	if !errors.Is(err, unix.EXDEV) {
		t.Fatal("unexpected mount rejection", err)
	}
	if os.Geteuid() != 0 {
		if _, err := buildCleanupStat(root, false); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatal("foreign owner authorized cleanup", err)
		}
	}
}
