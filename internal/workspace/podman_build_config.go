package workspace

import (
	"errors"
	"net/netip"
	"path/filepath"
)

// PodmanBuildConfig is host-owned configuration, never Dockerfile/MCP input.
// DNS selects defaults only; it does not restrict public DNS/DoH contacted by RUN.
type PodmanBuildConfig struct {
	BuildahPath string
	DataDir     string
	DNS         []string
}

func (c PodmanBuildConfig) Validate() error {
	if c.BuildahPath != "" && !filepath.IsAbs(c.BuildahPath) || c.DataDir != "" && !filepath.IsAbs(c.DataDir) {
		return errors.New("absolute Buildah/Data paths required")
	}
	if len(c.DNS) > 8 {
		return errors.New("too many build default resolvers")
	}
	for _, value := range c.DNS {
		ip, err := netip.ParseAddr(value)
		if err != nil || ip.Zone() != "" || !publicResolver(ip.Unmap()) {
			return errors.New("build default resolver must be a public-routable IP address")
		}
	}
	return nil
}

// Reject non-public address classes in trusted defaults. Actual destination
// enforcement (including host/other-workspace addresses) remains the UID OUTPUT
// boundary; this validation adds no network or resolver authority.
func publicResolver(ip netip.Addr) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	denied := []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"}
	for _, prefix := range denied {
		if netip.MustParsePrefix(prefix).Contains(ip) {
			return false
		}
	}
	return ip.Is4() || netip.MustParsePrefix("2000::/3").Contains(ip)
}
