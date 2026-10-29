package http

import (
	"net"
	"net/netip"

	"github.com/labstack/echo/v4"
)

// clientIPExtractor never implicitly trusts private, loopback or link-local
// clients. A trusted proxy must append the actual peer to X-Forwarded-For (or
// replace the header); extraction stops at the nearest non-proxy address.
func clientIPExtractor(prefixes []netip.Prefix) echo.IPExtractor {
	if len(prefixes) == 0 {
		return echo.ExtractIPDirect()
	}
	options := []echo.TrustOption{
		echo.TrustLoopback(false),
		echo.TrustLinkLocal(false),
		echo.TrustPrivateNet(false),
	}
	for _, prefix := range prefixes {
		// NewRouter also serves programmatic callers that bypass bootstrap.
		// A malformed allow-list must never broaden the trust boundary.
		if !prefix.IsValid() || prefix.Bits() == 0 || prefix.Addr().Is4In6() {
			return echo.ExtractIPDirect()
		}
		prefix = prefix.Masked()
		options = append(options, echo.TrustIPRange(&net.IPNet{
			IP:   prefix.Addr().AsSlice(),
			Mask: net.CIDRMask(prefix.Bits(), prefix.Addr().BitLen()),
		}))
	}
	return echo.ExtractIPFromXFFHeader(options...)
}
