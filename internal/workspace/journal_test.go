//go:build linux

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCorruptJournalPreventsRuntimeAccess(t *testing.T) {
	i := identity(t, "s", "c")
	g, err := freshGeneration()
	if err != nil {
		t.Fatal(err)
	}
	e := Exact{i.Workspace(), g}
	for _, data := range []string{
		`{`, `null`, `{"version":2}`, `{"version":1,"image":"wrong"}`,
		jsonText(t, journal{Version: 1, Image: testImage, Entries: []Exact{e, e}}),
		jsonText(t, journal{Version: 1, Image: testImage, Entries: []Exact{{Workspace: i.Workspace()}}}),
		jsonText(t, journal{Version: 1, Image: testImage}) + ` {}`,
		`{"version":1,"image":"` + testImage + `","unexpected":true}`,
	} {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig(filepath.Join(dir, "state"))
			if err := privateDir(cfg.StateDir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.StateDir, "journal.json"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			f := newFake()
			if s, err := open(context.Background(), cfg, func() Runtime { return f }, filepath.Join(dir, "lock", "daemon.lock")); err == nil || s != nil {
				t.Fatal("corrupt journal admitted")
			}
			if f.initializes != 0 || f.lists != 0 {
				t.Fatal("runtime accessed with invalid durable authority")
			}
		})
	}
}

func TestLockAndJournalRejectSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if lock, err := acquireLock(link); err == nil {
		lock.Close()
		t.Fatal("symlink lock accepted")
	}
	if _, err := readJournal(link, testImage); err == nil {
		t.Fatal("symlink journal accepted")
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournal(target, testImage); err == nil {
		t.Fatal("public journal accepted")
	}
}
