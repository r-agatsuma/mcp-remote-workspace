//go:build linux

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPublicToolsDiscoveryDispatchAndBoundedErrors(t *testing.T) {
	s, f, i, e := openTools(t, Config{})
	f.start = func(_ context.Context, _ ProcessSnapshot, r ExecRequest, out, diagnostic io.Writer) (<-chan ExecOutcome, error) {
		switch r.Argv[0] {
		case "echo":
			io.WriteString(out, "separate stdout")
			io.WriteString(diagnostic, "separate stderr")
		case "/bin/sh":
			if r.Argv[3] == "read_text" {
				io.WriteString(out, "UTF-8 text")
			}
		}
		return terminal(ExecOutcome{Completion: DefinitelyCompleted}), nil
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "tool-test", Version: "0"}, nil)
	admit := func(context.Context, *mcp.CallToolRequest) (Identity, Exact, error) { return i, e, nil }
	if err := RegisterTools(server, s, admit); err != nil {
		t.Fatal(err)
	}
	if err := RegisterTools(server, s, nil); err == nil {
		t.Fatal("missing trusted ingress accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "tool-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 3 {
		t.Fatal(list, err)
	}
	names := map[string]bool{}
	for _, tool := range list.Tools {
		names[tool.Name] = true
		if tool.InputSchema == nil {
			t.Fatal("missing schema")
		}
	}
	if !names["exec"] || !names["read_text"] || !names["write_text"] {
		t.Fatal(names)
	}
	for _, call := range []struct {
		name string
		args any
		want string
	}{
		{"exec", map[string]any{"argv": []string{"echo", "$HOME;literal"}}, `"exit_status":0`},
		{"exec", map[string]any{"argv": []string{"sleep", "1"}, "detach": true}, `{"status":"started"}`},
		{"read_text", map[string]any{"path": "/f"}, `{"text":"UTF-8 text"}`},
		{"write_text", map[string]any{"path": "/f", "content": "new"}, `{"status":"written"}`},
	} {
		result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil || result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
			t.Fatal(result, err)
		}
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok || !strings.Contains(text.Text, call.want) {
			t.Fatal(result.Content)
		}
	}
	before := f.calls
	for _, call := range []struct {
		name string
		args any
	}{
		{"exec", map[string]any{"argv": []string{}}},
		{"exec", map[string]any{"argv": []any{nil}}},
		{"exec", map[string]any{"argv": []string{"x"}, "env": map[string]any{"A": nil}}},
		{"exec", map[string]any{"argv": []string{"x"}, "cwd": ""}},
		{"exec", map[string]any{"argv": []string{"x"}, "generation": "caller authority"}},
		{"exec", map[string]any{"argv": []string{"x"}, "ARGV": []any{nil}}},
		{"exec", map[string]any{"argv": []string{strings.Repeat("private", MaxExecPayload)}}},
		{"read_text", map[string]any{"path": "../private"}},
		{"write_text", map[string]any{"path": "/f"}},
		{"write_text", map[string]any{"path": "/f", "content": nil}},
	} {
		result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil || !result.IsError || len(result.Content) != 1 {
			t.Fatal(result, err)
		}
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok || len(text.Text) > 300 || strings.Contains(text.Text, "private") {
			t.Fatal("unbounded/reflected public error", result.Content)
		}
	}
	if f.calls != before {
		t.Fatal("invalid public request submitted")
	}
	// The host, not unverified caller _meta, supplies identity and exact state.
	private := strings.Repeat("secret admission detail", 1<<17)
	if err := RegisterTools(server, s, func(context.Context, *mcp.CallToolRequest) (Identity, Exact, error) {
		return Identity{}, Exact{}, errors.New(private)
	}); err != nil {
		t.Fatal(err)
	}
	result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "exec", Arguments: map[string]any{"argv": []string{"true"}}})
	if err != nil || !result.IsError || len(result.Content[0].(*mcp.TextContent).Text) > 300 {
		t.Fatal(result, err)
	}
	if f.calls != before {
		t.Fatal("unadmitted public operation submitted")
	}
}

func TestDecodeToolRequestUTF8AndNullContract(t *testing.T) {
	for _, raw := range []string{
		`{"path":"/f","content":"\ud800"}`, `{"path":"/f","content":"\udc00"}`,
		`{"path":"/f","content":"\ud800x"}`, `{"path":"/f","content":"\ud800\u0041"}`,
		"{\"path\":\"/f\",\"content\":\"\xff\"}",
		`{"path":"/f","content":null}`, `{"path":"/f"}`, `null`, `[]`,
		`{"path":"/f","content":12}`, `{"path":"/f","content":"x"} {}`,
	} {
		var r WriteTextRequest
		if decodeToolRequest(json.RawMessage(raw), &r, "path", "content") == nil {
			t.Fatalf("invalid JSON text accepted: %s", raw)
		}
	}
	for _, raw := range []string{`{"path":"/f","content":"\ud83d\ude42"}`, `{"path":"/f","content":"\\ud800"}`, `{"path":"/f","content":"\u0000"}`} {
		var r WriteTextRequest
		if err := decodeToolRequest(json.RawMessage(raw), &r, "path", "content"); err != nil {
			t.Fatal(err)
		}
	}
	var r ExecRequest
	if err := decodeToolRequest(json.RawMessage(`{"argv":["x"],"env":{"A":null}}`), &r, "argv"); err == nil {
		t.Fatal("null environment value accepted")
	}
}
