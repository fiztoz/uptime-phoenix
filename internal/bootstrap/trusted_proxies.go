package bootstrap

import (
	"fmt"
	"net/netip"
	"strings"
)

func parseTrustedProxyCIDRs(raw string) ([]netip.Prefix, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var prefixes []netip.Prefix
	for index, entry := range strings.Split(raw, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(entry))
		if err != nil || prefix.Bits() == 0 || prefix.Addr().Is4In6() {
			// Do not echo arbitrary environment contents into startup logs.
			return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS: entry %d must be an IPv4 or IPv6 CIDR with a nonzero prefix", index+1)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}
