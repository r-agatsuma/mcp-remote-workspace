package workspace

import (
	"errors"
	"strings"
	"testing"
)

func TestImageSourcePolicy(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, ref := range []string{"docker.io/library/alpine:3.20", "registry.example:5000/team/image:tag", "registry:5000/team/image:tag", "[2001:db8::1]:5000/repo:v1", "[2001:db8::1]/repo:v1", "registry.example/repo@sha256:" + digest, "REGISTRY.EXAMPLE./repo:v1", "10.0.0.1/repo:v1"} {
		t.Run(ref, func(t *testing.T) {
			if _, err := normalizeSource(ref); err != nil {
				t.Fatalf("explicit source rejected: %v", err)
			}
		})
	}
	for _, ref := range []string{"alpine", "alpine:latest", "library/alpine:v1", "docker.io/library/alpine", "localhost/repo:v1", "LOCALHOST.:5000/repo:v1", "x.localhost/repo:v1", "127.0.0.1/repo:v1", "127.1/repo:v1", "0177.0.0.1/repo:v1", "0x7f000001:5000/repo:v1", "2130706433:5000/repo:v1", "[::1]/repo:v1", "[::ffff:127.0.0.1]:5000/repo:v1", "0.0.0.0/repo:v1", "containers-storage:docker.io/repo:v1", "dir:/repo:v1", "oci:/repo:v1", "oci-archive:/repo:v1", "docker-archive:/repo:v1", "docker://docker.io/repo:v1", "https://docker.io/repo:v1", "docker.io/../repo:v1", "docker.io/repo:v1@sha256:bad", "docker.io/repo:v1:extra", "registry.example/" + generationImageNamespace + "/image:v1"} {
		t.Run(ref, func(t *testing.T) {
			if _, err := normalizeSource(ref); !errors.Is(err, ErrDockerfilePolicy) {
				t.Fatalf("unsafe source accepted: %v", err)
			}
		})
	}
}
func TestDockerfileCompilationAndResolution(t *testing.T) {
	for _, source := range []string{
		"FROM scratch\nCMD [\"/bin/sh\"]\n",
		"ARG REG=docker.io\nARG IMAGE=${REG}/library/alpine:3.20\nFROM $IMAGE AS base\nRUN echo ok\nFROM scratch\nCOPY --from=BASE /bin/sh /bin/sh\nCOPY --from=0 /bin/sh /sh\n",
		"FROM docker.io/library/alpine:3.20 AS base\nFROM base AS final\nCOPY --from=base /bin/sh /bin/sh\n",
		"FROM scratch\nCOPY --chown=0:0 --from=registry.example/tool:v1 /tool /tool\n",
		"FROM scratch\nENV TOOL=registry.example/tool:v1\nCOPY --from=${TOOL} /tool /tool\n",
		"ARG TOOL=registry.example/tool:v1\nFROM scratch\nARG TOOL\nCOPY --from=$TOOL /tool /tool\n",
		"FROM \\\n registry.example/tool:v1\nCOPY . /src\n",
	} {
		plan, err := compileDockerfile(source, nil)
		if err != nil {
			t.Fatalf("compile %q: %v", source, err)
		}
		for _, line := range strings.Split(plan.dockerfile, "\n") {
			if strings.HasPrefix(line, "FROM ") || strings.HasPrefix(line, "COPY --from=") {
				if strings.Contains(line, "$") {
					t.Fatalf("unresolved source: %s", line)
				}
			}
		}
	}
	for _, source := range []string{
		"FROM alpine:latest", "FROM localhost/repo:v1", "FROM containers-storage:alpine", "FROM dir:/tmp/context",
		"ARG IMAGE\nFROM $IMAGE", "ARG IMAGE=alpine:latest\nFROM ${IMAGE}", "FROM ${TARGETPLATFORM}", "ARG IMAGE=registry.example/repo:v1\nFROM ${IMAGE:-alpine}",
		"FROM scratch\nCOPY --from=alpine /x /x", "FROM scratch\nCOPY --from=future /x /x\nFROM registry.example/repo:v1 AS future",
		"FROM scratch\nCOPY --from=$UNKNOWN /x /x", "FROM scratch\nCOPY --from=2 /x /x", "FROM scratch\nCOPY --from=oci-archive:/x /x /x",
		"FROM scratch\nRUN --mount=type=secret,id=x cat /x", "FROM scratch\nRUN --mount=from=alpine cat /x", "FROM scratch\nRUN --network=host echo ok", "FROM scratch\nONBUILD COPY --from=alpine /x /x",
		"# syntax=docker/dockerfile:1\nFROM scratch", "# escape=`\nFROM scratch", "FROM scratch\nRUN <<EOF\nFROM alpine\nEOF",
		"FROM scratch\nADD https://registry.example/context /src", "FROM scratch\nLABEL io.mcp-workspaced.owner=foo", "FROM --platform=$TARGETPLATFORM registry.example/repo:v1",
		"FROM scratch AS base\nFROM scratch AS base", "FROM scratch AS 0", "FROM scratch\nCOPY --from=registry.example/repo:v1 --from=alpine /x /x",
		"FROM scratch\nCOPY --from registry.example/repo:v1 /x /x", "FROM scratch\nENV TOOL=\"alpine\"\nCOPY --from=$TOOL /x /x",
		"FROM scratch\nRUN true\\", "FROM scratch\x00", "COPY . /x", strings.Repeat("a", MaxDockerfileBytes+1),
	} {
		if _, err := compileDockerfile(source, nil); !errors.Is(err, ErrDockerfilePolicy) {
			t.Errorf("unsafe/ambiguous source accepted: %q", source)
		}
	}
}
func TestKnownGenerationAliasesAndDigestsRejected(t *testing.T) {
	d := strings.Repeat("b", 64)
	known := []string{"registry.example/alias:v1", "registry.example/old@sha256:" + d, "sha256:" + strings.Repeat("c", 64), "docker.io/library/service:v1"}
	for _, ref := range []string{"REGISTRY.EXAMPLE/alias:v1", "registry.example./alias:v1", "other.example/name@sha256:" + d, "other.example/name@sha256:" + strings.Repeat("c", 64), "index.docker.io/service:v1", "docker.io:443/service:v1"} {
		for _, source := range []string{"FROM " + ref, "FROM scratch\nCOPY --from=" + ref + " /x /x"} {
			if _, err := compileDockerfile(source, known); err == nil {
				t.Fatalf("known generation accepted: %s", source)
			}
		}
	}
}
