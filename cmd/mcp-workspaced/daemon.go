package main

import (
	"context"
	"fmt"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

// runDaemon owns lifecycle state until shutdown. This skeleton intentionally
// supplies no network admission: trusted ingress and full admission come later.
func runDaemon(ctx context.Context, cfg workspace.Config) error {
	service, err := workspace.Open(ctx, cfg, func() workspace.Runtime {
		return workspace.NewPodman(cfg.PodmanPath, cfg.Image)
	})
	if err != nil {
		return err
	}
	fmt.Println("mcp-workspaced: lifecycle reconciliation complete")
	<-ctx.Done()
	return service.Close()
}
