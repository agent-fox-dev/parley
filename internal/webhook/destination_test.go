package webhook

import (
	"context"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

func TestDestinationAllowed_JudgesTheAddressForTheSchemeAndSettings(t *testing.T) {
	t.Parallel()
	// inside are addresses of the hub's own networks: loopback,
	// link-local with the metadata address, private, the shared address
	// space, this network, unspecified, multicast and unique-local, over
	// IPv4, IPv6 and the IPv6 forms that carry an IPv4 address:
	// IPv4-mapped, IPv4-compatible, NAT64 and 6to4.
	inside := []string{
		"127.0.0.1", "127.8.9.10", "127.255.255.255", "169.254.1.1", "169.254.169.254", "10.0.0.1", "10.255.255.255",
		"172.16.0.1", "172.31.255.255", "192.168.1.1", "0.0.0.0", "224.0.0.1", "239.255.255.255",
		"0.1.2.3", "0.255.255.255", "100.64.0.1", "100.127.255.255", "::ffff:100.64.0.1", "64:ff9b::6440:1",
		"::1", "fe80::1", "fe80::1%en0", "febf::1", "fc00::1", "fd00:ec2::254", "::", "ff02::1",
		"::ffff:10.0.0.1", "::ffff:127.0.0.1", "::ffff:169.254.169.254",
		"::10.0.0.1", "::169.254.169.254", "::192.168.1.1",
		"64:ff9b::a00:1", "64:ff9b::169.254.169.254", "64:ff9b::7f00:1",
		"2002:a00:1::1", "2002:a9fe:a9fe::", "2002:c0a8:101:1::1",
	}
	outside := []string{
		"203.0.113.10", "172.32.0.1", "172.15.255.255", "9.255.255.255", "11.0.0.1", "126.255.255.255", "128.0.0.1",
		"169.255.0.1", "192.169.0.1", "223.255.255.255", "1.0.0.1", "100.63.255.255", "100.128.0.0",
		"2001:db8::1", "fec0::1", "::ffff:203.0.113.10",
		"::203.0.113.10", "64:ff9b::cb00:710a", "2002:cb00:710a::1",
	}
	loopback := []string{"127.0.0.1", "127.8.9.10", "::1", "::ffff:127.0.0.1"}
	otherInside := []string{"10.0.0.1", "169.254.169.254", "192.168.1.1", "0.0.0.0", "0.1.2.3", "100.64.0.1", "fc00::1", "fe80::1", "ff02::1"}

	for _, tc := range []struct {
		name     string
		settings Settings
		scheme   string
		addrs    []string
		want     bool
	}{
		{"an https url to an inside address by default", Settings{}, "https", inside, false},
		{"an http url to an inside address by default", Settings{}, "http", inside, false},
		{"an https url to an outside address by default", Settings{}, "https", outside, true},
		{"an https url to an inside address with allow_private", Settings{AllowPrivate: true}, "https", inside, true},
		{"an http url to an inside address with allow_private", Settings{AllowPrivate: true}, "http", inside, true},
		{"an http url to a loopback address with allow_insecure", Settings{AllowInsecure: true}, "http", loopback, true},
		{"an https url to a loopback address with allow_insecure", Settings{AllowInsecure: true}, "https", loopback, false},
		{"an http url to another inside address with allow_insecure", Settings{AllowInsecure: true}, "http", otherInside, false},
		{"an https url to an outside address with both settings", Settings{AllowPrivate: true, AllowInsecure: true}, "https", outside, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, a := range tc.addrs {
				addr := netip.MustParseAddr(a)
				if got := destinationAllowed(addr, tc.scheme, tc.settings); got != tc.want {
					t.Errorf("destinationAllowed(%s, %s, %+v) = %t, want %t", a, tc.scheme, tc.settings, got, tc.want)
				}
			}
		})
	}
	if destinationAllowed(netip.Addr{}, "https", Settings{}) {
		t.Errorf("destinationAllowed allows the zero address")
	}
}

// hangingResolver answers no lookup until the lookup's context ends.
type hangingResolver struct{}

func (hangingResolver) LookupNetIP(ctx context.Context, _, _ string) ([]netip.Addr, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCheckDestination_AcceptsAHostWhoseLookupOutlastsTheTimeout(t *testing.T) {
	t.Parallel()
	c := &Component{}
	c.Bind(Deps{Settings: Settings{Timeout: time.Second}, Resolver: hangingResolver{}})
	u, _ := url.Parse("https://slow.example/hook")
	done := make(chan error, 1)
	go func() { done <- c.checkDestination(context.Background(), u) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("checkDestination = %v, want the url accepted as a name that does not resolve", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("checkDestination still waits for the resolver 5 s later, with a timeout of 1 s")
	}
}
