package mcpserver

import (
	"encoding/json"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
)

func pointer[T any](v T) *T { return &v }

func schemaFor[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](&jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[WorkspaceID]():  {Type: "string", MinLength: pointer(1), Description: "Opaque workspace ID; never a container ID or host path."},
		reflect.TypeFor[SourceType]():   {Type: "string", Enum: []any{SourceEmpty, SourcePublicGit}},
		reflect.TypeFor[ChangeStatus](): {Type: "string", Enum: []any{ChangeAdded, ChangeModified, ChangeDeleted, ChangeRenamed, ChangeUntracked}},
		reflect.TypeFor[SHA256]():       {Type: "string", Pattern: "^[0-9a-f]{64}$", Description: "SHA-256 of exact file bytes, in lowercase hexadecimal."},
		reflect.TypeFor[Timestamp]():    {Type: "string", Format: "date-time", Pattern: "Z$", Description: "RFC3339 UTC timestamp."},
		reflect.TypeFor[ReturnedPath](): {Type: "string", Pattern: "^([^/]+/)*[^/]+$", Not: &jsonschema.Schema{Pattern: `(^|/)(\.|\.\.)(/|$)`}, Description: "Normalized workspace-relative POSIX path."},
	}})
	if err != nil {
		panic(err) // All types are statically defined by this package.
	}
	if size := s.Properties["size_bytes"]; size != nil {
		size.Minimum = pointer(float64(0))
	}
	if path := s.Properties["path"]; path != nil {
		path.Description = "POSIX path relative to /workspace; must not escape the root. Returned paths are normalized."
	}
	return s
}

// Require the public Git fields only in the public_git branch. In the empty
// branch their presence (even an empty string) is rejected.
func sourceConditions(s *jsonschema.Schema, gitFields ...string) {
	s.If = &jsonschema.Schema{Properties: map[string]*jsonschema.Schema{"type": {Const: pointer(any(SourcePublicGit))}}, Required: []string{"type"}}
	s.Then = &jsonschema.Schema{Required: []string{"url"}}
	var forbidden []*jsonschema.Schema
	for _, field := range gitFields {
		forbidden = append(forbidden, &jsonschema.Schema{Required: []string{field}})
	}
	s.Else = &jsonschema.Schema{Not: &jsonschema.Schema{AnyOf: forbidden}}
}

func createInputSchema() *jsonschema.Schema {
	s := schemaFor[WorkspaceCreateInput]()
	source := s.Properties["source"]
	// Optional means omitted, not JSON null.
	source.Type, source.Types = "object", nil
	source.Default = json.RawMessage(`{"type":"empty"}`)
	sourceConditions(source, "url", "ref")
	return s
}

func createOutputSchema() *jsonschema.Schema {
	s := schemaFor[WorkspaceCreateOutput]()
	sourceConditions(s.Properties["source"], "url", "requested_ref", "resolved_commit_sha")
	return s
}

func execInputSchema() *jsonschema.Schema {
	s := schemaFor[ExecInput]()
	s.Properties["argv"].MinItems = pointer(1)
	s.Properties["argv"].Items.MinLength = pointer(1)
	s.Properties["cwd"].Default = json.RawMessage(`"."`)
	s.Properties["cwd"].Description = "POSIX path relative to /workspace; must not escape the root."
	s.Properties["timeout_seconds"].Minimum = pointer(float64(1))
	s.Properties["timeout_seconds"].Description = "Positive seconds; backend defaults and maximum still apply."
	return s
}

func changesOutputSchema() *jsonschema.Schema {
	s := schemaFor[WorkspaceChangesOutput]()
	s.Properties["changes"].Description = "Changes from the actual base_ref to current filesystem state, including local commits, staged, unstaged, and untracked files; sorted by path."
	change := s.Properties["changes"].Items
	change.If = &jsonschema.Schema{Properties: map[string]*jsonschema.Schema{"status": {Const: pointer(any(ChangeRenamed))}}, Required: []string{"status"}}
	change.Then = &jsonschema.Schema{Required: []string{"old_path"}}
	change.Else = &jsonschema.Schema{Not: &jsonschema.Schema{Required: []string{"old_path"}}}
	return s
}

func destroyOutputSchema() *jsonschema.Schema {
	s := schemaFor[WorkspaceDestroyOutput]()
	s.Properties["destroyed"].Const = pointer(any(true))
	return s
}
