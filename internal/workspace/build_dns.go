package workspace

import (
	"bufio"
	"errors"
	"io"
	"net/netip"
	"os"
	"strings"
)

// Podman's host-side image fetch uses the deployment's /etc/resolv.conf. The
// --dns build options configure RUN only. Neither path grants a host/private DNS
// exception, and this check does not restrict untrusted public DNS/DoH traffic.
func buildHostDNS() error {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return errors.New("deployment resolver configuration exceeds inspection bound")
	}
	return publicResolvers(strings.NewReader(string(data)))
}

func publicResolvers(r io.Reader) error {
	found := false
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != "nameserver" {
			continue
		}
		if len(fields) < 2 {
			return errors.New("public deployment DNS required")
		}
		if _, err := publicResolver(fields[1]); err != nil {
			return err
		}
		found = true
	}
	if scanner.Err() != nil || !found {
		return errors.New("public deployment DNS required")
	}
	return nil
}

// MaxPublicDNS bounds deployment input and matches conventional resolver limits.
const MaxPublicDNS = 3

// validatedPublicDNS returns a fresh, normalized slice in operator order.
func validatedPublicDNS(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > MaxPublicDNS {
		return nil, errors.New("configure between one and three public DNS IP addresses")
	}
	result := make([]string, len(values))
	for i, value := range values {
		address, err := publicResolver(value)
		if err != nil {
			return nil, err
		}
		result[i] = address.String()
	}
	return result, nil
}

func publicResolver(value string) (netip.Addr, error) {
	address, err := netip.ParseAddr(value)
	// Check the zone before Unmap, which can discard it for mapped addresses.
	if err != nil || address.Zone() != "" {
		return netip.Addr{}, errors.New("public deployment DNS literal IP required")
	}
	address = address.Unmap()
	// Native IPv6 resolver defaults must be in global unicast space; this
	// also excludes deprecated site-local, discard and translation-local space.
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.Is6() && !netip.MustParsePrefix("2000::/3").Contains(address) {
		return netip.Addr{}, errors.New("public deployment DNS required")
	}
	for _, reserved := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001::/32", "2001:2::/48", "2001:10::/28", "2001:20::/28", "2001:db8::/32", "2002::/16", "3fff::/20"} {
		if netip.MustParsePrefix(reserved).Contains(address) {
			return netip.Addr{}, errors.New("public deployment DNS required")
		}
	}
	return address, nil
}
