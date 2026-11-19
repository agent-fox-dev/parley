package webhook

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"

	"github.com/agent-fox-dev/parley/internal/server"
)

// errorTypeDestinationRefused answers, with 400, a create or a change of the
// url whose host is, or resolves to, an address the destination rules
// refuse.
const errorTypeDestinationRefused = "webhook_destination_refused"

// Resolver looks up the addresses of a host name; *net.Resolver is one.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// insideRanges are the addresses of the hub's own networks, which a
// subscription must not be able to reach: loopback, link-local (holding the
// cloud metadata address 169.254.169.254), private, unspecified, multicast
// and unique-local (holding fd00:ec2::254).
var insideRanges = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("0.0.0.0/32"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("ff00::/8"),
}

// destinationAllowed reports whether a request of a subscription whose url
// has the scheme may go to addr: an address outside the hub's own networks
// always, one inside them only when [webhooks].allow_private is set, or when
// [webhooks].allow_insecure is set, the scheme is http and addr is a
// loopback address. It is the one judge of a destination, at the create and
// change of a url and at every connection. An IPv4-mapped IPv6 address is
// judged by its IPv4 address, and an address that is not valid is refused.
func destinationAllowed(addr netip.Addr, scheme string, s Settings) bool {
	// A zoned address never matches a prefix, so the zone is dropped
	// before the ranges are looked at.
	addr = addr.WithZone("").Unmap()
	if !addr.IsValid() {
		return false
	}
	if s.AllowPrivate || !inside(addr) {
		return true
	}
	return s.AllowInsecure && scheme == "http" && addr.IsLoopback()
}

// inside reports whether addr, unmapped and without a zone, is in one of
// insideRanges.
func inside(addr netip.Addr) bool {
	for _, p := range insideRanges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// checkDestination answers 400 webhook_destination_refused when the host of
// u is an address destinationAllowed refuses, or is a name of which any
// resolved address is refused; the name localhost is judged as the loopback
// address it names. A name that does not resolve is accepted, so that a
// resolver outage does not block a configuration: its attempts fail with
// connection_failed, and every connection is judged again.
func (c *Component) checkDestination(ctx context.Context, u *url.URL) error {
	host := u.Hostname()
	var addrs []netip.Addr
	switch addr, err := netip.ParseAddr(host); {
	case err == nil:
		addrs = []netip.Addr{addr}
	case loopbackHost(host):
		addrs = []netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 1})}
	default:
		resolved, err := c.deps.Resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil
		}
		addrs = resolved
	}
	for _, addr := range addrs {
		if !destinationAllowed(addr, u.Scheme, c.deps.Settings) {
			return server.NewError(errorTypeDestinationRefused, http.StatusBadRequest,
				"url is refused: its host "+host+" is, or resolves to, an address of the hub's own networks, which [webhooks].allow_private allows")
		}
	}
	return nil
}
