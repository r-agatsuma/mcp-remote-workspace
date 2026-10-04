package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/control"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

type operationsBackend struct {
	lifecycleFake
	err     error
	id      string
	options workspace.ExecOptions
}

func (b *operationsBackend) Exec(_ context.Context, id string, in workspace.ExecOptions) (*control.ExecResult, error) {
	b.id, b.options = id, in
	code := 7
	return &control.ExecResult{ExitCode: &code, Stdout: "out", Stderr: "err", StdoutTruncated: true}, b.err
}
func (b *operationsBackend) WriteText(_ context.Context, id, path, content string) (*control.TextResult, error) {
	b.id = id
	return &control.TextResult{Path: path, SizeBytes: int64(len(content)), SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}, b.err
}
func (b *operationsBackend) ReadText(_ context.Context, id, path string) (*control.TextResult, error) {
	b.id = id
	return &control.TextResult{Path: path, Content: "", SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}, b.err
}

func TestMCPOperations(t *testing.T) {
	b := &operationsBackend{}
	ctx, client := connectLifecycle(t, b)
	for _, tc := range []struct {
		name string
		args any
	}{
		{"exec", ExecInput{WorkspaceID: "ws", Argv: []string{"printf", "hello"}, Env: map[string]string{"TEST": "value"}, TimeoutSeconds: 1}},
		{"write_text", WriteTextInput{WorkspaceID: "ws", Path: "file", Content: ""}},
		{"read_text", ReadTextInput{WorkspaceID: "ws", Path: "file"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := client.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err != nil || r.IsError || b.id != "ws" {
				t.Fatalf("operation: %+v %v", r, err)
			}
			if tc.name == "exec" {
				var out ExecOutput
				data, _ := json.Marshal(r.StructuredContent)
				if err := json.Unmarshal(data, &out); err != nil {
					t.Fatal(err)
				}
				if *out.ExitCode != 7 || out.Stdout != "out" || out.Stderr != "err" || !out.StdoutTruncated || b.options.Cwd != "." || b.options.Env["TEST"] != "value" {
					t.Fatal("exec mapping/defaults incorrect")
				}
			}
			for _, failure := range []struct {
				err  error
				code ErrorCode
			}{
				{workspace.ErrNotFound, ErrorWorkspaceNotFound}, {workspace.ErrBlocked, ErrorBackend},
				{&control.Error{Code: "invalid_path", Message: "symlink"}, ErrorInvalidPath},
				{&control.Error{Code: "size_limit", Message: "oversized"}, ErrorSizeLimit},
				{&control.Error{Code: "non_utf8", Message: "binary"}, ErrorNonUTF8},
				{errors.New("transport failure"), ErrorBackend},
			} {
				b.err = failure.err
				r, err := client.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
				if err != nil {
					t.Fatal(err)
				}
				assertToolError(t, r, failure.code)
			}
			b.err = nil
		})
	}
}
