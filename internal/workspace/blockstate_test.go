package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlockMarkersPersistAndRequireExplicitRemoval(t *testing.T) {
	dir := t.TempDir()
	id := "ws_" + strings.Repeat("a", 64)
	p := &podman{stateDir: dir}
	if blocked, err := p.Blocked(id); err != nil || blocked {
		t.Fatalf("new marker: %v %v", blocked, err)
	}
	if err := p.Block(id); err != nil {
		t.Fatal(err)
	}
	restarted := &podman{stateDir: dir}
	if blocked, err := restarted.Blocked(id); err != nil || !blocked {
		t.Fatalf("restart marker: %v %v", blocked, err)
	}
	if err := restarted.Block(id); err != nil {
		t.Fatal("existing valid marker rejected", err)
	}
	if err := restarted.UnblockDestroyed(id); err != nil {
		t.Fatal(err)
	}
	if blocked, err := p.Blocked(id); err != nil || blocked {
		t.Fatalf("destroy marker: %v %v", blocked, err)
	}
	if err := p.Block("../escape"); err == nil {
		t.Fatal("marker traversal accepted")
	}
	if err := os.Symlink(filepath.Join(dir, "outside"), filepath.Join(dir, id)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Blocked(id); err == nil {
		t.Fatal("symlink block marker accepted")
	}
}
