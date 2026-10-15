package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/r-agatsuma/mcp-remote-workspace/internal/workspace"
)

// Deployment-facing policy: one explicitly configured literal loopback endpoint.
// These finite bounds cover worst-case JSON escaping of the existing tool limits.
const (
	maxHTTPBody          = 9 << 20
	maxHTTPHeaders       = 16 << 10
	maxIdentityBytes     = 1024
	maxHTTPRequests      = 8
	maxHTTPConnections   = 32
	httpRequestLifetime  = 45 * time.Second
	httpHeaderLifetime   = 5 * time.Second
	httpShutdownLifetime = 10 * time.Second
)

func validateListen(endpoint string) error {
	host, port, err := net.SplitHostPort(endpoint)
	addr, parseErr := netip.ParseAddr(host)
	p, portErr := strconv.Atoi(port)
	if err != nil || parseErr != nil || !addr.IsLoopback() || addr.Zone() != "" || portErr != nil || p < 1 || p > 65535 || strconv.Itoa(p) != port {
		return errors.New("mcp-listen requires a literal loopback IP and port 1-65535")
	}
	return nil
}

type ingressContextKey struct{}
type admittedRequest struct {
	identity workspace.Identity
	lifetime context.Context
}

type ingress struct {
	server   *http.Server
	handler  http.Handler
	stopping atomic.Bool
	slots    chan struct{}
	cancel   context.CancelFunc
}

// newIngress is the production composition. The caller supplies only the single
// opened/reconciled daemon Service. It is never an authentication adapter for an
// arbitrary public transport. The dedicated host and its loopback clients are
// trusted under #20; workspace access to host loopback must be denied by deployment.
func newIngress(ctx context.Context, service *workspace.Service) (*ingress, error) {
	if service == nil {
		return nil, errors.New("reconciled service is required")
	}
	base, cancel := context.WithCancel(ctx)
	g := &ingress{slots: make(chan struct{}, maxHTTPRequests), cancel: cancel}
	server := newServer()
	// The SDK detaches old-protocol request cancellation. Restore the bounded HTTP
	// lifetime explicitly for both supported protocol families, without replay.
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			admitted, ok := ctx.Value(ingressContextKey{}).(admittedRequest)
			if !ok || g.stopping.Load() {
				return nil, workspace.ErrNotReady
			}
			ctx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(admitted.lifetime, cancel)
			defer stop()
			defer cancel()
			if admitted.lifetime.Err() != nil {
				cancel()
			}
			return next(ctx, method, req)
		}
	})
	err := workspace.RegisterTools(server, service, func(ctx context.Context, req *mcp.CallToolRequest) (workspace.Identity, workspace.Exact, error) {
		admitted, ok := ctx.Value(ingressContextKey{}).(admittedRequest)
		if !ok || g.stopping.Load() || admitted.identity.Workspace() == "" {
			return workspace.Identity{}, workspace.Exact{}, workspace.ErrNotReady
		}
		if err := ctx.Err(); err != nil {
			return workspace.Identity{}, workspace.Exact{}, err
		}
		switch req.Params.Name {
		case "exec", "read_text", "write_text":
			e, err := service.CurrentGeneration(ctx, admitted.identity)
			return admitted.identity, e, err
		default:
			return admitted.identity, workspace.Exact{}, nil
		}
	})
	if err != nil {
		cancel()
		return nil, err
	}
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: maxHTTPBody,
		PropagateRequestCancellation: true,
		// SDK diagnostics can include decoder/request text. Never log wire data.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	g.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.serveHTTP(transport, w, r) })
	// TimeoutHandler bounds the HTTP outcome even if inherited lifecycle work is
	// still unwinding. The inner handler retains its slot until it really exits.
	g.server = &http.Server{Handler: http.TimeoutHandler(g.handler, httpRequestLifetime, "Request lifetime exceeded"), ReadHeaderTimeout: httpHeaderLifetime,
		ReadTimeout: httpRequestLifetime, WriteTimeout: httpRequestLifetime + time.Second,
		IdleTimeout: httpHeaderLifetime, MaxHeaderBytes: maxHTTPHeaders,
		ErrorLog:    log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context { return base }}
	// Bound pre-header/idle sockets as well as decoded/admitted requests. Plain
	// HTTP/1 only: no TLS/HTTP2 listener or per-connection multiplexing is enabled.
	var mu sync.Mutex
	connections := make(map[net.Conn]bool)
	g.server.ConnState = func(c net.Conn, state http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch state {
		case http.StateNew:
			if len(connections) >= maxHTTPConnections {
				_ = c.Close()
			} else {
				connections[c] = true
			}
		case http.StateClosed, http.StateHijacked:
			delete(connections, c)
		}
	}
	return g, nil
}

func loopbackPeer(address string) bool {
	host, _, err := net.SplitHostPort(address)
	ip, parseErr := netip.ParseAddr(host)
	return err == nil && parseErr == nil && ip.IsLoopback() && ip.Zone() == ""
}

func (g *ingress) serveHTTP(next http.Handler, w http.ResponseWriter, r *http.Request) {
	reject := func(status int) { http.Error(w, http.StatusText(status), status) }
	if g.stopping.Load() || r.Context().Err() != nil {
		reject(http.StatusServiceUnavailable)
		return
	}
	// Host validation blocks DNS rebinding before the SDK can reflect Host text.
	// Browser-origin traffic is not part of the trusted Tunnel-to-loopback path.
	if !loopbackPeer(r.RemoteAddr) || !loopbackPeer(r.Host) || r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
		reject(http.StatusForbidden)
		return
	}
	if r.URL.Path != "/mcp" || r.URL.RawQuery != "" {
		reject(http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		reject(http.StatusMethodNotAllowed)
		return
	}
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		reject(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), httpRequestLifetime)
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, maxHTTPBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			reject(http.StatusRequestEntityTooLarge)
		} else {
			reject(http.StatusBadRequest)
		}
		return
	}
	clean, identity, err := admitEnvelope(raw)
	if err != nil {
		reject(http.StatusBadRequest)
		return
	}
	if ctx.Err() != nil || g.stopping.Load() {
		reject(http.StatusServiceUnavailable)
		return
	}
	r = r.Clone(context.WithValue(ctx, ingressContextKey{}, admittedRequest{identity, ctx}))
	// Even SDK compatibility switches cannot turn a transport identifier into state.
	r.Header.Del("Mcp-Session-Id")
	r.Body = io.NopCloser(bytes.NewReader(clean))
	r.ContentLength = int64(len(clean))
	next.ServeHTTP(w, r)
}

// object rejects duplicate keys and non-objects at the three authority-bearing
// envelope levels. Batches are intentionally unsupported: one HTTP slot admits
// at most one MCP request. No generic JSON-RPC implementation is introduced.
func object(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("invalid request")
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		token, err = d.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, errors.New("invalid request")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("invalid request")
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, errors.New("invalid request")
		}
		fields[key] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, errors.New("invalid request")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid request")
	}
	return fields, nil
}

// Admission is made only after the production loopback boundary has checked the
// peer and browser/rebinding guards. Raw OpenAI identity is removed before SDK
// parsing/logging; only the derived Identity crosses the private context boundary.
func admitEnvelope(raw []byte) ([]byte, workspace.Identity, error) {
	invalid := errors.New("invalid MCP request or trusted identity")
	if !utf8.Valid(raw) {
		return nil, workspace.Identity{}, invalid
	}
	envelope, err := object(raw)
	if err != nil {
		return nil, workspace.Identity{}, invalid
	}
	var method string
	if json.Unmarshal(envelope["method"], &method) != nil || len(method) > 128 {
		return nil, workspace.Identity{}, invalid
	}
	if method != "tools/call" {
		return raw, workspace.Identity{}, nil
	}
	params, err := object(envelope["params"])
	if err != nil {
		return nil, workspace.Identity{}, invalid
	}
	var toolName string
	if json.Unmarshal(params["name"], &toolName) != nil || len(toolName) > 128 || len(params["_meta"]) > 16<<10 {
		return nil, workspace.Identity{}, invalid
	}
	meta, err := object(params["_meta"])
	if err != nil {
		return nil, workspace.Identity{}, invalid
	}
	pair := make(map[string]any, 2)
	for _, key := range []string{"openai/subject", "openai/session"} {
		var value string
		if json.Unmarshal(meta[key], &value) != nil || value == "" || len(value) > maxIdentityBytes {
			return nil, workspace.Identity{}, invalid
		}
		// encoding/json replaces malformed surrogate escapes with U+FFFD. Reject
		// replacement characters conservatively so malformed metadata cannot alias.
		for _, c := range value {
			if c == '\ufffd' {
				return nil, workspace.Identity{}, invalid
			}
		}
		pair[key] = value
		delete(meta, key)
	}
	identity, err := workspace.IdentityFromTrustedMeta(pair)
	if err != nil {
		return nil, workspace.Identity{}, invalid
	}
	params["_meta"], _ = json.Marshal(meta)
	envelope["params"], _ = json.Marshal(params)
	clean, err := json.Marshal(envelope)
	return clean, identity, err
}

func (g *ingress) shutdown(ctx context.Context) error {
	g.stopping.Store(true)
	g.cancel()
	err := g.server.Shutdown(ctx)
	if err != nil {
		_ = g.server.Close()
	}
	return err
}
