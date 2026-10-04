package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

type testLifecycle struct{}

func (testLifecycle) Create(context.Context) (workspace.Workspace, error) {
	return workspace.Workspace{}, errors.New("test backend unavailable")
}
func (testLifecycle) Destroy(context.Context, string) error { return workspace.ErrNotFound }

// Exercise the real stdio serving path in a subprocess while mocking the
// backend, so normal tests need neither Podman nor a prepared service account.
func TestStdioHelperProcess(t *testing.T) {
	if os.Getenv("MCP_STDIO_TEST") != "1" {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, testLifecycle{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestStdioLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := func() *exec.Cmd {
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestStdioHelperProcess$")
		cmd.Env = append(os.Environ(), "MCP_STDIO_TEST=1")
		return cmd
	}
	t.Run("EOF before initialization", func(t *testing.T) {
		cmd := command()
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
			cmd := command()
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
