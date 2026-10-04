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
	"github.com/r-agatsuma/mcp-remote-workspace/internal/control"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

type Lifecycle interface {
	Create(context.Context) (workspace.Workspace, error)
	Destroy(context.Context, string) error
}

type Operations interface {
	Exec(context.Context, string, workspace.ExecOptions) (*control.ExecResult, error)
	WriteText(context.Context, string, string, string) (*control.TextResult, error)
	ReadText(context.Context, string, string) (*control.TextResult, error)
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
	addOperation[ExecInput](s, "exec", "Run an ephemeral job with explicit argv and no implicit shell. All descendants are terminated before return. Output is limited to 256 KiB per stream; timeout defaults to 60 seconds, maximum 300.", execInputSchema(), schemaFor[ExecOutput](), func(_ *mcp.CallToolRequest, in ExecInput) *ToolError { return validatePath(in.Cwd, false) }, func(ctx context.Context, in ExecInput) (any, error) {
		if operations == nil {
			return nil, nil
		}
		r, err := operations.Exec(ctx, string(in.WorkspaceID), workspace.ExecOptions{Argv: in.Argv, Cwd: in.Cwd, Env: in.Env, TimeoutSeconds: in.TimeoutSeconds})
		if err != nil {
			return nil, err
		}
		return ExecOutput{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr, StdoutTruncated: r.StdoutTruncated, StderrTruncated: r.StderrTruncated, TimedOut: r.TimedOut}, nil
	})
	addOperation[WriteTextInput](s, "write_text", "Atomically replace a UTF-8 file; create missing parent directories with mode 0755. Reject traversal and all symlinks. Backend text size limit applies.", schemaFor[WriteTextInput](), schemaFor[WriteTextOutput](), validateWrite, func(ctx context.Context, in WriteTextInput) (any, error) {
		if operations == nil {
			return nil, nil
		}
		r, err := operations.WriteText(ctx, string(in.WorkspaceID), in.Path, in.Content)
		if err != nil {
			return nil, err
		}
		return WriteTextOutput{Path: ReturnedPath(r.Path), SizeBytes: r.SizeBytes, SHA256: SHA256(r.SHA256)}, nil
	})
	addOperation[ReadTextInput](s, "read_text", "Read a complete UTF-8 text file. Reject binary files, traversal and all symlinks; oversized files return size_limit, never truncated content.", schemaFor[ReadTextInput](), schemaFor[ReadTextOutput](), func(_ *mcp.CallToolRequest, in ReadTextInput) *ToolError { return validatePath(in.Path, true) }, func(ctx context.Context, in ReadTextInput) (any, error) {
		if operations == nil {
			return nil, nil
		}
		r, err := operations.ReadText(ctx, string(in.WorkspaceID), in.Path)
		if err != nil {
			return nil, err
		}
		return ReadTextOutput{Path: ReturnedPath(r.Path), Content: r.Content, SizeBytes: r.SizeBytes, SHA256: SHA256(r.SHA256)}, nil
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

func addOperation[In any](s *mcp.Server, name, description string, input, output *jsonschema.Schema, validate func(*mcp.CallToolRequest, In) *ToolError, run func(context.Context, In) (any, error)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: description, InputSchema: input, OutputSchema: output}, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		if err := validate(req, in); err != nil {
			return errorResult(*err), nil, nil
		}
		result, err := run(ctx, in)
		if err != nil {
			code := ErrorBackend
			var domain *control.Error
			if errors.As(err, &domain) {
				code = ErrorCode(domain.Code)
			}
			if errors.Is(err, workspace.ErrNotFound) {
				code = ErrorWorkspaceNotFound
			}
			return errorResult(ToolError{code, err.Error()}), nil, nil
		}
		if result == nil {
			return errorResult(ToolError{ErrorNotImplemented, "workspace backend is not implemented"}), nil, nil
		}
		return nil, result, nil
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
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return &ToolError{ErrorInvalidPath, "parent traversal is forbidden"}
		}
	}
	return nil
}
