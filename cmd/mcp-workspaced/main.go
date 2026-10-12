// mcp-workspaced is currently a local MCP SDK smoke/build skeleton.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const serverName = "mcp-workspaced"
const serverVersion = "0.0.0-dev"

func main() {
	smoke := flag.Bool("smoke", false, "check an in-memory MCP connection and exit (5-second deadline)")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "mcp-workspaced: unexpected positional arguments")
		os.Exit(2)
	}
	if !*smoke {
		flag.Usage()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Bound the entire smoke operation, including SDK session teardown.
	type result struct {
		protocol string
		err      error
	}
	done := make(chan result, 1)
	go func() {
		protocol, err := smokeMCP(ctx)
		done <- result{protocol, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			fmt.Fprintln(os.Stderr, "mcp-workspaced:", r.err)
			os.Exit(1)
		}
		fmt.Printf("MCP smoke OK: server=%s protocol=%s tools=0\n", serverName, r.protocol)
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "mcp-workspaced:", ctx.Err())
		os.Exit(1)
	}
}

func newServer() *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
}

func smokeMCP(ctx context.Context) (protocol string, err error) {
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := newServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		return "", fmt.Errorf("connect server: %w", err)
	}
	defer func() { err = errors.Join(err, serverSession.Close()) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-workspaced-smoke", Version: serverVersion}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{},
	})
	// Ping belongs to the legacy protocol; it was removed in 2026-07-28.
	clientSession, err := client.Connect(ctx, clientTransport, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		return "", fmt.Errorf("connect client: %w", err)
	}
	defer func() { err = errors.Join(err, clientSession.Close()) }()

	info := clientSession.InitializeResult()
	if info == nil || info.ProtocolVersion == "" || info.ServerInfo == nil ||
		info.ServerInfo.Name != serverName || info.ServerInfo.Version != serverVersion ||
		info.Capabilities == nil || info.Capabilities.Tools == nil {
		return "", errors.New("unexpected MCP server identity or capabilities")
	}
	if err := clientSession.Ping(ctx, nil); err != nil {
		return "", fmt.Errorf("ping: %w", err)
	}
	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("list tools: %w", err)
	}
	if len(tools.Tools) != 0 || tools.NextCursor != "" {
		return "", errors.New("skeleton must expose an empty tool list")
	}
	return info.ProtocolVersion, nil
}
