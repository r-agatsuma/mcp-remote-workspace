package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	var domain *Error
	if !errors.As(err, &domain) || domain.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

func TestTextRoundTripAndAtomicReplace(t *testing.T) {
	root := t.TempDir()
	content := "日本語\n$(touch injected); 'quoted'\n"
	req := Request{Path: "./nested//file '$(touch injected)'", Content: content, MaxTextBytes: DefaultTextLimit}
	written, err := writeText(root, req)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256([]byte(content))
	if written.Path != "nested/file '$(touch injected)'" || written.SizeBytes != int64(len(content)) || written.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("write: %+v", written)
	}
	read, err := readText(root, req)
	if err != nil || read.Content != content || read.SHA256 != written.SHA256 {
		t.Fatalf("read: %+v, %v", read, err)
	}
	// Existing descriptors continue to see the old inode after atomic replace.
	old, err := os.Open(filepath.Join(root, written.Path))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	req.Content = "replacement"
	if _, err := writeText(root, req); err != nil {
		t.Fatal(err)
	}
	var preserved bytes.Buffer
	if _, err := preserved.ReadFrom(old); err != nil {
		t.Fatal(err)
	}
	if preserved.String() != content {
		t.Fatal("write modified old inode")
	}
	names, err := os.ReadDir(filepath.Join(root, "nested"))
	if err != nil || len(names) != 1 {
		t.Fatalf("temporary files left behind: %v, %v", names, err)
	}
	if _, err := os.Stat(filepath.Join(root, "injected")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("content or path was shell-interpolated")
	}
}

func TestFileTraversalAndSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, target string }{
		{"escape", outside}, {"absolute-file", filepath.Join(outside, "secret")}, {"internal", root}, {"dangling", "missing"},
	} {
		if err := os.Symlink(tc.target, filepath.Join(root, tc.name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"../secret", "a/../file", "a/../../secret", "/etc/passwd", "a\x00b", "escape/secret", "absolute-file", "internal/file", "dangling", "."} {
		t.Run(path, func(t *testing.T) {
			req := Request{Path: path, Content: "overwrite", MaxTextBytes: DefaultTextLimit}
			_, err := readText(root, req)
			expectCode(t, err, "invalid_path")
			_, err = writeText(root, req)
			expectCode(t, err, "invalid_path")
		})
	}
	data, err := os.ReadFile(filepath.Join(outside, "secret"))
	if err != nil || string(data) != "secret" {
		t.Fatal("outside target modified")
	}
}

func TestReadRejectsBinaryAndOversize(t *testing.T) {
	root := t.TempDir()
	for _, data := range [][]byte{{0xff, 0xfe}, {'a', 0, 'b'}} {
		if err := os.WriteFile(filepath.Join(root, "file"), data, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readText(root, Request{Path: "file", MaxTextBytes: 20})
		expectCode(t, err, "non_utf8")
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := readText(root, Request{Path: "file", MaxTextBytes: 4})
	expectCode(t, err, "size_limit")
	read, err := readText(root, Request{Path: "file", MaxTextBytes: 5})
	if err != nil || read.Content != "12345" {
		t.Fatalf("limit boundary: %+v, %v", read, err)
	}
	_, err = writeText(root, Request{Path: "file", Content: strings.Repeat("a", 6), MaxTextBytes: 5})
	expectCode(t, err, "size_limit")
	_, err = writeText(root, Request{Path: "file", Content: string([]byte{0xff}), MaxTextBytes: 5})
	expectCode(t, err, "non_utf8")
	if _, err = readText(root, Request{Path: ".", MaxTextBytes: 5}); err == nil {
		t.Fatal("read directory")
	}
	if err := os.WriteFile(filepath.Join(root, "empty"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := readText(root, Request{Path: "empty", MaxTextBytes: 5})
	if err != nil || r.SizeBytes != 0 || r.Content != "" {
		t.Fatalf("empty: %+v %v", r, err)
	}
}

func TestNoSpecialFileReadBlocking(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "directory"), 0755); err != nil {
		t.Fatal(err)
	}
	_, err := readText(root, Request{Path: "directory", MaxTextBytes: 5})
	expectCode(t, err, "non_utf8")
	if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = readText(root, Request{Path: "fifo", MaxTextBytes: 5})
	expectCode(t, err, "non_utf8")
}
