package control

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

func Normalize(value string, file bool) (string, error) {
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsRune(value, 0) || !utf8.ValidString(value) {
		return "", &Error{"invalid_path", "path must be workspace-relative"}
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", &Error{"invalid_path", "parent traversal is forbidden"}
		}
	}
	normalized := path.Clean(value)
	if file && normalized == "." {
		return "", &Error{"invalid_path", "path must name a file"}
	}
	return normalized, nil
}

// Walk by directory descriptors and reject every symlink, including internal
// ones. No mutable shell, Python runtime or /proc descriptor path is involved.
func openDirectory(root, relative string, create bool) (*os.File, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(relative, "/") {
		if part == "." {
			continue
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) && create {
			openErr = unix.Mkdirat(fd, part, 0755)
			if openErr == nil {
				next, openErr = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			}
		}
		unix.Close(fd)
		if openErr != nil {
			return nil, &Error{"invalid_path", "parent directory is missing, inaccessible or a symlink"}
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), relative), nil
}

func textResult(relative string, data []byte) *TextResult {
	hash := sha256.Sum256(data)
	return &TextResult{Path: relative, Content: string(data), SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
}

func readText(root string, req Request) (*TextResult, error) {
	relative, err := Normalize(req.Path, true)
	if err != nil {
		return nil, err
	}
	parent, err := openDirectory(root, path.Dir(relative), false)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), path.Base(relative), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, &Error{"invalid_path", "file is missing, inaccessible or a symlink"}
	}
	file := os.NewFile(uintptr(fd), relative)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &Error{"non_utf8", "only regular UTF-8 text files can be read"}
	}
	if info.Size() > req.MaxTextBytes {
		return nil, &Error{"size_limit", "text file exceeds the configured byte limit"}
	}
	data, err := io.ReadAll(io.LimitReader(file, req.MaxTextBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > req.MaxTextBytes {
		return nil, &Error{"size_limit", "text file exceeds the configured byte limit"}
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, &Error{"non_utf8", "file is binary or not valid UTF-8"}
	}
	return textResult(relative, data), nil
}

func writeText(root string, req Request) (*TextResult, error) {
	relative, err := Normalize(req.Path, true)
	if err != nil {
		return nil, err
	}
	if !utf8.ValidString(req.Content) {
		return nil, &Error{"non_utf8", "content must be valid UTF-8"}
	}
	if int64(len(req.Content)) > req.MaxTextBytes {
		return nil, &Error{"size_limit", "text content exceeds the configured byte limit"}
	}
	parent, err := openDirectory(root, path.Dir(relative), true)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	dir := int(parent.Fd())
	base := path.Base(relative)
	var stat unix.Stat_t
	err = unix.Fstatat(dir, base, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil && stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, &Error{"invalid_path", "target must be a regular file, never a symlink"}
	}
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return nil, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	temporary := ".mcp-write-" + hex.EncodeToString(random[:])
	fd, err := unix.Openat(dir, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0644)
	if err != nil {
		return nil, err
	}
	defer unix.Unlinkat(dir, temporary, 0)
	file := os.NewFile(uintptr(fd), temporary)
	defer file.Close()
	if _, err := file.WriteString(req.Content); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := unix.Renameat(dir, temporary, dir, base); err != nil {
		return nil, err
	}
	if err := parent.Sync(); err != nil {
		return nil, err
	}
	result := textResult(relative, []byte(req.Content))
	result.Content = ""
	return result, nil
}
