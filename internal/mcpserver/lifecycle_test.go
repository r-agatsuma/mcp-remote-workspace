package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

type lifecycleFake struct {
	w                     workspace.Workspace
	createErr, destroyErr error
	createCalls           int
	destroyedID           string
}

func (f *lifecycleFake) Create(context.Context) (workspace.Workspace, error) {
	f.createCalls++
	return f.w, f.createErr
}
func (f *lifecycleFake) Destroy(_ context.Context, id string) error {
	f.destroyedID = id
	return f.destroyErr
}

func TestMCPLifecycle(t *testing.T) {
	f := &lifecycleFake{w: workspace.Workspace{ID: "ws_opaque", CreatedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}}
	ctx, client := connectLifecycle(t, f)
	for _, args := range []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`{"source":{"type":"empty"}}`)} {
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "workspace_create", Arguments: args})
		if err != nil || result == nil || result.IsError {
			t.Fatalf("create: %+v, %v", result, err)
		}
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var out WorkspaceCreateOutput
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		if out.WorkspaceID != WorkspaceID(f.w.ID) || out.CreatedAt != "2026-10-04T00:00:00Z" || out.Source.Type != SourceEmpty {
			t.Fatalf("create output: %+v", out)
		}
	}
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "workspace_create", Arguments: json.RawMessage(`{"source":{"type":"public_git","url":"https://example.com/repo.git"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	assertToolError(t, result, ErrorNotImplemented)
	if f.createCalls != 2 {
		t.Fatal("public Git invoked lifecycle")
	}
	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "workspace_destroy", Arguments: WorkspaceDestroyInput{WorkspaceID: WorkspaceID(f.w.ID)}})
	if err != nil || result == nil || result.IsError || f.destroyedID != f.w.ID {
		t.Fatalf("destroy: %+v, %v", result, err)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out WorkspaceDestroyOutput
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.WorkspaceID != WorkspaceID(f.w.ID) || !out.Destroyed {
		t.Fatalf("destroy output: %+v", out)
	}
	f.createErr = errors.New("create failed")
	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "workspace_create", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	assertToolError(t, result, ErrorBackend)
	for _, tc := range []struct {
		err  error
		code ErrorCode
	}{{workspace.ErrNotFound, ErrorWorkspaceNotFound}, {errors.New("remove failed"), ErrorBackend}} {
		f.destroyErr = tc.err
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "workspace_destroy", Arguments: WorkspaceDestroyInput{WorkspaceID: "unknown"}})
		if err != nil {
			t.Fatal(err)
		}
		assertToolError(t, result, tc.code)
	}
}
