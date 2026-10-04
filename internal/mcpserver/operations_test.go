package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

type operationsFake struct {
	lifecycleFake
	execInput         workspace.ExecRequest
	id, path, content string
	err               error
	timedOut          bool
}

func (f *operationsFake) Exec(_ context.Context, id string, in workspace.ExecRequest) (workspace.ExecResult, error) {
	f.id, f.execInput = id, in
	code := 7
	out := workspace.ExecResult{ExitCode: &code, Stdout: "out", Stderr: "err", StdoutTruncated: true, TimedOut: f.timedOut}
	if f.timedOut {
		out.ExitCode = nil
	}
	return out, f.err
}
func (f *operationsFake) WriteText(_ context.Context, id, name, content string) (workspace.TextResult, error) {
	f.id, f.path, f.content = id, name, content
	return workspace.TextResult{Path: "file", SizeBytes: 6, SHA256: strings.Repeat("a", 64)}, f.err
}
func (f *operationsFake) ReadText(_ context.Context, id, name string) (workspace.TextResult, error) {
	f.id, f.path = id, name
	return workspace.TextResult{Path: "file", Content: "日本", SizeBytes: 6, SHA256: strings.Repeat("a", 64)}, f.err
}

func TestMCPOperations(t *testing.T) {
	f := &operationsFake{}
	ctx, client := connectLifecycle(t, f)
	call := func(name string, args any) *mcp.CallToolResult {
		t.Helper()
		out, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || out == nil || out.IsError {
			t.Fatalf("%s: %+v, %v", name, out, err)
		}
		return out
	}
	result := call("exec", map[string]any{"workspace_id": "ws_test", "argv": []string{"printf", "%s", ""}, "env": map[string]string{"KEY": "value"}, "timeout_seconds": 2})
	if f.id != "ws_test" || f.execInput.Cwd != "." || f.execInput.Timeout != 2*time.Second || f.execInput.Env["KEY"] != "value" || len(f.execInput.Argv) != 3 || f.execInput.Argv[2] != "" {
		t.Fatalf("exec input: %+v", f.execInput)
	}
	var out ExecOutput
	data, _ := json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(data, &out); err != nil || out.ExitCode == nil || *out.ExitCode != 7 || out.Stdout != "out" || out.Stderr != "err" || !out.StdoutTruncated {
		t.Fatalf("exec output: %+v, %v", out, err)
	}
	f.timedOut = true
	result = call("exec", ExecInput{WorkspaceID: "ws_test", Argv: []string{"sleep", "10"}})
	data, _ = json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(data, &out); err != nil || !out.TimedOut || out.ExitCode != nil {
		t.Fatalf("timeout output: %+v, %v", out, err)
	}
	call("write_text", WriteTextInput{WorkspaceID: "ws_test", Path: "./file", Content: "日本"})
	if f.id != "ws_test" || f.path != "./file" || f.content != "日本" {
		t.Fatalf("write input: %+v", f)
	}
	result = call("read_text", ReadTextInput{WorkspaceID: "ws_test", Path: "file"})
	var read ReadTextOutput
	data, _ = json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(data, &read); err != nil || read.Content != "日本" || read.SizeBytes != 6 || read.Path != "file" || read.SHA256 != SHA256(strings.Repeat("a", 64)) {
		t.Fatalf("read output: %+v, %v", read, err)
	}
}

func TestMCPOperationErrors(t *testing.T) {
	f := &operationsFake{}
	ctx, client := connectLifecycle(t, f)
	for _, tc := range []struct {
		err  error
		code ErrorCode
	}{
		{workspace.ErrNotFound, ErrorWorkspaceNotFound}, {workspace.ErrInvalidPath, ErrorInvalidPath},
		{workspace.ErrInvalidArgument, ErrorInvalidArgument}, {workspace.ErrNonUTF8, ErrorNonUTF8},
		{workspace.ErrSizeLimit, ErrorSizeLimit}, {context.DeadlineExceeded, ErrorTimeout}, {errors.New("backend failure"), ErrorBackend},
	} {
		f.err = tc.err
		for name, args := range map[string]any{
			"exec":       ExecInput{WorkspaceID: "ws", Argv: []string{"true"}},
			"write_text": WriteTextInput{WorkspaceID: "ws", Path: "file", Content: "text"},
			"read_text":  ReadTextInput{WorkspaceID: "ws", Path: "file"},
		} {
			result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			assertToolError(t, result, tc.code)
		}
	}
}
