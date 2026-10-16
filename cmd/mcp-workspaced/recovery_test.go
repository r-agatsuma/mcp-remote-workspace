//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

func TestRecoveryModeExclusivityAndNoBlindCompletion(t *testing.T) {
	for _, action := range []string{"inspect", "storage", "backend", "cleanup"} {
		selection := ""
		if action == "backend" || action == "cleanup" {
			selection = strings.Repeat("a", 64)
		}
		if err := validateModes(false, false, action, selection, ""); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"daemon", "smoke", "listener"} {
			listen := ""
			if mode == "listener" {
				listen = "127.0.0.1:8080"
			}
			if err := validateModes(mode == "daemon", mode == "smoke", action, selection, listen); err == nil {
				t.Fatal("concurrent authority permitted", action, mode)
			}
		}
	}
	for _, action := range []string{"completed", "mark-done", "trust-me", "mark-complete"} {
		if err := validateModes(false, false, action, strings.Repeat("a", 64), ""); err == nil {
			t.Fatal("blind recovery mode", action)
		}
	}
	if err := validateModes(false, false, "backend", "", ""); err == nil {
		t.Fatal("missing selection accepted")
	}
	if err := validateModes(false, false, "storage", "unexpected", ""); err == nil {
		t.Fatal("storage used backend selection")
	}
}

func TestOperatorRecoveryUsesDaemonSingletonAndNormalState(t *testing.T) {
	cfg := ingressConfig(t)
	runtime := &ingressRuntime{objects: make(map[workspace.Exact]workspace.Object)}
	factory := func() workspace.Runtime { return runtime }
	s, err := workspace.Open(context.Background(), cfg, factory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	i, err := workspace.IdentityFromTrustedMeta(map[string]any{"openai/subject": "private-human-identity", "openai/session": "private-session"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.BeginBackend(e, "test-pending")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err := runRecoveryWithRuntime(context.Background(), cfg, "inspect", "", io.Discard, func() workspace.Runtime { called = true; return runtime }); err == nil || called {
		t.Fatal("second authority reached runtime", err)
	}
	s.Close()
	var output bytes.Buffer
	if err := runRecoveryWithRuntime(context.Background(), cfg, "inspect", "", &output, factory); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "private-human-identity") || strings.Contains(output.String(), "private-session") {
		t.Fatal("identity exposed")
	}
	var state workspace.RecoveryState
	if err := json.Unmarshal(output.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Operations) != 1 || state.Operations[0].Operation != op || state.Operations[0].Evidence != "current_boot_unknown" {
		t.Fatal(state)
	}
	if err := runRecoveryWithRuntime(context.Background(), cfg, "backend", state.Operations[0].Selection, io.Discard, factory); err == nil {
		t.Fatal("operator selection became evidence")
	}
	if err := runRecoveryWithRuntime(context.Background(), cfg, "cleanup", state.Generations[0].Selection, io.Discard, factory); err == nil {
		t.Fatal("cleanup bypassed pending work")
	}
	r, err := workspace.Open(context.Background(), cfg, factory)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.PendingBackend()) != 1 || len(runtime.created) != 1 || len(runtime.objects) != 1 {
		t.Fatal("recovery replayed or lost state")
	}
}

type recoveryStorage func() (workspace.StorageSample, error)

func (f recoveryStorage) Sample(context.Context, string) (workspace.StorageSample, error) { return f() }
func TestOperatorStorageModeDoesNotAutomaticallyClearLatch(t *testing.T) {
	cfg := ingressConfig(t)
	sample := workspace.StorageSample{}
	cfg.Storage = recoveryStorage(func() (workspace.StorageSample, error) { return sample, nil })
	runtime := &ingressRuntime{objects: make(map[workspace.Exact]workspace.Object)}
	factory := func() workspace.Runtime { return runtime }
	s, err := workspace.Open(context.Background(), cfg, factory)
	if err != nil {
		t.Fatal(err)
	}
	i, _ := workspace.IdentityFromTrustedMeta(map[string]any{"openai/subject": "s", "openai/session": "c"})
	if _, err := s.Create(context.Background(), i); err == nil {
		t.Fatal("unsafe storage admitted")
	}
	s.Close()
	if err := runRecoveryWithRuntime(context.Background(), cfg, "storage", "", io.Discard, factory); err == nil {
		t.Fatal("unsafe sample recovered")
	}
	sample = workspace.StorageSample{AvailableBytes: 1 << 40, FreeInodes: 1 << 30}
	var out bytes.Buffer
	if err := runRecoveryWithRuntime(context.Background(), cfg, "inspect", "", &out, factory); err != nil {
		t.Fatal(err)
	}
	var snapshot workspace.RecoveryState
	if err := json.Unmarshal(out.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.StorageBlocked {
		t.Fatal("inspection/restart cleared latch")
	}
	if err := runRecoveryWithRuntime(context.Background(), cfg, "storage", "", io.Discard, factory); err != nil {
		t.Fatal(err)
	}
	s, err = workspace.Open(context.Background(), cfg, factory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Create(context.Background(), i); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryIsNotAnMCPTool(t *testing.T) {
	_, _, _, endpoint := serveIngress(t)
	client := connectIngress(t, endpoint, "2025-11-25", nil)
	for _, name := range []string{"recover", "recover_backend", "recover_storage"} {
		result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}, Meta: meta("s", "c")})
		if err == nil && (result == nil || !result.IsError) {
			t.Fatal("MCP recovery reachable", name)
		}
	}
}
