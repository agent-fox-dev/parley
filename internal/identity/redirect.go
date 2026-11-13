package identity

import (
	"net/url"
	"strings"
)

// allowedRedirect reports whether a login may send the provider the
// redirect URI uri. The provider sends the browser, with the code, to that
// URI, so only two kinds are allowed: the loopback listener of parley login
// (http on localhost or 127.0.0.1, any port and path) and a page of the hub
// itself (the scheme, host and port of externalURL, when it is set). A URI
// with user information is refused, because it is how a URL that reads as
// localhost names another host.
func allowedRedirect(uri, externalURL string) bool {
	u, err := url.Parse(uri)
	if err != nil || !u.IsAbs() || u.Opaque != "" || u.Host == "" || u.User != nil {
		return false
	}
	if u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || u.Hostname() == "127.0.0.1") {
		return true
	}
	if externalURL == "" {
		return false
	}
	hub, err := url.Parse(externalURL)
	if err != nil || hub.Host == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, hub.Scheme) && strings.EqualFold(u.Hostname(), hub.Hostname()) && port(u) == port(hub)
}

// port is u's port, or its scheme's default port when it names none.
func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}
