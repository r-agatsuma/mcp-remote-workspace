package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const namePrefix = "mcpws-v1-"
const labelPrefix = "io.mcp-workspaced."
const ownerLabel = labelPrefix + "owner"
const workspaceLabel = labelPrefix + "workspace"
const generationLabel = labelPrefix + "generation"
const imageLabel = labelPrefix + "image"
const ownerValue = "mcp-workspaced-v1"

type Object struct {
	ID     string
	Name   string
	Labels map[string]string
	Image  string // canonical image reference used to create the object
}

// Runtime is intentionally container-only and exact-generation scoped.
// Errors may mean that a mutation partially completed. The core must retain
// intent until absence has been independently confirmed. Create/Destroy errors
// do not attest backend completion unless wrapped in CompletedBackendError.
// Initialize/List are synchronous, read-only probes with no surviving backend
// work on return; adapters must wait for their local probe process to exit.
type Runtime interface {
	Initialize(context.Context) error
	List(context.Context) ([]Object, error)
	Create(context.Context, Exact, string) error
	Destroy(context.Context, Exact) error
}

func objectName(e Exact) string {
	return namePrefix + string(e.Workspace) + "-" + string(e.Generation)
}

func objectLabels(e Exact, image string) map[string]string {
	return map[string]string{ownerLabel: ownerValue, workspaceLabel: string(e.Workspace),
		generationLabel: string(e.Generation), imageLabel: image}
}

// classify ignores unrelated objects, but rejects anything claiming the service
// namespace without the complete, internally consistent ownership contract.
func classify(o Object) (Exact, bool, error) {
	claimed := strings.HasPrefix(o.Name, "mcpws-")
	for label := range o.Labels {
		claimed = claimed || strings.HasPrefix(label, labelPrefix)
		if strings.HasPrefix(label, labelPrefix) && label != ownerLabel && label != workspaceLabel && label != generationLabel && label != imageLabel {
			return Exact{}, false, errors.New("unknown service ownership label")
		}
	}
	if !claimed {
		return Exact{}, false, nil
	}
	e := Exact{WorkspaceKey(o.Labels[workspaceLabel]), Generation(o.Labels[generationLabel])}
	if e.validate() != nil || o.Name != objectName(e) || o.Labels[ownerLabel] != ownerValue ||
		!validRuntimeImage(e, o.Image) || o.Labels[imageLabel] != o.Image || !validHex(o.ID) {
		return Exact{}, false, errors.New("unrecognized service/lookalike runtime object")
	}
	return e, true, nil
}

func validImage(image string) bool {
	name, digest, ok := strings.Cut(image, "@sha256:")
	return ok && name != "" && !strings.HasPrefix(name, "-") &&
		!strings.ContainsAny(name, " \t\r\n@") && validHex(digest)
}

func discover(ctx context.Context, runtime Runtime, entries map[WorkspaceKey]Exact, image string) (map[Exact]Object, error) {
	objects, err := runtime.List(ctx)
	if err != nil {
		return nil, err
	}
	known := make(map[Exact]Object)
	for _, o := range objects {
		e, owned, err := classify(o)
		if err != nil {
			return nil, err
		}
		if !owned {
			continue
		}
		if entries[e.Workspace] != e || o.Image != image {
			return nil, fmt.Errorf("runtime generation has no matching canonical journal intent: %s", o.Name)
		}
		if _, duplicate := known[e]; duplicate {
			return nil, errors.New("duplicate runtime generation")
		}
		known[e] = o
	}
	return known, nil
}

func buildImageName(e Exact) string {
	return "localhost/" + generationImageNamespace + "/" + string(e.Workspace) + ":" + string(e.Generation)
}
func validRuntimeImage(e Exact, image string) bool {
	return validImage(image) || e.validate() == nil && image == buildImageName(e)
}
func (s *Service) discover(ctx context.Context) (map[Exact]Object, error) {
	objects, err := s.runtime.List(ctx)
	if err != nil {
		return nil, err
	}
	known := map[Exact]Object{}
	for _, o := range objects {
		e, owned, err := classify(o)
		if err != nil {
			return nil, err
		}
		if !owned {
			continue
		}
		if s.entries[e.Workspace] != e || s.records[e].Image != o.Image {
			return nil, errors.New("runtime generation has no matching journal image intent")
		}
		if _, duplicate := known[e]; duplicate {
			return nil, errors.New("duplicate runtime generation")
		}
		known[e] = o
	}
	return known, nil
}
