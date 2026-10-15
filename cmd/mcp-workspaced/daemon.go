package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

// runDaemon owns the single lifecycle Service and its loopback ingress.
func runDaemon(ctx context.Context, cfg workspace.Config, endpoint string) error {
	cfg.PublicDNS = append([]string(nil), cfg.PublicDNS...)
	return runDaemonWithRuntime(ctx, cfg, endpoint, func() workspace.Runtime {
		policy := workspace.ReferencePolicy()
		if cfg.Policy != nil {
			policy = *cfg.Policy
		}
		return workspace.NewPodmanWithPolicy(cfg.PodmanPath, cfg.Image, policy, cfg.PublicDNS...)
	})
}

func runDaemonWithRuntime(ctx context.Context, cfg workspace.Config, endpoint string, factory func() workspace.Runtime) (err error) {
	if err := validateListen(endpoint); err != nil {
		return err
	}
	ctx, cancelDaemon := context.WithCancel(ctx)
	defer cancelDaemon()
	service, err := workspace.Open(ctx, cfg, factory)
	if err != nil {
		return err
	}
	var ingress *ingress
	var ttlDone <-chan error
	// Close preserves Service/singleton ownership. A stuck trusted adapter never
	// causes an early unlock: after the finite shutdown budget main exits nonzero,
	// letting process exit release the OS lock; restart reconciliation owns residue.
	defer func() {
		cancelDaemon()
		if ingress != nil {
			ingress.stopping.Store(true)
		}
		deadline, cancel := context.WithTimeout(context.Background(), httpShutdownLifetime)
		defer cancel()
		finished := make(chan error, 1)
		go func() {
			var shutdownErr error
			if ingress != nil {
				shutdownErr = ingress.shutdown(deadline)
			}
			if ttlDone != nil {
				<-ttlDone
			}
			finished <- errors.Join(shutdownErr, service.Close())
		}()
		select {
		case closeErr := <-finished:
			err = errors.Join(err, closeErr)
		case <-deadline.Done():
			err = errors.Join(err, errors.New("daemon shutdown deadline exceeded; process exit required"))
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	ingress, err = newIngress(ctx, service)
	if err != nil {
		return err
	}
	stopAdmission := context.AfterFunc(ctx, func() { ingress.stopping.Store(true) })
	defer stopAdmission()
	listener, err := net.Listen("tcp", endpoint)
	if err != nil {
		return errors.New("cannot bind configured MCP loopback listener")
	}
	served := make(chan error, 1)
	go func() { served <- ingress.server.Serve(listener) }()
	fmt.Println("mcp-workspaced: lifecycle reconciled; loopback MCP admission ready")
	ttlResult := make(chan error, 1)
	ttlDone = ttlResult
	go func() { ttlResult <- runTTL(ctx, cfg, service); close(ttlResult) }()
	select {
	case <-ctx.Done():
		return nil
	case serveErr := <-served:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return errors.New("MCP HTTP server stopped unexpectedly")
	case ttlErr := <-ttlResult:
		return ttlErr
	}
}

// TTL shares the same Service. Its waits cannot delay the start of the daemon's
// shutdown budget, and shutdown waits for it before releasing singleton ownership.
func runTTL(ctx context.Context, cfg workspace.Config, service *workspace.Service) error {
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
