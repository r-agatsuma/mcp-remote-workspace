// mcp-workspaced supplies a local MCP smoke check and host lifecycle skeleton.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

const serverName = "mcp-workspaced"
const serverVersion = "0.0.0-dev"

func main() {
	smoke := flag.Bool("smoke", false, "check an in-memory MCP connection and exit (5-second deadline)")
	daemon := flag.Bool("daemon", false, "own and reconcile local lifecycle state until shutdown (no listener)")
	stateDir := flag.String("state-dir", "", "private absolute lifecycle journal directory")
	image := flag.String("image", "", "canonical locally installed image pinned with @sha256:digest")
	podmanPath := flag.String("podman", "/usr/bin/podman", "absolute host-side rootless Podman executable")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "mcp-workspaced: unexpected positional arguments")
		os.Exit(2)
	}
	if *daemon && *smoke {
		fmt.Fprintln(os.Stderr, "mcp-workspaced: choose -daemon or -smoke")
		os.Exit(2)
	}
	if *daemon {
		if *stateDir == "" {
			var err error
			*stateDir, err = workspace.DefaultStateDir()
			if err != nil {
				fmt.Fprintln(os.Stderr, "mcp-workspaced:", err)
				os.Exit(1)
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := runDaemon(ctx, workspace.Config{StateDir: *stateDir, Image: *image, PodmanPath: *podmanPath}); err != nil {
			fmt.Fprintln(os.Stderr, "mcp-workspaced:", err)
			os.Exit(1)
		}
		return
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
