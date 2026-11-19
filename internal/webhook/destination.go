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
// cloud metadata address 169.254.169.254), private, the shared address space
// 100.64.0.0/10 that carrier-grade NAT and some cloud and container
// networks use inside, "this network" 0.0.0.0/8 (holding the unspecified
// address, which some systems connect to as the host itself), multicast,
// and IPv6's unspecified address and unique-local (holding fd00:ec2::254).
var insideRanges = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("0.0.0.0/8"),
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
// change of a url and at every connection. An IPv6 address that carries an
// IPv4 address (embeddedIPv4) is judged by that IPv4 address, and an address
// that is not valid is refused.
func destinationAllowed(addr netip.Addr, scheme string, s Settings) bool {
	// A zoned address never matches a prefix, so the zone is dropped
	// before the ranges are looked at.
	addr = embeddedIPv4(addr.WithZone(""))
	if !addr.IsValid() {
		return false
	}
	if s.AllowPrivate || !inside(addr) {
		return true
	}
	return s.AllowInsecure && scheme == "http" && addr.IsLoopback()
}

// The IPv6 prefixes whose addresses carry an IPv4 address, besides the
// IPv4-mapped ::ffff:0:0/96: IPv4-compatible addresses and the NAT64
// well-known prefix in their last 32 bits, which a translator sends on to
// that IPv4 address, and 6to4 addresses in bits 16 to 47, which a relay
// tunnels to it.
var (
	compatiblePrefix = netip.MustParsePrefix("::/96")
	nat64Prefix      = netip.MustParsePrefix("64:ff9b::/96")
	sixToFourPrefix  = netip.MustParsePrefix("2002::/16")
)

// embeddedIPv4 is the IPv4 address that the IPv6 address addr carries, and
// addr itself when it carries none, so that no IPv6 spelling of an address
// of the hub's networks escapes the ranges: an IPv4-mapped address, an
// IPv4-compatible one (::a.b.c.d other than :: and ::1, which are IPv6's
// own unspecified and loopback addresses), one of the NAT64 well-known
// prefix, and a 6to4 address, whose IPv4 address is bits 16 to 47.
func embeddedIPv4(addr netip.Addr) netip.Addr {
	if addr.Is4In6() {
		return addr.Unmap()
	}
	if !addr.Is6() {
		return addr
	}
	b := addr.As16()
	switch {
	case compatiblePrefix.Contains(addr) && addr != netip.IPv6Unspecified() && addr != netip.IPv6Loopback(),
		nat64Prefix.Contains(addr):
		return netip.AddrFrom4([4]byte(b[12:16]))
	case sixToFourPrefix.Contains(addr):
		return netip.AddrFrom4([4]byte(b[2:6]))
	}
	return addr
}

// inside reports whether addr, its embedded IPv4 address taken and without
// a zone, is in one of insideRanges.
func inside(addr netip.Addr) bool {
	for _, p := range insideRanges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// addressesOf is the addresses host stands for: a literal address itself,
// the name localhost the loopback address 127.0.0.1, and any other name the
// addresses r resolves it to.
func addressesOf(ctx context.Context, r Resolver, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	if loopbackHost(host) {
		return []netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 1})}, nil
	}
	return r.LookupNetIP(ctx, "ip", host)
}

// checkDestination answers 400 webhook_destination_refused when the host of
// u is an address destinationAllowed refuses, or is a name of which any
// resolved address is refused; the name localhost is judged as the loopback
// address it names. A name that does not resolve is accepted, so that a
// resolver outage does not block a configuration: its attempts fail with
// connection_failed, and every connection is judged again. The lookup is
// bounded by [webhooks].timeout, as an attempt is, so that a resolver that
// hangs holds no request: it counts as a name that does not resolve.
func (c *Component) checkDestination(ctx context.Context, u *url.URL) error {
	host := u.Hostname()
	if d := c.deps.Settings.Timeout; d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	addrs, err := addressesOf(ctx, c.deps.Resolver, host)
	if err != nil {
		return nil
	}
	for _, addr := range addrs {
		if !destinationAllowed(addr, u.Scheme, c.deps.Settings) {
			return server.NewError(errorTypeDestinationRefused, http.StatusBadRequest,
				"url is refused: its host "+host+" is, or resolves to, an address of the hub's own networks, which [webhooks].allow_private allows")
		}
	}
	return nil
}
