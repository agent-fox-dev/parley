// Package workspace holds what the hub knows about a workspace: its record,
// the rules every workspace route judges a slug, a URL, a branch name and a
// duration by, the guard that answers for a workspace that cannot serve a
// route needing its repository, and the work that creates a repository
// workspace and clones its origin into the hub's own bare copy.
package workspace

import (
	"errors"
	"math"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The validators return errors without the name of the member they judged, so
// that a route can answer "<member>: <reason>" for whichever member it read.

// ValidateSlug returns nil when s can name a repository workspace, and
// otherwise an error saying which rule s breaks. A slug is also a directory
// name on disk and a path segment of every workspace URL, hence the narrow
// alphabet.
func ValidateSlug(s string) error {
	return checkName(s, 3, 64, true)
}

// ValidateForkName returns nil when s can name a task fork, and otherwise an
// error saying which rule s breaks. Unlike a slug, a fork name may start with
// a digit, because it is only ever a part of the fork's slug.
func ValidateForkName(s string) error {
	return checkName(s, 1, 32, false)
}

// checkName applies the rule shared by slugs and fork names. Lengths count
// code points, so that a non-ASCII letter is refused by the alphabet rule and
// not mistaken for a length problem.
func checkName(s string, minLen, maxLen int, letterFirst bool) error {
	if n := utf8.RuneCountInString(s); n < minLen || n > maxLen {
		return errors.New("must be " + strconv.Itoa(minLen) + " to " + strconv.Itoa(maxLen) + " characters long")
	}
	for _, r := range s {
		if !isLowerAlnum(r) && r != '-' {
			return errors.New("may hold only the letters a to z, digits and hyphens")
		}
	}
	first, last := rune(s[0]), rune(s[len(s)-1])
	switch {
	case letterFirst && !(first >= 'a' && first <= 'z'):
		return errors.New("must start with a letter")
	case !letterFirst && first == '-':
		return errors.New("must start with a letter or digit")
	case last == '-':
		return errors.New("must end with a letter or digit")
	case strings.Contains(s, "--"):
		return errors.New("must not contain two hyphens in a row")
	}
	return nil
}

func isLowerAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

// ValidateGitURL returns nil when s is an origin the hub accepts, and
// otherwise an error saying which rule s breaks. It decides on the text
// alone: no name is resolved and no connection is opened, so a create request
// is refused for its URL before anything leaves the hub. An https URL with
// user information is refused, because a credential in the URL would be
// stored and shown wherever the URL is.
func ValidateGitURL(s string) error {
	if strings.ContainsFunc(s, func(r rune) bool { return r == ' ' || unicode.IsControl(r) }) {
		return errors.New("must not hold a space or control character")
	}
	if rest, ok := strings.CutPrefix(s, "https://"); ok {
		authority, path, _ := strings.Cut(rest, "/")
		switch {
		case strings.Contains(authority, "@"):
			return errors.New("must not hold user information")
		case path == "":
			return errors.New("must have a path after the host")
		}
		return checkAuthority(authority)
	}
	if rest, ok := strings.CutPrefix(s, "git@"); ok {
		host, path, _ := strings.Cut(rest, ":")
		switch {
		case host == "" || strings.ContainsAny(host, "/@"):
			return errors.New("must have a host between git@ and the colon")
		case path == "":
			return errors.New("must have a path after the colon")
		}
		return nil
	}
	return errors.New("must have the form https://<host>/<path> or git@<host>:<path>")
}

// checkAuthority accepts a host with an optional decimal port; a bracketed
// IPv6 address is a host.
func checkAuthority(authority string) error {
	host := authority
	if strings.LastIndex(authority, ":") > strings.LastIndex(authority, "]") {
		h, port, err := net.SplitHostPort(authority)
		if err != nil || port == "" || strings.Trim(port, "0123456789") != "" {
			return errors.New("must have a decimal port after the colon of the host")
		}
		host = h
	}
	if strings.Trim(host, "[]") == "" {
		return errors.New("must have a host")
	}
	return nil
}

// ValidateBranchName returns nil when s is a branch name the hub accepts, and
// otherwise an error saying which rule s breaks. The rule is git's own
// ref-name rule for a branch, so that every name the hub accepts is one git
// can create.
func ValidateBranchName(s string) error {
	switch {
	case s == "":
		return errors.New("must not be empty")
	case strings.ContainsFunc(s, unicode.IsControl):
		return errors.New("must not hold a control character")
	case strings.ContainsAny(s, ` ~^:?*[\`):
		return errors.New(`must not hold a space or any of ~ ^ : ? * [ \`)
	case strings.Contains(s, "..") || strings.Contains(s, "//") || strings.Contains(s, "@{"):
		return errors.New("must not hold '..', '//' or '@{'")
	case s == "@":
		return errors.New("must not be '@'")
	case strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/"):
		return errors.New("must not start with '-' or '/'")
	case strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock"):
		return errors.New("must not end with '/', '.' or '.lock'")
	}
	for component := range strings.SplitSeq(s, "/") {
		if strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return errors.New("no path component may start with '.' or end with '.lock'")
		}
	}
	return nil
}

// durationUnits are the units a duration may name. Seconds are left out on
// purpose: no workspace lifetime is that short.
var durationUnits = map[byte]time.Duration{
	'm': time.Minute,
	'h': time.Hour,
	'd': 24 * time.Hour,
}

// ParseDuration returns the length of time s names, s being a whole number
// above zero in decimal digits followed by exactly one unit m, h or d (a day
// is 24 hours), or an error saying which rule s breaks.
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, errors.New("must be a whole number followed by m, h or d")
	}
	unit, ok := durationUnits[s[len(s)-1]]
	if !ok {
		return 0, errors.New("must end in one unit: m, h or d")
	}
	digits := s[:len(s)-1]
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, errors.New("must be a whole number in decimal digits followed by m, h or d")
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n > math.MaxInt64/int64(unit) {
		return 0, errors.New("is too long a duration")
	}
	if n == 0 {
		return 0, errors.New("must be above zero")
	}
	return time.Duration(n) * unit, nil
}
