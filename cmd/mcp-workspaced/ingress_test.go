//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

// In-memory runtime follows the existing strict production object namespace.
// No executable, Podman, OCI, systemd, or Tunnel is used by these tests.
type ingressRuntime struct {
	mu         sync.Mutex
	objects    map[workspace.Exact]workspace.Object
	created    []workspace.Exact
	executed   []workspace.Exact
	initialize func(context.Context) error
	build      func(context.Context) (string, error)
	finalize   func(context.Context) error
	start      func(context.Context) (<-chan workspace.ExecOutcome, error)
}

func (f *ingressRuntime) Initialize(ctx context.Context) error {
	if f.initialize != nil {
		return f.initialize(ctx)
	}
	return nil
}
func (f *ingressRuntime) List(context.Context) ([]workspace.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var objects []workspace.Object
	for _, o := range f.objects {
		objects = append(objects, o)
	}
	return objects, nil
}
func (f *ingressRuntime) Create(ctx context.Context, e workspace.Exact, image string) error {
	f.mu.Lock()
	finalize := f.finalize
	f.mu.Unlock()
	if strings.HasPrefix(image, "sha256:") && finalize != nil {
		if err := finalize(ctx); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, e)
	f.objects[e] = workspace.Object{ID: string(e.Generation), Name: "mcpws-v1-" + string(e.Workspace) + "-" + string(e.Generation), Image: image,
		Labels: map[string]string{"io.mcp-workspaced.owner": "mcp-workspaced-v1", "io.mcp-workspaced.workspace": string(e.Workspace), "io.mcp-workspaced.generation": string(e.Generation), "io.mcp-workspaced.image": image}}
	return nil
}
func (f *ingressRuntime) Destroy(_ context.Context, e workspace.Exact) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, e)
	return nil
}
func (f *ingressRuntime) ProbeProcess(_ context.Context, e workspace.Exact) (workspace.ProcessSnapshot, error) {
	p := workspace.ReferencePolicy()
	return workspace.ProcessSnapshot{Exact: e, ObjectID: string(e.Generation), PID: 42, PIDStart: 1, CgroupPath: "/fixture", CgroupID: 1, Tasks: 1,
		MemoryBytes: p.MemoryBytes, SwapBytes: p.SwapBytes, CPUQuota: p.CPUQuota, CPUWeight: p.CPUWeight, PIDsMax: p.PIDsMax, NoFileSoft: p.NoFile, NoFileHard: p.NoFile}, nil
}
func (f *ingressRuntime) StartExec(ctx context.Context, p workspace.ProcessSnapshot, r workspace.ExecRequest, out, _ io.Writer) (<-chan workspace.ExecOutcome, error) {
	f.mu.Lock()
	f.executed = append(f.executed, p.Exact)
	start := f.start
	f.mu.Unlock()
	if start != nil {
		return start(ctx)
	}
	if len(r.Argv) > 3 && r.Argv[3] == "read_text" {
		_, _ = io.WriteString(out, "fixture text")
	}
	done := make(chan workspace.ExecOutcome, 1)
	done <- workspace.ExecOutcome{Completion: workspace.DefinitelyCompleted}
	close(done)
	return done, nil
}

func (f *ingressRuntime) Build(ctx context.Context, _ workspace.Exact, _, _ string) (string, error) {
	f.mu.Lock()
	build := f.build
	f.mu.Unlock()
	if build == nil {
		return "", workspace.CompletedBackendError(errors.New("no fixture build"))
	}
	return build(ctx)
}
func (f *ingressRuntime) CleanupBuild(context.Context, workspace.Exact, string, string) error {
	return nil
}

type ingressStorage struct{}

func (ingressStorage) Sample(context.Context, string) (workspace.StorageSample, error) {
	return workspace.StorageSample{AvailableBytes: 1 << 40, FreeInodes: 1 << 30}, nil
}
func ingressConfig(t *testing.T) workspace.Config {
	t.Helper()
	p := workspace.ReferencePolicy()
	p.MaxGenerations = 8
	return workspace.Config{StateDir: filepath.Join(t.TempDir(), "state"), Image: "example.invalid/test@sha256:" + strings.Repeat("a", 64), PodmanPath: "/unused/podman", Storage: ingressStorage{}, Policy: &p}
}
func serveIngress(t *testing.T) (*ingress, *workspace.Service, *ingressRuntime, string) {
	t.Helper()
	f := &ingressRuntime{objects: make(map[workspace.Exact]workspace.Object)}
	service, err := workspace.Open(context.Background(), ingressConfig(t), func() workspace.Runtime { return f })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	g, err := newIngress(context.Background(), service)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- g.server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = g.shutdown(ctx)
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	return g, service, f, "http://" + listener.Addr().String() + "/mcp"
}
func connectIngress(t *testing.T, endpoint, protocol string, client *http.Client) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "ingress-test", Version: "0"}, nil)
	session, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client}, &mcp.ClientSessionOptions{ProtocolVersion: protocol})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}
func meta(subject, session string) mcp.Meta {
	return mcp.Meta{"openai/subject": subject, "openai/session": session}
}
func callIngress(t *testing.T, c *mcp.ClientSession, name string, args any, metadata mcp.Meta) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := c.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args, Meta: metadata})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestProductionIngressComposition(t *testing.T) {
	for _, protocol := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(protocol, func(t *testing.T) {
			_, _, f, url := serveIngress(t)
			c := connectIngress(t, url, protocol, nil)
			list, err := c.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tool := range list.Tools {
				names = append(names, tool.Name)
			}
			slices.Sort(names)
			if !slices.Equal(names, []string{"exec", "read_text", "workspace_create", "workspace_destroy", "write_text"}) {
				t.Fatal(names)
			}
			pair := meta("private-subject", "private-session")
			for _, name := range []string{"exec", "read_text", "write_text"} {
				args := map[string]any{"argv": []string{"true"}}
				if name != "exec" {
					args = map[string]any{"path": "/f"}
					if name == "write_text" {
						args["content"] = "text"
					}
				}
				if !callIngress(t, c, name, args, pair).IsError {
					t.Fatal("absent operation succeeded")
				}
			}
			f.mu.Lock()
			n := len(f.created)
			f.mu.Unlock()
			if n != 0 {
				t.Fatal("implicit create")
			}
			for _, pair := range []mcp.Meta{pair, pair, meta("other-subject", "private-session"), meta("private-subject", "other-session")} {
				result := callIngress(t, c, "workspace_create", map[string]any{}, pair)
				if result.IsError {
					t.Fatal(result)
				}
				raw, _ := json.Marshal(result)
				if bytes.Contains(raw, []byte("private-")) {
					t.Fatal("identity leaked")
				}
			}
			f.mu.Lock()
			created := slices.Clone(f.created)
			f.mu.Unlock()
			if len(created) != 3 || created[0].Workspace == created[1].Workspace || created[0].Workspace == created[2].Workspace || created[1].Workspace == created[2].Workspace {
				t.Fatal(created)
			}
			expected, err := workspace.IdentityFromTrustedMeta(pair)
			if err != nil || created[0].Workspace != expected.Workspace() {
				t.Fatal("wrong identity", err)
			}
			for _, call := range []struct {
				name string
				args any
			}{{"exec", map[string]any{"argv": []string{"true"}}}, {"read_text", map[string]any{"path": "/f"}}, {"write_text", map[string]any{"path": "/f", "content": "text"}}} {
				if result := callIngress(t, c, call.name, call.args, pair); result.IsError {
					t.Fatal(result)
				}
			}
			f.mu.Lock()
			executed := slices.Clone(f.executed)
			f.mu.Unlock()
			if len(executed) != 3 {
				t.Fatal(executed)
			}
			for _, e := range executed {
				if e != created[0] {
					t.Fatal("wrong exact target", e)
				}
			}
			if callIngress(t, c, "workspace_destroy", map[string]any{}, pair).IsError {
				t.Fatal("destroy failed")
			}
		})
	}
}

func postIngress(t *testing.T, url, body string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(raw)
}
func wireCall(metadata, args string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workspace_create","arguments":` + args + `,"_meta":` + metadata + `}}`
}
func TestIngressIdentityFailClosed(t *testing.T) {
	_, _, f, url := serveIngress(t)
	for _, metadata := range []string{`{}`, `{"openai/subject":"secret-marker"}`, `{"openai/session":"secret-marker"}`, `{"openai/subject":42,"openai/session":"secret-marker"}`, `{"openai/subject":"secret-marker","openai/session":false}`, `{"openai/subject":"","openai/session":"secret-marker"}`, `{"openai/subject":"secret-marker","openai/session":""}`, `{"openai/subject":null,"openai/session":"secret-marker"}`, `"secret-marker"`, `[]`, `null`, `{"openai/subject":"secret-marker","openai/session":"\ud800"}`, `{"openai/subject":"secret-marker","openai/subject":"other","openai/session":"x"}`} {
		status, body := postIngress(t, url, wireCall(metadata, `{}`), nil)
		if status != http.StatusBadRequest || strings.Contains(body, "secret-marker") {
			t.Fatal(status, body)
		}
	}
	for _, field := range []string{"openai/subject", "openai/session"} {
		m := meta("secret-marker", "secret-marker")
		m[field] = strings.Repeat("z", maxIdentityBytes+1)
		raw, _ := json.Marshal(m)
		status, body := postIngress(t, url, wireCall(string(raw), `{}`), nil)
		if status != 400 || strings.Contains(body, "secret-marker") {
			t.Fatal(status, body)
		}
	}
	for _, body := range []string{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workspace_create","arguments":{}}}`, `{"method":"tools/call","params":`, "\xff", `[]`} {
		status, _ := postIngress(t, url, body, nil)
		if status != 400 {
			t.Fatal(status)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.created) != 0 {
		t.Fatal("invalid identity mutated runtime")
	}
}

func TestIngressNonAuthoritativeInputs(t *testing.T) {
	_, _, f, url := serveIngress(t)
	headers := map[string]string{"X-OpenAI-Subject": "header-subject", "X-OpenAI-Session": "header-session", "X-Workspace-Id": "chosen", "Mcp-Session-Id": "chosen"}
	status, _ := postIngress(t, url, wireCall(`{}`, `{}`), headers)
	if status != 400 {
		t.Fatal("headers supplied missing identity", status)
	}
	pair := `{"openai/subject":"actual-subject","openai/session":"actual-session"}`
	for _, key := range []string{"workspace_id", "user_id", "session_id", "generation", "_meta"} {
		args, _ := json.Marshal(map[string]any{key: "chosen"})
		status, body := postIngress(t, url, wireCall(pair, string(args)), headers)
		if status != 200 || !strings.Contains(body, `"isError":true`) {
			t.Fatal(status, body)
		}
	}
	for _, transportID := range []string{"first", "second", ""} {
		headers["Mcp-Session-Id"] = transportID
		status, body := postIngress(t, url, wireCall(pair, `{}`), headers)
		if status != 200 || strings.Contains(body, `"isError":true`) || strings.Contains(body, "actual-") {
			t.Fatal(status, body)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	expected, _ := workspace.IdentityFromTrustedMeta(meta("actual-subject", "actual-session"))
	if len(f.created) != 1 || f.created[0].Workspace != expected.Workspace() {
		t.Fatal("non-authoritative identity override", f.created)
	}
}

func TestIngressBindAndResourceBounds(t *testing.T) {
	for _, endpoint := range []string{"", "localhost:8080", ":8080", "0.0.0.0:8080", "[::]:8080", "192.0.2.1:8080", "127.0.0.1", "http://127.0.0.1:8080", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:-1", "127.0.0.1:08080", "[::1%lo]:8080"} {
		if validateListen(endpoint) == nil {
			t.Fatal("accepted invalid endpoint", endpoint)
		}
	}
	for _, endpoint := range []string{"127.0.0.1:8080", "[::1]:8080"} {
		if err := validateListen(endpoint); err != nil {
			t.Fatal(err)
		}
	}
	g, _, _, url := serveIngress(t)
	if g.server.ReadHeaderTimeout != httpHeaderLifetime || g.server.ReadTimeout != httpRequestLifetime || g.server.WriteTimeout <= 0 || g.server.IdleTimeout <= 0 || g.server.MaxHeaderBytes != maxHTTPHeaders {
		t.Fatal("unbounded HTTP server")
	}
	status, _ := postIngress(t, url, strings.Repeat(" ", maxHTTPBody+1), nil)
	if status != 413 {
		t.Fatal(status)
	}
	pair := `{"openai/subject":"s","openai/session":"c"}`
	for _, headers := range []map[string]string{{"Host": "attacker.invalid:8080"}, {"Origin": "http://attacker.invalid"}, {"Sec-Fetch-Site": "same-origin"}} {
		status, body := postIngress(t, url, wireCall(pair, `{}`), headers)
		if status != 403 || strings.Contains(body, "attacker") {
			t.Fatal(status, body)
		}
	}
	for n := 0; n < maxHTTPRequests; n++ {
		g.slots <- struct{}{}
	}
	status, _ = postIngress(t, url, wireCall(pair, `{}`), nil)
	if status != 503 {
		t.Fatal(status)
	}
	for n := 0; n < maxHTTPRequests; n++ {
		<-g.slots
	}
	status, _ = postIngress(t, url, "["+wireCall(pair, `{}`)+","+wireCall(pair, `{}`)+"]", nil)
	if status != 400 {
		t.Fatal("batch admitted", status)
	}
	g.stopping.Store(true)
	status, _ = postIngress(t, url, wireCall(pair, `{}`), nil)
	if status != 503 {
		t.Fatal("shutdown admitted request", status)
	}
}

func TestIngressCancellationNoReplay(t *testing.T) {
	for _, protocol := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(protocol, func(t *testing.T) {
			_, service, f, url := serveIngress(t)
			c := connectIngress(t, url, protocol, nil)
			pair := meta("cancel-subject", "cancel-session")
			if callIngress(t, c, "workspace_create", map[string]any{}, pair).IsError {
				t.Fatal("create")
			}
			started := make(chan struct{})
			canceled := make(chan struct{})
			terminal := make(chan workspace.ExecOutcome, 1)
			f.mu.Lock()
			f.start = func(ctx context.Context) (<-chan workspace.ExecOutcome, error) {
				close(started)
				go func() { <-ctx.Done(); close(canceled) }()
				return terminal, nil
			}
			f.mu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			returned := make(chan struct{})
			go func() {
				_, _ = c.CallTool(ctx, &mcp.CallToolParams{Name: "exec", Arguments: map[string]any{"argv": []string{"work"}}, Meta: pair})
				close(returned)
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("not started")
			}
			cancel()
			select {
			case <-returned:
			case <-time.After(3 * time.Second):
				t.Fatal("caller not bounded")
			}
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("HTTP cancellation not propagated")
			}
			if len(service.PendingBackend()) != 1 {
				t.Fatal("cancellation incorrectly proved terminality")
			}
			if !callIngress(t, c, "exec", map[string]any{"argv": []string{"work"}}, pair).IsError {
				t.Fatal("uncertain generation accepted again")
			}
			f.mu.Lock()
			calls := len(f.executed)
			f.mu.Unlock()
			if calls != 1 {
				t.Fatal("replay", calls)
			}
			terminal <- workspace.ExecOutcome{Completion: workspace.DefinitelyCompleted}
			close(terminal)
			// Wait for the inherited asynchronous ledger update, with a finite deadline.
			deadline := time.Now().Add(time.Second)
			for len(service.PendingBackend()) != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if len(service.PendingBackend()) != 0 {
				t.Fatal("late exact completion not accounted")
			}
		})
	}
}

func TestIngressShutdownCancelsAndDrains(t *testing.T) {
	g, _, f, url := serveIngress(t)
	c := connectIngress(t, url, "2025-11-25", nil)
	pair := meta("shutdown", "session")
	if callIngress(t, c, "workspace_create", map[string]any{}, pair).IsError {
		t.Fatal("create")
	}
	started := make(chan struct{})
	f.mu.Lock()
	f.start = func(ctx context.Context) (<-chan workspace.ExecOutcome, error) {
		close(started)
		done := make(chan workspace.ExecOutcome, 1)
		go func() {
			<-ctx.Done()
			done <- workspace.ExecOutcome{Completion: workspace.DefinitelyCompleted}
			close(done)
		}()
		return done, nil
	}
	f.mu.Unlock()
	returned := make(chan struct{})
	go func() {
		_, _ = c.CallTool(context.Background(), &mcp.CallToolParams{Name: "exec", Arguments: map[string]any{"argv": []string{"work"}}, Meta: pair})
		close(returned)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := g.shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-ctx.Done():
		t.Fatal("shutdown did not drain")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.executed) != 1 {
		t.Fatal("shutdown replayed")
	}
}

func TestDaemonReconcilesBeforeListening(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := listener.Addr().String()
	_ = listener.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	f := &ingressRuntime{objects: make(map[workspace.Exact]workspace.Object), initialize: func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	cfg := ingressConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runDaemonWithRuntime(ctx, cfg, endpoint, func() workspace.Runtime { return f }) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initialization not reached")
	}
	// The endpoint is still unbound while runtime initialization/reconciliation is blocked.
	probe, err := net.Listen("tcp", endpoint)
	if err != nil {
		t.Fatal("listener preceded reconciliation", err)
	}
	_ = probe.Close()
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", endpoint, 20*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listener did not start")
		}
		time.Sleep(time.Millisecond)
	}
	client := connectIngress(t, "http://"+endpoint+"/mcp", "2025-11-25", nil)
	if callIngress(t, client, "workspace_create", map[string]any{}, meta("ready", "session")).IsError {
		t.Fatal("admission failed after readiness")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon shutdown not bounded")
	}
	// Proper daemon close released singleton ownership; the same journal reconciles.
	f.initialize = nil
	reopened, err := workspace.Open(context.Background(), cfg, func() workspace.Runtime { return f })
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonInvalidBindDoesNotInitialize(t *testing.T) {
	called := false
	err := runDaemonWithRuntime(context.Background(), ingressConfig(t), "0.0.0.0:8080", func() workspace.Runtime { called = true; return nil })
	if err == nil || called {
		t.Fatal("invalid bind reached runtime", err)
	}
}

func TestIngressShutdownDeadlineClosesStalledBody(t *testing.T) {
	g, _, _, url := serveIngress(t)
	address := strings.TrimSuffix(strings.TrimPrefix(url, "http://"), "/mcp")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = io.WriteString(conn, "POST /mcp HTTP/1.1\r\nHost: "+address+"\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(g.slots) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(g.slots) != 1 {
		t.Fatal("body read not reached")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = g.shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal("shutdown not bounded", err)
	}
	if !g.stopping.Load() {
		t.Fatal("admission left open")
	}
}

func TestDaemonReconciliationFailureNeverListens(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := listener.Addr().String()
	_ = listener.Close()
	f := &ingressRuntime{initialize: func(context.Context) error { return errors.New("fixture reconcile failure") }}
	err = runDaemonWithRuntime(context.Background(), ingressConfig(t), endpoint, func() workspace.Runtime { return f })
	if err == nil {
		t.Fatal("reconciliation failure ignored")
	}
	probe, err := net.Listen("tcp", endpoint)
	if err != nil {
		t.Fatal("listener opened despite failure", err)
	}
	_ = probe.Close()
}

func TestEnvelopeStripsRawIdentityBeforeSDK(t *testing.T) {
	pair := `{"openai/subject":"private-subject","openai/session":"private-session","io.modelcontextprotocol/protocolVersion":"2026-07-28"}`
	clean, identity, err := admitEnvelope([]byte(wireCall(pair, `{}`)))
	if err != nil || identity.Workspace() == "" || bytes.Contains(clean, []byte("private-")) || !bytes.Contains(clean, []byte("protocolVersion")) {
		t.Fatal("identity was not isolated", err)
	}
	// The maximum decoded field size is accepted (wire JSON may be escaped).
	m := meta(strings.Repeat("s", maxIdentityBytes), strings.Repeat("c", maxIdentityBytes))
	encoded, _ := json.Marshal(m)
	if _, _, err := admitEnvelope([]byte(wireCall(string(encoded), `{}`))); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"method":"tools/call","method":"tools/list","params":{}}`,
		`{"method":"tools/call","params":{"_meta":{},"_meta":{}}}`,
	} {
		if _, _, err := admitEnvelope([]byte(raw)); err == nil {
			t.Fatal("duplicate authority accepted")
		}
	}
}

func TestProductionIngressRejectsBuildingAndFinalizing(t *testing.T) {
	_, _, f, url := serveIngress(t)
	c := connectIngress(t, url, "2025-11-25", nil)
	pair := meta("build", "session")
	building, finishBuild, finalizing, finishCreate := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.build = func(ctx context.Context) (string, error) {
		close(building)
		select {
		case <-finishBuild:
			return "sha256:" + strings.Repeat("b", 64), nil
		case <-ctx.Done():
			return "", workspace.CompletedBackendError(ctx.Err())
		}
	}
	f.finalize = func(ctx context.Context) error {
		close(finalizing)
		select {
		case <-finishCreate:
			return nil
		case <-ctx.Done():
			return workspace.CompletedBackendError(ctx.Err())
		}
	}
	f.mu.Unlock()
	result := callIngress(t, c, "workspace_create", map[string]any{"dockerfile": "FROM alpine:3.23\n"}, pair)
	if result.IsError {
		t.Fatal(result)
	}
	for _, phase := range []struct {
		entered <-chan struct{}
		release chan struct{}
	}{{building, finishBuild}, {finalizing, finishCreate}} {
		select {
		case <-phase.entered:
		case <-time.After(time.Second):
			t.Fatal("phase not reached")
		}
		for _, call := range []struct {
			name string
			args any
		}{{"exec", map[string]any{"argv": []string{"true"}}}, {"read_text", map[string]any{"path": "/f"}}, {"write_text", map[string]any{"path": "/f", "content": "x"}}} {
			result := callIngress(t, c, call.name, call.args, pair)
			encoded, _ := json.Marshal(result)
			if !result.IsError || !bytes.Contains(encoded, []byte("not_ready")) {
				t.Fatal("candidate admitted", string(encoded))
			}
		}
		close(phase.release)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.executed) != 0 {
		t.Fatal("candidate reached execution")
	}
}
