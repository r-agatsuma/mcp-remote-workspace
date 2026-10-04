package control

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestControlSubprocess(t *testing.T) {
	if os.Getenv("MCP_CONTROL_TEST") != "1" {
		return
	}
	if err := Serve(os.Getenv("MCP_CONTROL_ROOT"), os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func callHelper(t *testing.T, root string, req Request) Response {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestControlSubprocess$")
	cmd.Env = append(os.Environ(), "MCP_CONTROL_TEST=1", "MCP_CONTROL_ROOT="+root)
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin = bytes.NewReader(data)
	data, err = cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatalf("response %q: %v", data, err)
	}
	return response
}

func execRequest(argv ...string) Request {
	return Request{Operation: "exec", Argv: argv, Cwd: ".", TimeoutSeconds: 5, MaxTextBytes: DefaultTextLimit}
}

func TestArgvExecutionAndSeparateStreams(t *testing.T) {
	root := t.TempDir()
	r := callHelper(t, root, execRequest("printf", "hello"))
	if r.Error != nil || r.Exec == nil || r.Exec.ExitCode == nil || *r.Exec.ExitCode != 0 || r.Exec.Stdout != "hello" || r.Exec.Stderr != "" {
		t.Fatalf("printf: %+v", r)
	}
	r = callHelper(t, root, execRequest("printf", "%s", "$HOME; $(touch injected)"))
	if r.Exec == nil || r.Exec.Stdout != "$HOME; $(touch injected)" {
		t.Fatalf("argv interpreted by shell: %+v", r)
	}
	r = callHelper(t, root, execRequest("/bin/sh", "-c", "printf out; printf err >&2; exit 7"))
	if r.Exec == nil || *r.Exec.ExitCode != 7 || r.Exec.Stdout != "out" || r.Exec.Stderr != "err" {
		t.Fatalf("streams: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(root, "injected")); !os.IsNotExist(err) {
		t.Fatal("implicit shell")
	}
	req := execRequest("sh", "-c", "printf '%s' \"$TEST_VAR\"")
	req.Env = map[string]string{"TEST_VAR": "日本語"}
	r = callHelper(t, root, req)
	if r.Exec == nil || r.Exec.Stdout != "日本語" {
		t.Fatalf("environment: %+v", r)
	}
}

func TestExecutionTimeout(t *testing.T) {
	req := execRequest("/bin/sleep", "30")
	req.TimeoutSeconds = 1
	start := time.Now()
	r := callHelper(t, t.TempDir(), req)
	if r.Exec == nil || !r.Exec.TimedOut || r.Exec.ExitCode != nil || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout: %+v", r)
	}
}

func TestBothStreamsBounded(t *testing.T) {
	r := callHelper(t, t.TempDir(), execRequest("/bin/sh", "-c", "head -c 300000 /dev/zero | tr '\\000' x; head -c 300000 /dev/zero | tr '\\000' y >&2"))
	if r.Exec == nil || !r.Exec.StdoutTruncated || !r.Exec.StderrTruncated || len(r.Exec.Stdout) != StreamLimit || len(r.Exec.Stderr) != StreamLimit {
		t.Fatalf("output bounds: %+v", r.Error)
	}
	if r.Exec.Stdout != strings.Repeat("x", StreamLimit) || r.Exec.Stderr != strings.Repeat("y", StreamLimit) {
		t.Fatal("wrong streams")
	}
}

func TestCwdEffectiveTargetAndEnvPath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "child"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{"../", "child/../child", "/tmp", "escape"} {
		req := execRequest("printf", "hello")
		req.Cwd = cwd
		r := callHelper(t, root, req)
		if r.Error == nil || r.Error.Code != "invalid_path" {
			t.Fatalf("cwd %q: %+v", cwd, r)
		}
	}
	program := filepath.Join(root, "child", "custom")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf custom"), 0755); err != nil {
		t.Fatal(err)
	}
	req := execRequest("custom")
	req.Cwd = "child"
	req.Env = map[string]string{"PATH": "."}
	r := callHelper(t, root, req)
	if r.Exec == nil || r.Exec.Stdout != "custom" {
		t.Fatalf("PATH: %+v", r)
	}
}

func TestStreamByteAndUnicodeBoundaries(t *testing.T) {
	for _, size := range []int{StreamLimit - 1, StreamLimit, StreamLimit + 1} {
		var b boundedStream
		if n, err := b.Write(bytes.Repeat([]byte{'x'}, size)); err != nil || n != size {
			t.Fatal("bad Writer contract")
		}
		text, truncated := b.result()
		if len(text) > StreamLimit || truncated != (size > StreamLimit) {
			t.Fatalf("size %d: len=%d truncated=%v", size, len(text), truncated)
		}
	}
	var b boundedStream
	b.Write(bytes.Repeat([]byte{0xff}, StreamLimit))
	text, _ := b.result()
	if len(text) > StreamLimit {
		t.Fatal("UTF-8 replacement exceeded limit")
	}
}
