package canonical

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/moby/buildkit/frontend/dockerfile/parser"
)

// Use the repository's existing Dockerfile parser plus a narrow recipe contract.
// This test performs no build, registry lookup or image pull.
func TestDockerfileContract(t *testing.T) {
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parser.Parse(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	var runs []string
	labels := map[string]string{}
	user, workdir, base, revisionArgs := "", "", "", 0
	for _, node := range parsed.AST.Children {
		if len(node.Flags) != 0 || len(node.Heredocs) != 0 || node.Next == nil {
			t.Fatal("unexpected flags, heredoc or missing argument", node.Original)
		}
		switch strings.ToLower(node.Value) {
		case "from":
			if base != "" || node.Next.Next != nil {
				t.Fatal("require one stage and no overridable base")
			}
			base = node.Next.Value
			if !regexp.MustCompile(`^docker.io/library/ubuntu:24\.04@sha256:[0-9a-f]{64}$`).MatchString(base) {
				t.Fatal("require pinned Ubuntu base", base)
			}
		case "arg":
			if node.Next.Value != "SOURCE_REVISION" || node.Next.Next != nil {
				t.Fatal("only required source revision may be supplied as build input")
			}
			revisionArgs++
		case "label":
			for pair := node.Next; pair != nil; pair = pair.Next.Next.Next {
				if pair.Next == nil || pair.Next.Next == nil || pair.Next.Next.Value != "=" {
					t.Fatal("invalid label", node.Original)
				}
				if _, exists := labels[pair.Value]; exists {
					t.Fatal("duplicate label", pair.Value)
				}
				labels[pair.Value] = strings.Trim(pair.Next.Value, `"`)
			}
		case "run":
			if node.Attributes["json"] || node.Next.Next != nil {
				t.Fatal("require bounded shell RUN", node.Original)
			}
			runs = append(runs, node.Next.Value)
		case "user":
			user = node.Next.Value
		case "workdir":
			workdir = node.Next.Value
		default:
			// No context imports, ENV/secrets, ENTRYPOINT, service ports, volumes,
			// ONBUILD or additional runtime requirements belong in this toolbox.
			t.Fatalf("unexpected image instruction: %s", node.Original)
		}
	}
	if user != "0" || workdir != "/workspace" || revisionArgs != 1 || len(runs) != 2 {
		t.Fatal("user/workdir/build-input contract changed", user, workdir, revisionArgs, runs)
	}
	if labels["org.opencontainers.image.source"] != "https://github.com/r-agatsuma/mcp-remote-workspace" ||
		labels["org.opencontainers.image.revision"] != "${SOURCE_REVISION}" ||
		labels["org.opencontainers.image.base.name"] != "docker.io/library/ubuntu:24.04" ||
		!strings.HasSuffix(base, "@"+labels["org.opencontainers.image.base.digest"]) {
		t.Fatal("source/revision/base provenance missing or inconsistent", labels)
	}
	if !strings.Contains(runs[0], `test "${#SOURCE_REVISION}" -eq 40`) || !strings.Contains(runs[0], `*[!0-9a-f]*) exit 1`) {
		t.Fatal("missing exact revision guard", runs[0])
	}
	install := regexp.MustCompile(`^apt-get update &&\s+DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends\s+(.+?) &&\s+ln -s /usr/bin/fdfind /usr/local/bin/fd &&\s+rm -rf /var/lib/apt/lists/\*$`)
	match := install.FindStringSubmatch(runs[1])
	if match == nil {
		t.Fatal("expected bounded distro-only installation, fd alias and apt cleanup", runs[1])
	}
	packages := strings.Fields(match[1])
	slices.Sort(packages)
	want := strings.Fields("bash build-essential ca-certificates coreutils curl fd-find file git git-lfs jq less openssh-client patch procps python3 python3-pip python3-venv ripgrep rsync tree unzip wget zip")
	if !slices.Equal(packages, want) {
		t.Fatal("required/allowed package baseline changed", packages)
	}
}
