package identity

import "net/url"

// defaultPort fills in a scheme's default port.
func defaultPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// allowedRedirect reports whether a login may send the provider back to
// raw: an absolute URL without user information that is http on localhost
// or 127.0.0.1 (any port and path), or whose scheme, host and port equal
// those of [server].external_url when that is set.
func allowedRedirect(raw, externalURL string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return false
	}
	if u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1") {
		return true
	}
	if externalURL == "" {
		return false
	}
	e, err := url.Parse(externalURL)
	if err != nil || e.Host == "" {
		return false
	}
	return u.Scheme == e.Scheme && u.Hostname() == e.Hostname() && defaultPort(u) == defaultPort(e)
}
