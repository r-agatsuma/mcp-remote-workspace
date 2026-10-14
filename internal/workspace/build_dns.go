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
		address, err := netip.ParseAddr(fields[1])
		address = address.Unmap()
		if err != nil || !address.IsGlobalUnicast() || address.IsPrivate() || address.Zone() != "" {
			return errors.New("public deployment DNS required")
		}
		for _, reserved := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32"} {
			if netip.MustParsePrefix(reserved).Contains(address) {
				return errors.New("public deployment DNS required")
			}
		}
		found = true
	}
	if scanner.Err() != nil || !found {
		return errors.New("public deployment DNS required")
	}
	return nil
}
