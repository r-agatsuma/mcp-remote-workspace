//go:build linux

package workspace

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestBuildDNSRequiresTrustedPublicDefaults(t *testing.T) {
	for _, value := range []string{"127.0.0.53", "169.254.169.254", "10.0.0.53", "172.16.0.53", "192.168.0.53", "100.64.0.53", "0.0.0.0", "224.0.0.1", "255.255.255.255", "::", "::1", "fc00::53", "fe80::53", "ff02::1", "::ffff:127.0.0.53", "2001:db8::53", "fe80::1%eth0", "localhost", "dns.example", "1.1.1.1:53", "none", ""} {
		cfg := testConfig(t.TempDir())
		cfg.BuildDNS = []string{value}
		if err := cfg.Validate(); err == nil {
			t.Errorf("prohibited default accepted: %s", value)
		}
		p, stub := podmanStub(t)
		p.buildConfig.DNS = cfg.BuildDNS
		if err := p.Initialize(context.Background()); err == nil || stub.calls != 0 {
			t.Errorf("invalid defaults reached engine: %s", value)
		}
	}
	for _, values := range [][]string{nil, {"1.1.1.1", "2606:4700:4700::1111"}, {"::ffff:8.8.8.8"}} {
		if err := (PodmanBuildConfig{DNS: values}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	input := []string{"1.1.1.1"}
	p := NewPodmanWithBuildConfig("/stub/podman", testImage, ReferencePolicy(), PodmanBuildConfig{DNS: input})
	input[0] = "127.0.0.53"
	if p.buildConfig.DNS[0] != "1.1.1.1" {
		t.Fatal("constructor retained mutable resolver input")
	}
}

func TestPodmanBuildDefaultDNSAndPrivateAttribution(t *testing.T) {
	for _, resolvers := range [][]string{nil, {"1.1.1.1", "2606:4700:4700::1111"}} {
		t.Run(strings.Join(resolvers, ","), func(t *testing.T) {
			e := Exact{identity(t, "s", "dns").Workspace(), Generation(strings.Repeat("d", 64))}
			// Untrusted ENV and RUN text can select public endpoints for workload
			// use, but cannot select the service's default resolver or host config.
			plan, err := compileDockerfile("FROM scratch\nENV DNS=127.0.0.53\nRUN echo dns.example\n", nil)
			if err != nil {
				t.Fatal(err)
			}
			p, stub := podmanStub(t, infoStep(), imageListStep(t))
			cli := &buildCLIStub{stubCLI: stub, result: BackendResult{DefinitelyCompleted, injected}}
			p.runner = cli
			path := configureBuildFixture(t, p, e, plan)
			p.buildConfig.DNS = resolvers
			if err := p.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			if result := p.Build(context.Background(), e, plan, path, io.Discard, io.Discard); result.Err == nil {
				t.Fatal("expected fixture failure")
			}
			var got []string
			for _, arg := range cli.args {
				if v, ok := strings.CutPrefix(arg, "--dns="); ok {
					got = append(got, v)
				}
			}
			if !reflect.DeepEqual(got, resolvers) {
				t.Fatalf("defaults=%v want %v", got, resolvers)
			}
			module := filepath.Join(path, "build-container.conf")
			if cli.args[0] != "--module="+module {
				t.Fatal("missing candidate attribution module")
			}
			data, err := os.ReadFile(module)
			if err != nil || string(data) != "[containers]\ndefault_mounts_file = "+strconv.Quote(filepath.Join(path, buildMountsFile))+"\n" {
				t.Fatalf("unexpected module: %q %v", data, err)
			}
			mounts, err := os.ReadFile(filepath.Join(path, buildMountsFile))
			if err != nil || len(mounts) != 0 {
				t.Fatal("build received default host mounts")
			}
			for _, name := range []string{module, filepath.Join(path, buildMountsFile)} {
				info, err := os.Stat(name)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("metadata is not private")
				}
			}
			entries, err := os.ReadDir(filepath.Join(path, "context"))
			if err != nil || len(entries) != 1 || entries[0].Name() != "Dockerfile" {
				t.Fatal("metadata entered caller context")
			}
			if !reflect.DeepEqual(p.buildConfig.DNS, resolvers) {
				t.Fatal("Dockerfile mutated service defaults")
			}
		})
	}
}

func TestBuildAttributionRejectsReusedMetadata(t *testing.T) {
	e := Exact{identity(t, "s", "metadata").Workspace(), Generation(strings.Repeat("d", 64))}
	p := &Podman{buildConfig: PodmanBuildConfig{DataDir: t.TempDir()}}
	path := p.candidateBuildPath(e)
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", filepath.Join(path, buildMountsFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.prepareBuildAttribution(e, path); err == nil {
		t.Fatal("substituted metadata accepted")
	}
}
