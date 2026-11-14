package cli

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// newCredentialHelperCommand builds parley credential-helper <verb>: git's
// credential helper protocol over the client's own settings, so that git
// fetches and pushes to the hub's plain URLs with the hub credential. It
// makes no request to the hub. Its auth is none: a command of auth user would
// fail on missing settings with an error on stderr, where git must see no
// output and go on to its next helper.
func newCredentialHelperCommand() *cobra.Command {
	return declare(command{
		Name:        "credential-helper",
		Description: "Give git the hub credential for the hub's own URL (git's credential helper protocol)",
		Args:        []argSpec{{Name: "verb", Required: true}},
		Auth:        authNone,
		Run:         credentialHelper,
	})
}

// credentialHelper answers git's get with the credential when the request
// names the hub's scheme, host and port, and with nothing otherwise. Every
// other verb, store and erase among them, does nothing: the credential lives
// in the client's settings, not in git. No failure is an error, because an
// error would stop git where it should try its next helper.
func credentialHelper(c *call) error {
	if c.args["verb"] != "get" {
		return nil
	}
	protocol, host := readCredentialRequest(c.opts.stdin)
	if protocol == "" || host == "" {
		return nil
	}
	// resolveSettings refuses a missing setting, a file it cannot read and a
	// hub URL that is not http or https with a host; each is no answer here.
	s, err := resolveSettings(c.cmd)
	if err != nil {
		return nil
	}
	if !isHubOrigin(s.hubURL, protocol, host) {
		return nil
	}
	_, err = fmt.Fprintf(c.stdout, "protocol=%s\nhost=%s\nusername=x-token-auth\npassword=%s\n", protocol, host, s.credential)
	return err
}

// readCredentialRequest reads git's request, key=value lines up to the first
// empty line or the end of r, and returns the values of protocol and host,
// each the text after the line's first '='. Lines without '=' and every other
// key are ignored; of a key given twice the last value counts.
func readCredentialRequest(r io.Reader) (protocol, host string) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			return protocol, host
		}
		switch key, value, _ := strings.Cut(line, "="); key {
		case "protocol":
			protocol = value
		case "host":
			host = value
		}
		if err != nil {
			return protocol, host
		}
	}
}

// isHubOrigin tells whether protocol and host, as git gives them, name the
// origin of hubURL: the same scheme, the same host name without regard to
// case and the same port, a missing port being the scheme's default. Neither
// path counts, so the credential reaches every repository of the hub and no
// other host.
func isHubOrigin(hubURL, protocol, host string) bool {
	hub, err := url.Parse(hubURL)
	if err != nil || protocol != hub.Scheme {
		return false
	}
	asked := &url.URL{Host: host}
	return strings.EqualFold(asked.Hostname(), hub.Hostname()) && portOf(asked, hub.Scheme) == portOf(hub, hub.Scheme)
}

// portOf is u's port, or the default port of scheme when u has none.
func portOf(u *url.URL, scheme string) string {
	if port := u.Port(); port != "" {
		return port
	}
	if scheme == "http" {
		return "80"
	}
	return "443"
}
