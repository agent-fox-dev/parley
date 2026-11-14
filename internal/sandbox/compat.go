package sandbox

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
)

// The OpenShell releases the hub works with: from LowestSupportedRelease,
// included, to FirstUnsupportedRelease, excluded, compared as semantic
// versions. They follow the OpenShell SDK in go.mod
// (github.com/ai-openshell/sdk-go v0.1.8): the lowest is that version's
// major.minor.0, and the first unsupported is the next major version, or
// the next minor version while the major version is 0. Moving the SDK to
// another minor version moves them.
const (
	LowestSupportedRelease  = "0.1.0"
	FirstUnsupportedRelease = "0.2.0"
)

// ReleaseRange is a range of OpenShell releases: from Lowest, included, to
// FirstUnsupported, excluded, both semantic versions.
type ReleaseRange struct {
	Lowest           string
	FirstUnsupported string
}

// SupportedReleases is the range of the releases the hub works with.
var SupportedReleases = ReleaseRange{Lowest: LowestSupportedRelease, FirstUnsupported: FirstUnsupportedRelease}

// String renders the range as ">=<lowest> <<first unsupported>".
func (r ReleaseRange) String() string { return ">=" + r.Lowest + " <" + r.FirstUnsupported }

// Contains reports whether release lies in the range, compared as semantic
// versions (semver.org 2.0.0) after an optional leading "v": a pre-release
// is below its release, and build metadata is ignored. A release that is
// not a semantic version, or a range bound that is not one, is outside.
func (r ReleaseRange) Contains(release string) bool {
	v, ok := parseVersion(strings.TrimPrefix(release, "v"))
	lo, okLo := parseVersion(r.Lowest)
	hi, okHi := parseVersion(r.FirstUnsupported)
	return ok && okLo && okHi && v.compare(lo) >= 0 && v.compare(hi) < 0
}

// version is a parsed semantic version.
type version struct {
	core [3]uint64
	pre  []string // nil for a release
}

// parseVersion parses MAJOR.MINOR.PATCH[-PRERELEASE][+BUILD] strictly: no
// leading zero in a number, no empty identifier.
func parseVersion(s string) (version, bool) {
	var v version
	if i := strings.IndexByte(s, '+'); i >= 0 {
		if !identifiers(s[i+1:], false) {
			return v, false
		}
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		if !identifiers(s[i+1:], true) {
			return v, false
		}
		v.pre = strings.Split(s[i+1:], ".")
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, ok := number(p)
		if !ok {
			return v, false
		}
		v.core[i] = n
	}
	return v, true
}

// identifiers reports whether s is a dot-separated list of non-empty
// identifiers of letters, digits and '-'; in a pre-release a numeric one has
// no leading zero.
func identifiers(s string, pre bool) bool {
	for id := range strings.SplitSeq(s, ".") {
		if id == "" {
			return false
		}
		numeric := true
		for _, c := range id {
			switch {
			case c >= '0' && c <= '9':
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-':
				numeric = false
			default:
				return false
			}
		}
		if pre && numeric && len(id) > 1 && id[0] == '0' {
			return false
		}
	}
	return true
}

// number parses a numeric identifier: digits only, no leading zero.
func number(s string) (uint64, bool) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

// compare returns -1, 0 or 1 as v is below, equal to or above w, in the
// precedence of semantic versions.
func (v version) compare(w version) int {
	if c := slices.Compare(v.core[:], w.core[:]); c != 0 {
		return c
	}
	switch {
	case v.pre == nil && w.pre == nil:
		return 0
	case v.pre == nil:
		return 1
	case w.pre == nil:
		return -1
	}
	for i := 0; i < len(v.pre) && i < len(w.pre); i++ {
		if c := compareIdentifier(v.pre[i], w.pre[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(v.pre), len(w.pre))
}

// compareIdentifier orders two pre-release identifiers: numeric ones by
// value and below alphanumeric ones, alphanumeric ones in ASCII order.
func compareIdentifier(a, b string) int {
	na, aNum := number(a)
	nb, bNum := number(b)
	switch {
	case aNum && bNum:
		return cmp.Compare(na, nb)
	case aNum:
		return -1
	case bNum:
		return 1
	}
	return strings.Compare(a, b)
}
