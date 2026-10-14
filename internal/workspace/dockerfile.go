package workspace

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/distribution/reference"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
)

const maxDockerfileBytes = 1 << 20

var ErrDockerfile = errors.New("Dockerfile is invalid or its image sources/runtime overrides cannot be safely classified")

// Syntax belongs to the maintained Dockerfile parser. This function owns only
// source and isolation policy. Image names retain normal short-name, registry,
// tag and digest syntax. Dynamic image expressions fail closed before consumption.
func dockerfileSources(text string) ([]string, error) {
	if len(text) == 0 || len(text) > maxDockerfileBytes || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return nil, ErrDockerfile
	}
	parsed, err := parser.Parse(strings.NewReader(text))
	if err != nil {
		return nil, ErrDockerfile
	}
	stages, seen := map[string]bool{}, map[string]bool{}
	var sources []string
	stageCount := 0
	classifySource := func(value string) error {
		if stages[value] {
			return nil
		}
		if index, err := strconv.Atoi(value); err == nil && index >= 0 && index < stageCount {
			return nil
		}
		if strings.ContainsAny(value, "$\\\"'`") || value == "" {
			return ErrDockerfile
		}
		ref, err := reference.ParseAnyReference(value)
		if err != nil {
			return ErrDockerfile
		}
		if named, ok := ref.(reference.Named); ok {
			for part := range strings.SplitSeq(reference.Path(named), "/") {
				if strings.HasPrefix(part, namePrefix) {
					return ErrDockerfile
				}
			}
		}
		if value != "scratch" && !seen[value] {
			sources = append(sources, value)
			seen[value] = true
		}
		return nil
	}
	nodes := append([]*parser.Node(nil), parsed.AST.Children...)
	lex := shell.NewLex(parsed.EscapeToken)
	for index := 0; index < len(nodes); index++ {
		node := nodes[index]
		switch strings.ToLower(node.Value) {
		case "onbuild":
			if node.Next == nil || len(node.Next.Children) != 1 {
				return nil, ErrDockerfile
			}
			// Inspect inherited instructions too, before any stage can execute them.
			nodes = append(nodes[:index+1], append(node.Next.Children, nodes[index+1:]...)...)
		case "from":
			// Platform selection could consume a different image from the one
			// inspected before submission. Only the host-default platform is used.
			if len(node.Flags) != 0 || node.Next == nil || classifySource(node.Next.Value) != nil {
				return nil, ErrDockerfile
			}
			if as := node.Next.Next; as != nil {
				if !strings.EqualFold(as.Value, "as") || as.Next == nil || as.Next.Next != nil {
					return nil, ErrDockerfile
				}
				if strings.ContainsAny(as.Next.Value, "$\\\"'`") {
					return nil, ErrDockerfile
				}
				stages[as.Next.Value] = true
			}
			stageCount++
		case "copy":
			for _, flag := range node.Flags {
				name, value, _ := strings.Cut(flag, "=")
				if strings.Contains(name, "$") {
					return nil, ErrDockerfile
				}
				name, _, err := lex.ProcessWord(name, shell.EnvsFromSlice(nil))
				if err != nil || name == "--from" && classifySource(value) != nil {
					return nil, ErrDockerfile
				}
			}
		case "label":
			// Intermediate-image labels are applied before Dockerfile labels.
			// Reserve their keys, including quoted/escaped spellings; dynamic
			// keys cannot be classified before image consumption.
			for key := node.Next; key != nil; key = key.Next.Next.Next {
				if key.Next == nil || key.Next.Next == nil || strings.Contains(key.Value, "$") {
					return nil, ErrDockerfile
				}
				value, _, err := lex.ProcessWord(key.Value, shell.EnvsFromSlice(nil))
				if err != nil || strings.HasPrefix(value, labelPrefix) {
					return nil, ErrDockerfile
				}
			}
		case "run":
			for _, flag := range node.Flags {
				// Buildah expands the whole flag before interpreting mount keys.
				// Expansion could introduce an otherwise invisible from= source.
				if strings.Contains(flag, "$") {
					return nil, ErrDockerfile
				}
				flag, _, err := lex.ProcessWord(flag, shell.EnvsFromSlice(nil))
				if err != nil {
					return nil, ErrDockerfile
				}
				mount, ok := strings.CutPrefix(flag, "--mount=")
				if !ok { // no caller-selected network, security or device profile
					return nil, ErrDockerfile
				}
				for field := range strings.SplitSeq(mount, ",") {
					key, value, _ := strings.Cut(field, "=")
					if key == "from" && classifySource(value) != nil || key == "type" && (value == "secret" || value == "ssh") {
						return nil, ErrDockerfile
					}
				}
			}
		}
	}
	if stageCount == 0 {
		return nil, ErrDockerfile
	}
	return sources, nil
}
