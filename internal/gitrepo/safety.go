package gitrepo

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// userinfo matches the user information of every URL of a text: what lies
// between a scheme's "://" and an "@" before the URL's path, query or end.
var userinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)[^/?#\s'"]*@`)

// SanitizeGitText is text, the output or error text of a git command, made
// fit to show to a client or to log: every non-empty value of secrets, such
// as the credential the command presented, is removed, and then the user
// information of every URL in it reads ***@. Longer values are removed
// first, so that one that holds another goes whole. The values go first
// because one may hold a character that ends a URL's user information
// early, such as a '/' in a password, which would keep the rest of it, the
// user name, from reading ***@; so cleaned, the text is the same when it
// is cleaned again. ssh's user@host:path form and a mail address are no
// URLs and are kept.
func SanitizeGitText(text string, secrets ...string) string {
	sorted := slices.SortedFunc(slices.Values(secrets), func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, s := range sorted {
		if s != "" {
			text = strings.ReplaceAll(text, s, "")
		}
	}
	return userinfo.ReplaceAllString(text, "${1}***@")
}

// ErrInvalidArgument is matched by the error of an operation given a
// branch name, ref name or revision that begins with '-', which git would
// read as an option: no git process is started for it. No branch name a
// caller may create and no commit id begins with '-', so nothing that
// names one is refused.
var ErrInvalidArgument = errors.New("invalid argument")

// refuseOption is nil when no value of values begins with '-', and an
// error matching ErrInvalidArgument that names the first that does, as a
// value of the kind kind (branch, ref, revision), otherwise. An operation
// that passes a value a caller gave to git checks it with this before any
// process starts, and passes it where git reads no option: after
// --end-of-options, or inside a full ref name.
func refuseOption(kind string, values ...string) error {
	for _, v := range values {
		if strings.HasPrefix(v, "-") {
			return fmt.Errorf("the %s %q: %w: it begins with '-', which git would read as an option", kind, v, ErrInvalidArgument)
		}
	}
	return nil
}

// ErrTransportRefused is matched by the error of an operation with a remote
// whose URL names a transport other than https or ssh: no git process is
// started for it.
var ErrTransportRefused = errors.New("transport refused")

// allowedProtocols are the transports git may use for an operation with a
// remote, as GIT_ALLOW_PROTOCOL lists them. Git holds a redirect, a remote
// helper and every transport it starts on its own to them too, which a
// check of the URL alone cannot.
const allowedProtocols = "https:ssh"

// CheckTransport is nil for a URL the hub may contact a remote at: an
// https:// or ssh:// URL, or ssh's [user@]host:path form. Every other form
// is refused with an error that matches ErrTransportRefused and names the
// transport git would use: another scheme (http, git, file, ...), a remote
// helper (<helper>::<address>, such as ext::, whatever the helper), or a
// local path, which git reads as a file URL. It reads the URL as git does
// and does not contact anything.
func CheckTransport(url string) error {
	transport, helper := remoteTransport(url)
	switch {
	case helper:
		return fmt.Errorf("%w: the remote is reached through the remote helper %q; the hub contacts a remote over https or ssh only", ErrTransportRefused, transport)
	case transport == "ssh" && !validSSHHost(url):
		return fmt.Errorf("%w: the address of transport %q names no host ssh may connect to", ErrTransportRefused, transport)
	case transport == "https" || transport == "ssh":
		return nil
	}
	return fmt.Errorf("%w: the remote's transport is %q; the hub contacts a remote over https or ssh only", ErrTransportRefused, transport)
}

// remoteTransport is the transport git uses for url, lower case: the name
// of a remote helper before "::", with helper set, or of a scheme before
// "://", "ssh" for the [user@]host:path form, which has a ':' before any
// '/', and "file" for a local path.
func remoteTransport(url string) (transport string, helper bool) {
	n := 0
	for n < len(url) && isSchemeChar(url[n], n == 0) {
		n++
	}
	switch {
	case n > 0 && strings.HasPrefix(url[n:], "::"):
		return strings.ToLower(url[:n]), true
	case n > 0 && strings.HasPrefix(url[n:], "://"):
		return strings.ToLower(url[:n]), false
	}
	colon, slash := strings.IndexByte(url, ':'), strings.IndexByte(url, '/')
	if colon > 0 && (slash < 0 || colon < slash) {
		return "ssh", false
	}
	return "file", false
}

// isSchemeChar reports whether c may be in the name of a URL scheme or a
// remote helper, at its start when first.
func isSchemeChar(c byte, first bool) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		return true
	case first:
		return false
	}
	return '0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'
}

// validSSHHost reports whether the ssh URL or [user@]host:path address
// url names a host, and neither it nor the user begins with '-', which
// ssh would read as an option.
func validSSHHost(url string) bool {
	addr := url
	if i := strings.Index(addr, "://"); i >= 0 {
		addr, _, _ = strings.Cut(addr[i+3:], "/")
	}
	host := addr
	if at := strings.IndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	end := ":"
	if inBrackets, ok := strings.CutPrefix(host, "["); ok {
		host, end = inBrackets, "]"
	}
	return !strings.HasPrefix(addr, "-") && host != "" && !strings.HasPrefix(host, "-") && !strings.HasPrefix(host, end)
}
