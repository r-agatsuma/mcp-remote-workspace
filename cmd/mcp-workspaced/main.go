package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/mcpserver"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

var maxTextFileBytes = flag.Int64("max-text-file-bytes", workspace.DefaultMaxTextFileBytes, "maximum read_text file size in bytes")

func main() {
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	if *maxTextFileBytes <= 0 {
		return errors.New("max-text-file-bytes must be positive")
	}
	backend, err := workspace.OpenWithOptions(ctx, workspace.Options{MaxTextFileBytes: *maxTextFileBytes})
	if err != nil {
		return err
	}
	return serve(ctx, backend)
}

func serve(ctx context.Context, backend mcpserver.Lifecycle) error {
	err := mcpserver.New(backend).Run(ctx, &mcp.StdioTransport{})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
