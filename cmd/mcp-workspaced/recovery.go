package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

func validateModes(daemon, smoke bool, recovery, selection, listen string) error {
	count := 0
	if daemon {
		count++
	}
	if smoke {
		count++
	}
	if recovery != "" {
		count++
	}
	if count > 1 {
		return errors.New("choose one of -daemon, -smoke or -recover")
	}
	switch recovery {
	case "":
		if selection != "" {
			return errors.New("recovery selection requires -recover")
		}
	case "inspect", "storage":
		if selection != "" {
			return errors.New("this recovery action takes no selection")
		}
	case "backend", "cleanup":
		if len(selection) != 64 {
			return errors.New("select an exact entry from -recover inspect")
		}
	default:
		return errors.New("recover must be inspect, backend, storage or cleanup")
	}
	if recovery != "" && listen != "" {
		return errors.New("recovery has no MCP listener")
	}
	return nil
}

func runRecovery(ctx context.Context, cfg workspace.Config, action, selection string, output io.Writer) error {
	return runRecoveryWithRuntime(ctx, cfg, action, selection, output, runtimeFactory(cfg))
}

func runtimeFactory(cfg workspace.Config) func() workspace.Runtime {
	cfg.PublicDNS = append([]string(nil), cfg.PublicDNS...)
	policy := workspace.ReferencePolicy()
	if cfg.Policy != nil {
		policy = *cfg.Policy
	}
	return func() workspace.Runtime {
		return workspace.NewPodmanWithPolicy(cfg.PodmanPath, cfg.Image, policy, cfg.PublicDNS...)
	}
}

func runRecoveryWithRuntime(ctx context.Context, cfg workspace.Config, action, selection string, output io.Writer, factory func() workspace.Runtime) (err error) {
	if err := validateModes(false, false, action, selection, ""); err != nil {
		return err
	}
	if action == "" {
		return errors.New("recovery action required")
	}
	// Bounded operator command, same account-wide lock and normal reconciliation.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	service, err := workspace.Open(ctx, cfg, factory)
	if err != nil {
		return errors.New("recovery startup failed; lifecycle authority unavailable")
	}
	defer func() { err = errors.Join(err, service.Close()) }()
	switch action {
	case "inspect":
		state, err := service.RecoverySnapshot(ctx)
		if err != nil {
			return errors.New("recovery inspection unavailable")
		}
		return json.NewEncoder(output).Encode(state)
	case "backend":
		err = service.RecoverPendingBackend(ctx, selection)
	case "storage":
		err = service.RecoverStorage(ctx)
	case "cleanup":
		err = service.CleanupRecoveryGeneration(ctx, selection)
	}
	if err != nil {
		return errors.New("recovery refused or incomplete; inspect current state; authority remains fail-closed")
	}
	return json.NewEncoder(output).Encode(struct {
		Status string `json:"status"`
	}{"completed"})
}
