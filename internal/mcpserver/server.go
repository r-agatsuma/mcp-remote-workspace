// Package mcpserver implements the stdio service's frozen v0 MCP tool contract.
package mcpserver

import (
	"context"
	"encoding/json"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func New() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "mcp-workspaced", Version: "0.0.0"}, nil)
	addStub[WorkspaceCreateInput](s, "workspace_create", "Create an isolated disposable development workspace, empty or from public Git; no credentials.", createInputSchema(), createOutputSchema(), validateCreate)
	addStub[ExecInput](s, "exec", "Run one process inside an existing workspace. argv has no implicit shell expansion; cwd is workspace-relative. Backend time and output limits apply.", execInputSchema(), schemaFor[ExecOutput](), func(_ *mcp.CallToolRequest, in ExecInput) *ToolError { return validatePath(in.Cwd, false) })
	addStub[WriteTextInput](s, "write_text", "Replace one complete UTF-8 text file at a workspace-relative path; no patch, append, or shell interpolation.", schemaFor[WriteTextInput](), schemaFor[WriteTextOutput](), validateWrite)
	addStub[ReadTextInput](s, "read_text", "Retrieve one complete UTF-8 text file at a workspace-relative path. Oversized or non-UTF-8 files return errors; content is never silently truncated.", schemaFor[ReadTextInput](), schemaFor[ReadTextOutput](), func(_ *mcp.CallToolRequest, in ReadTextInput) *ToolError { return validatePath(in.Path, true) })
	addStub[WorkspaceChangesInput](s, "workspace_changes", "List Git change metadata relative to base_ref or the recorded source baseline, including local commits, staged, unstaged, and untracked files. No file bodies; missing baseline requires base_ref.", schemaFor[WorkspaceChangesInput](), changesOutputSchema(), nil)
	addStub[WorkspaceDestroyInput](s, "workspace_destroy", "Permanently remove one disposable workspace. Unknown IDs return workspace_not_found once the backend is implemented.", schemaFor[WorkspaceDestroyInput](), destroyOutputSchema(), nil)
	return s
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
	return nil
}
