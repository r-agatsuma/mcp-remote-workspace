package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connect(t *testing.T) (context.Context, *mcp.ClientSession) {
	return connectLifecycle(t, nil)
}

func connectLifecycle(t *testing.T, backend Lifecycle) (context.Context, *mcp.ClientSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := New(backend).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "contract-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return ctx, client
}

func discover(t *testing.T) map[string]*mcp.Tool {
	t.Helper()
	ctx, client := connect(t)
	result, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := make(map[string]*mcp.Tool)
	for _, tool := range result.Tools {
		if tools[tool.Name] != nil {
			t.Fatalf("duplicate tool %q", tool.Name)
		}
		tools[tool.Name] = tool
	}
	return tools
}

func decodeSchema(t *testing.T, value any) *jsonschema.Schema {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	return &schema
}

func assertFields(t *testing.T, schema *jsonschema.Schema, required, optional string) {
	t.Helper()
	wantRequired := strings.Fields(required)
	gotRequired := slices.Clone(schema.Required)
	slices.Sort(wantRequired)
	slices.Sort(gotRequired)
	if !slices.Equal(gotRequired, wantRequired) {
		t.Errorf("required = %v, want %v", gotRequired, wantRequired)
	}
	wantProperties := append(strings.Fields(required), strings.Fields(optional)...)
	var gotProperties []string
	for name := range schema.Properties {
		gotProperties = append(gotProperties, name)
	}
	slices.Sort(wantProperties)
	slices.Sort(gotProperties)
	if !slices.Equal(gotProperties, wantProperties) {
		t.Errorf("properties = %v, want %v", gotProperties, wantProperties)
	}
}

func TestDiscoveryAndSchemaFields(t *testing.T) {
	tools := discover(t)
	want := []string{"exec", "read_text", "workspace_changes", "workspace_create", "workspace_destroy", "write_text"}
	var names []string
	for name := range tools {
		names = append(names, name)
	}
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Fatalf("tool names = %v, want %v", names, want)
	}
	cases := []struct{ name, inRequired, inOptional, outRequired, outOptional string }{
		{"workspace_create", "", "source", "workspace_id created_at source", ""},
		{"exec", "workspace_id argv", "cwd env timeout_seconds", "exit_code stdout stderr stdout_truncated stderr_truncated timed_out", ""},
		{"write_text", "workspace_id path content", "", "path size_bytes sha256", ""},
		{"read_text", "workspace_id path", "", "path content size_bytes sha256", ""},
		{"workspace_changes", "workspace_id", "base_ref", "base_ref changes", "head_sha"},
		{"workspace_destroy", "workspace_id", "", "workspace_id destroyed", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := tools[tc.name]
			if tool.Description == "" {
				t.Error("missing description")
			}
			assertFields(t, decodeSchema(t, tool.InputSchema), tc.inRequired, tc.inOptional)
			assertFields(t, decodeSchema(t, tool.OutputSchema), tc.outRequired, tc.outOptional)
		})
	}
	in := decodeSchema(t, tools["workspace_create"].InputSchema).Properties["source"]
	out := decodeSchema(t, tools["workspace_create"].OutputSchema).Properties["source"]
	assertFields(t, in, "type", "url ref")
	assertFields(t, out, "type", "url requested_ref resolved_commit_sha")
	if !reflect.DeepEqual(in.Properties["type"].Enum, []any{"empty", "public_git"}) {
		t.Errorf("source enum = %v", in.Properties["type"].Enum)
	}
	change := decodeSchema(t, tools["workspace_changes"].OutputSchema).Properties["changes"].Items
	assertFields(t, change, "status path", "old_path")
	if !reflect.DeepEqual(change.Properties["status"].Enum, []any{"added", "modified", "deleted", "renamed", "untracked"}) {
		t.Errorf("change enum = %v", change.Properties["status"].Enum)
	}
}

func TestValidArgumentsReturnStableStubError(t *testing.T) {
	ctx, client := connect(t)
	cases := []struct{ name, args string }{
		{"workspace_create", `{}`},
		{"workspace_create", `{"source":{"type":"empty"}}`},
		{"workspace_create", `{"source":{"type":"public_git","url":"https://github.com/example/repo.git"}}`},
		{"workspace_create", `{"source":{"type":"public_git","url":"https://example.com/repo.git","ref":"main"}}`},
		{"exec", `{"workspace_id":"ws_opaque","argv":["go","test","./..."]}`},
		{"exec", `{"workspace_id":"ws_opaque","argv":["bash","-lc","echo $FOO"],"cwd":"cmd/server","env":{"FOO":"bar"},"timeout_seconds":120}`},
		{"write_text", `{"workspace_id":"ws_opaque","path":"internal/foo/foo.go","content":"package foo\n日本語"}`},
		{"write_text", `{"workspace_id":"ws_opaque","path":"file","content":""}`},
		{"read_text", `{"workspace_id":"ws_opaque","path":"internal/foo/foo.go"}`},
		{"workspace_changes", `{"workspace_id":"ws_opaque"}`},
		{"workspace_changes", `{"workspace_id":"ws_opaque","base_ref":"main"}`},
		{"workspace_destroy", `{"workspace_id":"ws_opaque"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name+tc.args, func(t *testing.T) {
			result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: json.RawMessage(tc.args)})
			if err != nil {
				t.Fatal(err)
			}
			assertToolError(t, result, ErrorNotImplemented)
		})
	}
	// Arguments itself can be omitted for workspace_create.
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "workspace_create"})
	if err != nil {
		t.Fatal(err)
	}
	assertToolError(t, result, ErrorNotImplemented)
}

func TestInvalidArguments(t *testing.T) {
	ctx, client := connect(t)
	cases := []struct{ name, args string }{
		{"workspace_create", `{"source":null}`},
		{"workspace_create", `{"source":{}}`},
		{"workspace_create", `{"source":{"type":"private_git"}}`},
		{"workspace_create", `{"source":{"type":"public_git"}}`},
		{"workspace_create", `{"source":{"type":"empty","url":""}}`},
		{"workspace_create", `{"source":{"type":"empty","ref":""}}`},
		{"workspace_create", `{"source":{"type":"public_git","url":123}}`},
		{"workspace_create", `{"source":{"type":"public_git","url":"https://example.com/repo","credentials":"secret"}}`},
		{"exec", `{"workspace_id":"ws","argv":[]}`},
		{"exec", `{"workspace_id":"ws","argv":null}`},
		{"exec", `{"workspace_id":"ws","argv":[""]}`},
		{"exec", `{"workspace_id":"ws","argv":[1]}`},
		{"exec", `{"workspace_id":"ws","argv":"go test"}`},
		{"exec", `{"workspace_id":"","argv":["go"]}`},
		{"exec", `{"workspace_id":"ws"}`},
		{"exec", `{"argv":["go"]}`},
		{"exec", `{"workspace_id":"ws","argv":["go"],"timeout_seconds":0}`},
		{"exec", `{"workspace_id":"ws","argv":["go"],"timeout_seconds":-1}`},
		{"exec", `{"workspace_id":"ws","argv":["go"],"timeout_seconds":1.5}`},
		{"exec", `{"workspace_id":"ws","argv":["go"],"env":{"FOO":1}}`},
		{"exec", `{"workspace_id":"ws","argv":["go"],"cwd":null}`},
		{"write_text", `{"workspace_id":"ws","path":"file"}`},
		{"write_text", `{"workspace_id":"ws","content":"text"}`},
		{"write_text", `{"workspace_id":"ws","path":"file","content":null}`},
		{"read_text", `{"workspace_id":"ws"}`},
		{"workspace_changes", `{"workspace_id":"ws","base_ref":123}`},
		{"workspace_destroy", `{}`},
		{"workspace_destroy", `{"workspace_id":123}`},
	}
	// Reject fields that could otherwise expand the service's execution boundary.
	for _, field := range []string{"image", "mount", "device", "capability", "network_mode", "podman_options", "shell"} {
		cases = append(cases, struct{ name, args string }{"exec", `{"workspace_id":"ws","argv":["go"],"` + field + `":"unexpected"}`})
		cases = append(cases, struct{ name, args string }{"workspace_create", `{"` + field + `":"unexpected"}`})
	}
	for _, tc := range cases {
		t.Run(tc.name+tc.args, func(t *testing.T) {
			_, err := client.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: json.RawMessage(tc.args)})
			var rpcErr *jsonrpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("error = %v, want MCP invalid params", err)
			}
		})
	}
}

func assertToolError(t *testing.T, result *mcp.CallToolResult, code ErrorCode) {
	t.Helper()
	if result == nil || !result.IsError {
		t.Fatalf("result = %#v, want tool error", result)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out ErrorResult
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Error.Code != code || out.Error.Message == "" {
		t.Fatalf("error = %#v, want %s", out.Error, code)
	}
	if len(result.Content) != 1 {
		t.Fatalf("content = %#v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != string(data) {
		t.Fatalf("text content disagrees with structured error: %#v", result.Content)
	}
}

func TestDomainValidation(t *testing.T) {
	ctx, client := connect(t)
	for _, value := range []string{"", "/workspace/foo.go", "/etc/passwd", "../foo", "a/../../foo", "a\x00b"} {
		for _, name := range []string{"exec", "write_text", "read_text"} {
			t.Run(name+value, func(t *testing.T) {
				args := map[string]any{"workspace_id": "ws", "path": value}
				if name == "exec" {
					delete(args, "path")
					args["cwd"] = value
					args["argv"] = []string{"go"}
				}
				if name == "write_text" {
					args["content"] = "text"
				}
				result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
				if err != nil {
					t.Fatal(err)
				}
				assertToolError(t, result, ErrorInvalidPath)
			})
		}
	}
	for _, value := range []string{"https://user:password@example.com/repo.git", "https://token@example.com/repo.git"} {
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "workspace_create", Arguments: map[string]any{"source": map[string]any{"type": "public_git", "url": value}}})
		if err != nil {
			t.Fatal(err)
		}
		assertToolError(t, result, ErrorInvalidArgument)
	}
	for _, content := range []string{`"\ud800"`, `"\udc00"`, `"\ud800text"`} {
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "write_text", Arguments: json.RawMessage(`{"workspace_id":"ws","path":"file","content":` + content + `}`)})
		if err != nil {
			t.Fatal(err)
		}
		assertToolError(t, result, ErrorNonUTF8)
	}
}

func TestUTF8ContentIntegrity(t *testing.T) {
	for _, raw := range []string{`"plain"`, `"日本語"`, `"\ud83d\ude00"`, `"\\ud800"`, `"�"`, `"\u0000"`} {
		if !validTextToken([]byte(raw)) {
			t.Errorf("rejected valid token %s", raw)
		}
	}
	for _, raw := range []string{"\"\xff\"", `"\ud800"`, `"\udc00"`, `"\ud800\ud800"`} {
		if validTextToken([]byte(raw)) {
			t.Errorf("accepted invalid token %q", raw)
		}
	}
}

func TestDefaults(t *testing.T) {
	for _, tc := range []struct {
		schema      *jsonschema.Schema
		input, want map[string]any
	}{
		{createInputSchema(), map[string]any{}, map[string]any{"source": map[string]any{"type": "empty"}}},
		{execInputSchema(), map[string]any{"workspace_id": "ws", "argv": []any{"go"}}, map[string]any{"workspace_id": "ws", "argv": []any{"go"}, "cwd": "."}},
	} {
		resolved, err := tc.schema.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := resolved.ApplyDefaults(&tc.input); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(tc.input, tc.want) {
			t.Errorf("defaults = %#v, want %#v", tc.input, tc.want)
		}
	}
}

func TestOutputSchemas(t *testing.T) {
	tools := discover(t)
	hash := strings.Repeat("a", 64)
	cases := []struct {
		name, output string
		valid        bool
	}{
		{"workspace_create", `{"workspace_id":"ws","created_at":"2026-10-04T00:00:00Z","source":{"type":"empty"}}`, true},
		{"workspace_create", `{"workspace_id":"ws","created_at":"2026-10-04T00:00:00Z","source":{"type":"public_git","url":"https://example.com/repo"}}`, true},
		{"workspace_create", `{"workspace_id":"ws","created_at":"2026-10-04T00:00:00Z","source":{"type":"public_git","url":"https://example.com/repo","requested_ref":"main","resolved_commit_sha":"abc123"}}`, true},
		{"workspace_create", `{"workspace_id":"ws","created_at":"2026-10-04T00:00:00Z","source":{"type":"public_git"}}`, false},
		{"workspace_create", `{"workspace_id":"ws","created_at":"2026-10-04T00:00:00Z","source":{"type":"empty","requested_ref":"main"}}`, false},
		{"exec", `{"exit_code":null,"stdout":"","stderr":"","stdout_truncated":true,"stderr_truncated":false,"timed_out":true}`, true},
		{"exec", `{"exit_code":1,"stdout":"","stderr":"failed","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`, true},
		{"exec", `{"exit_code":"0","stdout":"","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`, false},
		{"exec", `{"exit_code":0,"stdout":"","stderr":"","timed_out":false}`, false},
		{"write_text", `{"path":"internal/foo.go","size_bytes":12,"sha256":"` + hash + `"}`, true},
		{"write_text", `{"path":"/workspace/foo","size_bytes":12,"sha256":"` + hash + `"}`, false},
		{"read_text", `{"path":"file","content":"text","size_bytes":4,"sha256":"` + hash + `"}`, true},
		{"read_text", `{"path":"../file","content":"text","size_bytes":4,"sha256":"` + hash + `"}`, false},
		{"read_text", `{"path":"file","content":"text","size_bytes":4,"sha256":"ABC"}`, false},
		{"workspace_changes", `{"base_ref":"abc123","changes":[]}`, true},
		{"workspace_changes", `{"base_ref":"abc123","changes":null}`, false},
		{"workspace_changes", `{"base_ref":"abc123","head_sha":"def456","changes":[{"status":"modified","path":"file"},{"status":"renamed","path":"new","old_path":"old"},{"status":"untracked","path":"test"}]}`, true},
		{"workspace_changes", `{"changes":[]}`, false},
		{"workspace_changes", `{"base_ref":"abc123","changes":[{"status":"copied","path":"file"}]}`, false},
		{"workspace_changes", `{"base_ref":"abc123","changes":[{"status":"renamed","path":"new"}]}`, false},
		{"workspace_changes", `{"base_ref":"abc123","changes":[{"status":"modified","path":"file","old_path":"old"}]}`, false},
		{"workspace_destroy", `{"workspace_id":"ws","destroyed":true}`, true},
		{"workspace_destroy", `{"workspace_id":"ws","destroyed":false}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name+tc.output, func(t *testing.T) {
			resolved, err := decodeSchema(t, tools[tc.name].OutputSchema).Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			var value any
			if err := json.Unmarshal([]byte(tc.output), &value); err != nil {
				t.Fatal(err)
			}
			if err := resolved.Validate(value); (err == nil) != tc.valid {
				t.Errorf("valid = %v, want %v: %v", err == nil, tc.valid, err)
			}
		})
	}
}
