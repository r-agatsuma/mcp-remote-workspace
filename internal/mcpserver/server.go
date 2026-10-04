// Package mcpserver implements the stdio service's frozen v0 MCP tool contract.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

type Lifecycle interface {
	Create(context.Context) (workspace.Workspace, error)
	Destroy(context.Context, string) error
}

type Operations interface {
	Exec(context.Context, string, workspace.ExecRequest) (workspace.ExecResult, error)
	WriteText(context.Context, string, string, string) (workspace.TextResult, error)
	ReadText(context.Context, string, string) (workspace.TextResult, error)
}

// A nil lifecycle supports contract tests without requiring a prepared host.
// The daemon always supplies a recovered rootless Podman manager.
func New(lifecycle Lifecycle) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "mcp-workspaced", Version: "0.0.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "workspace_create", Description: "Create an isolated disposable empty development workspace. Public Git bootstrap is not implemented; no credentials.", InputSchema: createInputSchema(), OutputSchema: createOutputSchema()}, func(ctx context.Context, req *mcp.CallToolRequest, in WorkspaceCreateInput) (*mcp.CallToolResult, any, error) {
		if err := validateCreate(req, in); err != nil {
			return errorResult(*err), nil, nil
		}
		if lifecycle == nil || (in.Source != nil && in.Source.Type == SourcePublicGit) {
			return errorResult(ToolError{ErrorNotImplemented, "workspace source backend is not implemented"}), nil, nil
		}
		w, err := lifecycle.Create(ctx)
		if err != nil {
			return errorResult(ToolError{ErrorBackend, err.Error()}), nil, nil
		}
		return nil, WorkspaceCreateOutput{WorkspaceID: WorkspaceID(w.ID), CreatedAt: Timestamp(w.CreatedAt.Format(time.RFC3339Nano)), Source: SourceOutput{Type: SourceEmpty}}, nil
	})
	operations, _ := lifecycle.(Operations)
	addOperation(s, "exec", "Run argv without an implicit shell inside the workspace. cwd defaults to '.'; symlinks are rejected. Timeout defaults to 60 seconds (maximum 600). Each output stream is limited to 256 KiB with explicit truncation flags.", execInputSchema(), schemaFor[ExecOutput](), func(_ *mcp.CallToolRequest, in ExecInput) *ToolError { return validatePath(in.Cwd, false) }, operations, func(ctx context.Context, in ExecInput) (any, error) {
		out, err := operations.Exec(ctx, string(in.WorkspaceID), workspace.ExecRequest{Argv: in.Argv, Cwd: in.Cwd, Env: in.Env, Timeout: time.Duration(in.TimeoutSeconds) * time.Second})
		return ExecOutput(out), err
	})
	addOperation(s, "write_text", "Atomically replace one complete UTF-8 text file using stdin, without shell interpolation. Parent directories must already exist; symlinks are rejected.", schemaFor[WriteTextInput](), schemaFor[WriteTextOutput](), validateWrite, operations, func(ctx context.Context, in WriteTextInput) (any, error) {
		out, err := operations.WriteText(ctx, string(in.WorkspaceID), in.Path, in.Content)
		return WriteTextOutput{Path: ReturnedPath(out.Path), SizeBytes: out.SizeBytes, SHA256: SHA256(out.SHA256)}, err
	})
	addOperation(s, "read_text", "Read a complete UTF-8 file. Configured byte-size limit applies (default 1 MiB); oversized, binary, non-UTF-8 and symlink files are rejected. Content is never truncated.", schemaFor[ReadTextInput](), schemaFor[ReadTextOutput](), func(_ *mcp.CallToolRequest, in ReadTextInput) *ToolError { return validatePath(in.Path, true) }, operations, func(ctx context.Context, in ReadTextInput) (any, error) {
		out, err := operations.ReadText(ctx, string(in.WorkspaceID), in.Path)
		return ReadTextOutput{Path: ReturnedPath(out.Path), Content: out.Content, SizeBytes: out.SizeBytes, SHA256: SHA256(out.SHA256)}, err
	})
	addStub[WorkspaceChangesInput](s, "workspace_changes", "List Git change metadata relative to base_ref or the recorded source baseline, including local commits, staged, unstaged, and untracked files. No file bodies; missing baseline requires base_ref.", schemaFor[WorkspaceChangesInput](), changesOutputSchema(), nil)
	mcp.AddTool(s, &mcp.Tool{Name: "workspace_destroy", Description: "Permanently remove one disposable workspace. Unknown IDs return workspace_not_found.", InputSchema: schemaFor[WorkspaceDestroyInput](), OutputSchema: destroyOutputSchema()}, func(ctx context.Context, _ *mcp.CallToolRequest, in WorkspaceDestroyInput) (*mcp.CallToolResult, any, error) {
		if lifecycle == nil {
			return errorResult(ToolError{ErrorNotImplemented, "workspace backend is not implemented"}), nil, nil
		}
		if err := lifecycle.Destroy(ctx, string(in.WorkspaceID)); err != nil {
			code := ErrorBackend
			if errors.Is(err, workspace.ErrNotFound) {
				code = ErrorWorkspaceNotFound
			}
			return errorResult(ToolError{code, err.Error()}), nil, nil
		}
		return nil, WorkspaceDestroyOutput{WorkspaceID: in.WorkspaceID, Destroyed: true}, nil
	})
	return s
}

func addOperation[In any](s *mcp.Server, name, description string, input, output *jsonschema.Schema, validate func(*mcp.CallToolRequest, In) *ToolError, backend Operations, run func(context.Context, In) (any, error)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: description, InputSchema: input, OutputSchema: output}, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		if err := validate(req, in); err != nil {
			return errorResult(*err), nil, nil
		}
		if backend == nil {
			return errorResult(ToolError{ErrorNotImplemented, "workspace backend is not implemented"}), nil, nil
		}
		out, err := run(ctx, in)
		if err == nil {
			return nil, out, nil
		}
		code := ErrorBackend
		switch {
		case errors.Is(err, workspace.ErrNotFound):
			code = ErrorWorkspaceNotFound
		case errors.Is(err, workspace.ErrInvalidPath):
			code = ErrorInvalidPath
		case errors.Is(err, workspace.ErrInvalidArgument):
			code = ErrorInvalidArgument
		case errors.Is(err, workspace.ErrSizeLimit):
			code = ErrorSizeLimit
		case errors.Is(err, workspace.ErrNonUTF8):
			code = ErrorNonUTF8
		case errors.Is(err, context.DeadlineExceeded):
			code = ErrorTimeout
		}
		return errorResult(ToolError{code, err.Error()}), nil, nil
	})
}

func addStub[In any](s *mcp.Server, name, description string, input, output *jsonschema.Schema, validate func(*mcp.CallToolRequest, In) *ToolError) {
	// Explicit output schemas are reflected from the typed success structs.
	// A nil 'any' output lets the SDK preserve the structured error without
	// attempting to validate it as a successful output.
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: description, InputSchema: input, OutputSchema: output}, func(_ context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		if validate != nil {
			if err := validate(req, in); err != nil {
				return errorResult(*err), nil, nil
			}
		}
		return errorResult(ToolError{ErrorNotImplemented, "workspace backend is not implemented"}), nil, nil
	})
}

func errorResult(err ToolError) *mcp.CallToolResult {
	out := ErrorResult{Error: err}
	data, _ := json.Marshal(out) // Contains only strings.
	return &mcp.CallToolResult{IsError: true, StructuredContent: out, Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}
}

func validateCreate(_ *mcp.CallToolRequest, in WorkspaceCreateInput) *ToolError {
	if in.Source != nil && in.Source.Type == SourcePublicGit {
		u, err := url.Parse(in.Source.URL)
		if err != nil || u.User != nil {
			return &ToolError{ErrorInvalidArgument, "source.url must be a URL without embedded credentials"}
		}
	}
	return nil
}

func validateWrite(req *mcp.CallToolRequest, in WriteTextInput) *ToolError {
	if err := validatePath(in.Path, true); err != nil {
		return err
	}
	// JSON decoders can silently replace invalid UTF-8 or lone UTF-16
	// surrogates. Check the original content token before accepting it.
	var raw struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &raw); err != nil || !validTextToken(raw.Content) || !utf8.ValidString(in.Content) {
		return &ToolError{ErrorNonUTF8, "content must be valid UTF-8"}
	}
	return nil
}

// The SDK already checked JSON syntax. Here we check Unicode integrity only.
func validTextToken(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		value, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value < 0xd800 || value > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}

func validatePath(value string, file bool) *ToolError {
	normalized := path.Clean(value)
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsRune(value, '\x00') || normalized == ".." || strings.HasPrefix(normalized, "../") || (file && normalized == ".") {
		return &ToolError{ErrorInvalidPath, "path must be relative to the workspace root and must not escape it"}
	}
	for _, component := range strings.Split(value, "/") {
		if component == ".." {
			return &ToolError{ErrorInvalidPath, "path must not contain '..' components"}
		}
	}
	return nil
}
