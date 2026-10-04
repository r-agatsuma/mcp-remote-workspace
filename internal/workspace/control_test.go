package workspace

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestStaticControlArtifact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "mcp-control")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/mcp-control")
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("static control build: %s %v", data, err)
	}
	if err := verifyControlArtifact(binary); err != nil {
		t.Fatal(err)
	}
	// Execute the built static artifact with no loader/startup environment.
	cmd = exec.CommandContext(ctx, binary, "operate")
	cmd.Env = []string{"PATH=/nonexistent", "LD_PRELOAD=/nonexistent/library.so", "PYTHONPATH=/nonexistent"}
	cmd.Stdin = bytes.NewBufferString(`{"operation":"unknown","max_text_bytes":1}`)
	data, err := cmd.Output()
	if err != nil || !bytes.Contains(data, []byte(`"invalid_argument"`)) {
		t.Fatalf("static runtime: %s %v", data, err)
	}
}

func TestRuntimeRejectsMissingOrMutableArtifact(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	if err := verifyControlRuntime(missing); err == nil {
		t.Fatal("missing control runtime accepted")
	}
	file := filepath.Join(dir, "mutable")
	if err := os.WriteFile(file, []byte("#!/bin/sh\nexit 0"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := verifyControlRuntime(file); err == nil {
		t.Fatal("mutable shell control runtime accepted")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if err := verifyControlRuntime(link); err == nil {
		t.Fatal("symlink control runtime accepted")
	}
	if err := verifyControlArtifact(file); err == nil {
		t.Fatal("shell runtime accepted as static binary")
	}
}
