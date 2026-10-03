package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/mcpserver"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	backend, err := workspace.NewPodman(ctx)
	if err == nil {
		err = mcpserver.New(backend).Run(ctx, &mcp.StdioTransport{})
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
