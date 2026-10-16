//go:build linux

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestTrustedPublicDNSValidation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		input, want []string
	}{
		{"one", []string{"9.9.9.9"}, []string{"9.9.9.9"}},
		{"ordered", []string{"2606:4700:4700:0:0:0:0:1111", "9.9.9.9"}, []string{"2606:4700:4700::1111", "9.9.9.9"}},
		{"maximum", []string{"9.9.9.9", "8.8.4.4", "1.0.0.1"}, []string{"9.9.9.9", "8.8.4.4", "1.0.0.1"}},
		{"mapped", []string{"::ffff:9.9.9.9"}, []string{"9.9.9.9"}},
		{"missing", nil, nil},
		{"too-many", []string{"9.9.9.9", "8.8.4.4", "1.0.0.1", "1.1.1.1"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validatedPublicDNS(tc.input)
			if tc.want == nil {
				if err == nil {
					t.Fatal("invalid resolvers accepted")
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatal(got, err)
			}
			tc.input[0] = "127.0.0.1"
			if !slices.Equal(got, tc.want) {
				t.Fatal("resolver storage aliased input")
			}
		})
	}
	for _, bad := range []string{"", "dns.example", "9.9.9.9:53", " 9.9.9.9", "0.0.0.0", "::", "127.0.0.53", "::1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "100.64.0.1", "169.254.169.254", "169.254.123.123", "fd00::1", "fe80::1", "fe80::1%eth0", "2606:4700:4700::1111%eth0", "::ffff:9.9.9.9%eth0", "::ffff:192.168.1.1", "::ffff:192.0.2.1", "192.0.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "240.0.0.1", "255.255.255.255", "224.0.0.1", "ff02::1", "2001:db8::1", "0.1.2.3", "192.88.99.1", "100::1", "64:ff9b:1::1", "2001:2::1", "2001:10::1", "2001:20::1", "2002:a00:1::1", "3fff::1", "fec0::1"} {
		t.Run(bad, func(t *testing.T) {
			if _, err := validatedPublicDNS([]string{bad}); err == nil {
				t.Fatal("non-public resolver accepted")
			}
			if err := publicResolvers(strings.NewReader("nameserver " + bad + "\n")); err == nil && bad != " 9.9.9.9" {
				t.Fatal("host validation differs")
			}
		})
	}
}

// Exercise Open's real configuration path and both final Create branches. The
// fake engine returns image metadata but never runs Podman or network traffic.
type profileCLI struct {
	t      *testing.T
	e      Exact
	image  string
	create []string
}

func (c *profileCLI) run(_ context.Context, args ...string) ([]byte, error) {
	switch {
	case slices.Equal(args, []string{"info", "--format=json"}):
		return []byte(infoStep().output), nil
	case args[0] == "container" && args[1] == "list":
		return []byte("[]"), nil
	case args[0] == "image" && args[1] == "inspect":
		return []byte(jsonText(c.t, []any{map[string]any{"Id": c.image, "Config": map[string]any{"Labels": objectLabels(c.e, buildImageName(c.e)), "Volumes": map[string]any{"/host": struct{}{}}, "Healthcheck": map[string]any{"Test": []string{"CMD", "hostile"}}, "ExposedPorts": map[string]any{"8080/tcp": struct{}{}}, "Env": []string{"CONTAINER_HOST=unix:///run/podman/podman.sock", "HTTP_PROXY=http://127.0.0.1"}}}})), nil
	case args[0] == "container" && args[1] == "create":
		c.create = slices.Clone(args)
		return []byte(string(c.e.Generation)), nil
	case args[0] == "container" && args[1] == "inspect":
		return []byte(jsonText(c.t, []any{map[string]any{"Id": string(c.e.Generation), "Name": objectName(c.e), "Image": c.image, "ImageName": c.image, "Config": map[string]any{"Labels": objectLabels(c.e, c.image)}}})), nil
	case args[0] == "container" && args[1] == "start":
		return nil, nil
	default:
		c.t.Fatalf("unexpected command: %v", args)
		return nil, nil
	}
}

func TestTrustedProfileOpenBuildAndFinalParity(t *testing.T) {
	e := Exact{WorkspaceKey(strings.Repeat("a", 64)), Generation(strings.Repeat("b", 64))}
	var canonical []string
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "canonical", true: "custom"}[custom], func(t *testing.T) {
			cfg := testConfig(filepath.Join(t.TempDir(), "state"))
			cfg.PublicDNS = []string{"::ffff:9.9.9.9", "2606:4700:4700::1111"}
			image := testImage
			if custom {
				image = "sha256:" + strings.Repeat("c", 64)
			}
			cli := &profileCLI{t: t, e: e, image: image}
			// The Config passed to Open is authoritative even if the factory supplied
			// different constructor DNS. Neither slice may mutate the admitted profile.
			initial := []string{"8.8.4.4"}
			p := NewPodman("/unused/podman", testImage, initial...)
			initial[0] = "127.0.0.1"
			if p.publicDNS[0] != "8.8.4.4" {
				t.Fatal("constructor did not copy")
			}
			p.runner = cli
			s, err := open(context.Background(), cfg, func() Runtime { return p }, filepath.Join(t.TempDir(), "lock", "daemon.lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			cfg.PublicDNS[0] = "127.0.0.1"
			if err := p.Create(context.Background(), e, image); err != nil {
				t.Fatal(err)
			}
			args := cli.create
			for _, required := range []string{"--network=pasta:--no-map-gw,-t,none,-u,none,-T,none,-U,none", "--image-volume=ignore", "--no-healthcheck", "--hooks-dir=" + filepath.Join(p.profileDir, "hooks"), "--default-mounts-file=" + filepath.Join(p.profileDir, "mounts.conf"), "--http-proxy=false", "--no-hosts", "--dns-search=.", "--publish-all=false", "--privileged=false", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--systemd=false", "--seccomp-policy=default", "--sdnotify=ignore", "--pid=private", "--ipc=private", "--uts=private", "--cgroupns=private"} {
				if !slices.Contains(args, required) {
					t.Errorf("missing %s", required)
				}
			}
			wantDNS := []string{"--dns=9.9.9.9", "--dns=2606:4700:4700::1111"}
			build := p.buildArgs(e, "/data/exact", "parent")
			for _, command := range [][]string{args, build} {
				var dns []string
				for _, a := range command {
					if strings.HasPrefix(a, "--dns=") {
						dns = append(dns, a)
					}
				}
				if !slices.Equal(dns, wantDNS) {
					t.Fatal("DNS not propagated immutably and in order", dns)
				}
			}
			if !slices.Contains(build, "--network=pasta:--no-map-gw,-t,none,-u,none,-T,none,-U,none,--hostname,"+objectName(e)) {
				t.Fatal("build attribution changed")
			}
			for _, a := range args {
				for _, forbidden := range []string{"--network=none", "--network=host", "--network=bridge", "--publish=", "--publish-all=true", "--add-host", "--device", "--volume", "--mount=", "--cap-add", "--security-opt=seccomp=unconfined", "--env", "--pod", "--hostname"} {
					if strings.HasPrefix(a, forbidden) {
						t.Fatal("runtime broadening", a)
					}
				}
			}
			// Compare the complete effective argv, ignoring only exact image/labels
			// and the per-service configuration location.
			normalized := slices.Clone(args)
			for i, a := range normalized {
				normalized[i] = strings.ReplaceAll(strings.ReplaceAll(a, p.profileDir, "PROFILE"), image, "IMAGE")
			}
			if custom {
				if !reflect.DeepEqual(normalized, canonical) {
					t.Fatal("canonical/custom profile diverged", normalized, canonical)
				}
			} else {
				canonical = normalized
			}
		})
	}
}

func TestNoCompiledDNSFallback(t *testing.T) {
	p := NewPodman("/unused/podman", testImage)
	if len(p.publicDNS) != 0 {
		t.Fatal("constructor supplied hidden defaults")
	}
	p.runner = &profileCLI{t: t}
	if err := p.Initialize(context.Background()); err == nil {
		t.Fatal("missing DNS initialized")
	}
	cfg := testConfig(filepath.Join(t.TempDir(), "state"))
	cfg.PublicDNS = nil
	called := false
	if _, err := open(context.Background(), cfg, func() Runtime { called = true; return p }, filepath.Join(t.TempDir(), "lock")); err == nil || called {
		t.Fatal("missing Config DNS reached runtime")
	}
}

func TestEmptyRuntimeProfileFailsClosed(t *testing.T) {
	for _, mode := range []string{"clean", "hook", "mount", "hook-symlink", "mount-symlink", "public"} {
		t.Run(mode, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "profile")
			if err := prepareRuntimeProfile(dir); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "hook":
				if err := os.WriteFile(filepath.Join(dir, "hooks", "hook.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "mount":
				if err := os.WriteFile(filepath.Join(dir, "mounts.conf"), []byte("/host:/host"), 0600); err != nil {
					t.Fatal(err)
				}
			case "hook-symlink":
				if err := os.Remove(filepath.Join(dir, "hooks")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), filepath.Join(dir, "hooks")); err != nil {
					t.Fatal(err)
				}
			case "mount-symlink":
				if err := os.Remove(filepath.Join(dir, "mounts.conf")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/dev/null", filepath.Join(dir, "mounts.conf")); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			err := prepareRuntimeProfile(dir)
			if (err == nil) != (mode == "clean") {
				t.Fatal("profile validation", err)
			}
		})
	}
}
