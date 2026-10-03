package main

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStdioLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "mcp-workspaced")
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	t.Run("EOF before initialization", func(t *testing.T) {
		cmd := exec.CommandContext(ctx, binary)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("EOF exit: %v, stderr: %s", err, &stderr)
		}
		if stdout.Len() != 0 {
			t.Errorf("unexpected stdout: %s", &stdout)
		}
	})
	for _, shutdown := range []string{"EOF after initialization", "SIGTERM", "SIGINT"} {
		t.Run(shutdown, func(t *testing.T) {
			cmd := exec.CommandContext(ctx, binary)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { stdin.Close(); stdout.Close() })
			client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "1"}, nil)
			session, err := client.Connect(ctx, &mcp.IOTransport{Reader: stdout, Writer: stdin}, nil)
			if err != nil {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal(err)
			}
			result, err := session.ListTools(ctx, nil)
			if err != nil {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal(err)
			}
			if len(result.Tools) != 6 {
				t.Errorf("discovered %d tools", len(result.Tools))
			}
			call, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "workspace_create", Arguments: map[string]any{}})
			if err != nil || call == nil || !call.IsError {
				t.Errorf("tool result: %#v, error: %v", call, err)
			}
			switch shutdown {
			case "EOF after initialization":
				err = stdin.Close()
			case "SIGTERM":
				err = cmd.Process.Signal(syscall.SIGTERM)
			case "SIGINT":
				err = cmd.Process.Signal(syscall.SIGINT)
			}
			if err != nil {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Errorf("shutdown: %v, stderr: %s", err, &stderr)
			}
			session.Close()
		})
	}
}
