package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolAdmission is supplied by trusted host ingress. It must bind the request
// to an existing exact generation, without create or retry. Neither identity nor
// generation is a caller argument; unverified wire _meta is not trusted here.
// It must honor context cancellation and must not submit runtime work itself.
type ToolAdmission func(context.Context, *mcp.CallToolRequest) (Identity, Exact, error)

// RegisterTools installs the public v0 tools without opening any transport.
func RegisterTools(server *mcp.Server, service *Service, admit ToolAdmission) error {
	if server == nil || service == nil || admit == nil {
		return errors.New("server, service and trusted admission are required")
	}
	type definition struct {
		name, description string
		properties        map[string]any
		required          []string
		call              func(context.Context, Identity, Exact, json.RawMessage) (any, error)
	}
	stringType := map[string]any{"type": "string"}
	definitions := []definition{
		{"exec", "Execute argv in the admitted container generation. Detached success attests native acceptance only.", map[string]any{
			"argv": map[string]any{"type": "array", "items": stringType, "minItems": 1, "maxItems": MaxArgv},
			"cwd":  stringType, "env": map[string]any{"type": "object", "additionalProperties": stringType},
			"stdin": stringType, "detach": map[string]any{"type": "boolean"},
		}, []string{"argv"}, func(ctx context.Context, i Identity, e Exact, raw json.RawMessage) (any, error) {
			var r ExecRequest
			if err := decodeToolRequest(raw, &r, "argv"); err != nil {
				return nil, err
			}
			return service.Exec(ctx, i, e, r)
		}},
		{"read_text", "Read at most 1 MiB of UTF-8 text from an absolute container path.", map[string]any{"path": stringType}, []string{"path"}, func(ctx context.Context, i Identity, e Exact, raw json.RawMessage) (any, error) {
			var r ReadTextRequest
			if err := decodeToolRequest(raw, &r, "path"); err != nil {
				return nil, err
			}
			return service.ReadText(ctx, i, e, r)
		}},
		{"write_text", "Create or truncate a container file with at most 1 MiB of UTF-8 text. Partial content may remain on failure.", map[string]any{"path": stringType, "content": stringType}, []string{"path", "content"}, func(ctx context.Context, i Identity, e Exact, raw json.RawMessage) (any, error) {
			var r WriteTextRequest
			if err := decodeToolRequest(raw, &r, "path", "content"); err != nil {
				return nil, err
			}
			return service.WriteText(ctx, i, e, r)
		}},
	}
	for _, d := range definitions {
		server.AddTool(&mcp.Tool{Name: d.name, Description: d.description, InputSchema: map[string]any{
			"type": "object", "properties": d.properties, "required": d.required, "additionalProperties": false,
		}}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Share one deadline across host admission and the Service operation.
			ctx, cancel := context.WithTimeout(ctx, service.toolTimeout)
			defer cancel()
			i, e, err := awaitToolAdmission(ctx, request, admit)
			var output any
			if err == nil {
				output, err = d.call(ctx, i, e, request.Params.Arguments)
			}
			result := &mcp.CallToolResult{}
			if err != nil {
				result.IsError = true
				output = mapToolError(err)
			}
			encoded, _ := json.Marshal(output) // only bounded, JSON-safe tool structs
			result.Content = []mcp.Content{&mcp.TextContent{Text: string(encoded)}}
			result.StructuredContent = output
			return result, nil
		})
	}
	return nil
}

// Only admission runs in the worker, so a late callback cannot dispatch a tool.
// The buffered result lets a callback finish after the caller has timed out.
func awaitToolAdmission(ctx context.Context, request *mcp.CallToolRequest, admit ToolAdmission) (Identity, Exact, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, Exact{}, err
	}
	type result struct {
		identity Identity
		exact    Exact
		err      error
	}
	finished := make(chan result, 1)
	go func() {
		i, e, err := admit(ctx, request)
		finished <- result{i, e, err}
	}()
	select {
	case r := <-finished:
		if err := ctx.Err(); err != nil {
			return Identity{}, Exact{}, errors.Join(ErrOutcomeUnknown, err)
		}
		return r.identity, r.exact, r.err
	case <-ctx.Done():
		return Identity{}, Exact{}, errors.Join(ErrOutcomeUnknown, ctx.Err())
	}
}

func decodeToolRequest(raw json.RawMessage, destination any, required ...string) error {
	invalid := func() error {
		return toolError("invalid_request", "Tool arguments do not match the request contract.", nil)
	}
	// Covers worst-case JSON escaping of the text and argv/env budgets while
	// bounding decoder allocations and reflected errors before decoding.
	if len(raw) > 8<<20 || !utf8.Valid(raw) {
		return invalid()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return invalid()
	}
	var allowed []string
	switch destination.(type) {
	case *ExecRequest:
		allowed = []string{"argv", "cwd", "env", "stdin", "detach"}
	case *ReadTextRequest:
		allowed = []string{"path"}
	case *WriteTextRequest:
		allowed = []string{"path", "content"}
	default:
		return invalid()
	}
	// Go's decoder matches struct fields case-insensitively; the public JSON
	// contract requires the exact schema names, including for optional fields.
	for name := range fields {
		ok := false
		for _, candidate := range allowed {
			if name == candidate {
				ok = true
				break
			}
		}
		if !ok {
			return invalid()
		}
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return invalid()
		}
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return invalid()
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(destination) != nil {
		return invalid()
	}
	// encoding/json accepts null as the zero value of a Go string, including
	// inside slices/maps. Those are not string values in the public contract.
	if value, ok := fields["argv"]; ok {
		var elements []json.RawMessage
		if json.Unmarshal(value, &elements) != nil {
			return invalid()
		}
		for _, element := range elements {
			if bytes.Equal(bytes.TrimSpace(element), []byte("null")) {
				return invalid()
			}
		}
	}
	if value, ok := fields["env"]; ok {
		var overlay map[string]json.RawMessage
		if json.Unmarshal(value, &overlay) != nil {
			return invalid()
		}
		for _, element := range overlay {
			if bytes.Equal(bytes.TrimSpace(element), []byte("null")) {
				return invalid()
			}
		}
	}
	if value, ok := fields["cwd"]; ok && bytes.Equal(bytes.TrimSpace(value), []byte(`""`)) {
		return validateContainerPath("")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return invalid()
	}
	// encoding/json replaces lone surrogate escapes; text contracts require
	// rejecting them instead of silently changing the supplied content.
	if !validJSONSurrogates(raw) {
		return invalid()
	}
	return nil
}

func validJSONSurrogates(raw []byte) bool {
	hex := func(p []byte) int {
		v := 0
		for _, c := range p {
			v *= 16
			switch {
			case c >= '0' && c <= '9':
				v += int(c - '0')
			case c >= 'a' && c <= 'f':
				v += int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				v += int(c-'A') + 10
			default:
				return -1
			}
		}
		return v
	}
	for n := 0; n < len(raw); n++ {
		if raw[n] != '\\' {
			continue
		}
		n++
		if n >= len(raw) {
			return false
		}
		if raw[n] != 'u' {
			continue
		}
		if n+4 >= len(raw) {
			return false
		}
		v := hex(raw[n+1 : n+5])
		n += 4
		if v >= 0xdc00 && v <= 0xdfff {
			return false
		}
		if v >= 0xd800 && v <= 0xdbff {
			if n+6 >= len(raw) || raw[n+1] != '\\' || raw[n+2] != 'u' {
				return false
			}
			low := hex(raw[n+3 : n+7])
			if low < 0xdc00 || low > 0xdfff {
				return false
			}
			n += 6
		}
	}
	return true
}
