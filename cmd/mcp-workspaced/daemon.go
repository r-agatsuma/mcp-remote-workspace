package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

// runDaemon owns lifecycle state until shutdown. This skeleton intentionally
// supplies no network admission: trusted ingress comes later.
func runDaemon(ctx context.Context, cfg workspace.Config) (err error) {
	service, err := workspace.Open(ctx, cfg, func() workspace.Runtime {
		policy := workspace.ReferencePolicy()
		if cfg.Policy != nil {
			policy = *cfg.Policy
		}
		return workspace.NewPodmanWithPolicy(cfg.PodmanPath, cfg.Image, policy)
	})
	if err != nil {
		return err
	}
	fmt.Println("mcp-workspaced: lifecycle reconciliation complete")
	defer func() { err = errors.Join(err, service.Close()) }()
	interval := time.Minute
	if cfg.Policy != nil && cfg.Policy.IdleTTL/2 < interval {
		interval = max(cfg.Policy.IdleTTL/2, time.Nanosecond)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			for _, e := range service.ExpiredGenerations() {
				if err := service.CleanupExpired(ctx, e); err != nil {
					if errors.Is(err, workspace.ErrNotExpired) || errors.Is(err, workspace.ErrStaleGeneration) {
						continue
					}
					if ctx.Err() != nil {
						return nil
					}
					return fmt.Errorf("TTL cleanup admission: %w", err)
				}
			}
		}
	}
}
