package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSmokeMCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	protocol, err := smokeMCP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if protocol != "2025-11-25" {
		t.Fatalf("unexpected smoke protocol: %s", protocol)
	}
}

// Exercise HTTP, protocol negotiation, schema inference and tool dispatch using
// a test-only echo tool. Production composition is covered in ingress_test.go.
func TestSDKStreamableHTTP(t *testing.T) {
	for _, protocol := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(protocol, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server := newServer()
			type input struct {
				Message string `json:"message"`
			}
			mcp.AddTool(server, &mcp.Tool{Name: "test_echo", Description: "SDK suitability probe"},
				func(_ context.Context, _ *mcp.CallToolRequest, args input) (*mcp.CallToolResult, any, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: args.Message}}}, nil, nil
				})
			httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(
				func(*http.Request) *mcp.Server { return server },
				&mcp.StreamableHTTPOptions{Stateless: true},
			))
			defer httpServer.Close()
			client := mcp.NewClient(&mcp.Implementation{Name: "sdk-suitability-test", Version: serverVersion}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL},
				&mcp.ClientSessionOptions{ProtocolVersion: protocol})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			}()
			info := session.InitializeResult()
			if info.ProtocolVersion != protocol || info.ServerInfo.Name != serverName || info.Capabilities.Tools == nil {
				t.Fatalf("unexpected handshake: %+v", info)
			}
			if protocol == "2025-11-25" {
				if err := session.Ping(ctx, nil); err != nil {
					t.Fatal(err)
				}
			}
			tools, err := session.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(tools.Tools) != 1 || tools.Tools[0].Name != "test_echo" || tools.Tools[0].InputSchema == nil {
				t.Fatalf("unexpected tool discovery: %+v", tools)
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name: "test_echo", Arguments: map[string]any{"message": "SDK round trip"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError || len(result.Content) != 1 {
				t.Fatalf("unexpected tool result: %+v", result)
			}
			content, ok := result.Content[0].(*mcp.TextContent)
			if !ok || content.Text != "SDK round trip" {
				t.Fatalf("unexpected content: %+v", result.Content)
			}
		})
	}
}
