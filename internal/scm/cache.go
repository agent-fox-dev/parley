package scm

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"strings"
)

// The Cache-Control values of a successful read. An answer computed from
// commits the caller named by their full ids alone cannot change, so it may
// be kept for good; any other may change when a ref moves, so it is kept
// only to be revalidated with its entity tag.
const (
	cacheImmutable  = "private, max-age=31536000, immutable"
	cacheRevalidate = "private, no-cache"
)

// The representations of a read that answers in two forms, for its entity
// tag.
const (
	reprJSON  = "json"
	reprRaw   = "raw"
	reprPatch = "patch"
)

// notModified gives the answer of a successful read its caching headers
// and reports whether the caller's copy is current, in which case it has
// answered 304 with no body. A read calls it once what the answer is
// computed from is known (its resolved commit ids, and the representation
// where it has two) and before the costly part of the read, so that a
// current copy costs no diff, blame or search. The entity tag digests the
// request's path and query with parts; immutable is set when every
// revision of the request was written as a full commit id.
func notModified(w http.ResponseWriter, r *http.Request, immutable bool, parts ...string) bool {
	tag := entityTag(r, parts)
	h := w.Header()
	h.Set("ETag", tag)
	if immutable {
		h.Set("Cache-Control", cacheImmutable)
	} else {
		h.Set("Cache-Control", cacheRevalidate)
	}
	if !tagListed(r.Header.Values("If-None-Match"), tag) {
		return false
	}
	w.WriteHeader(http.StatusNotModified)
	return true
}

// entityTag is the quoted digest of the request's path (the route, the
// workspace and any path parameter), its query (a list's cursor included,
// so each page has its own tag) and parts. Each value is written with its
// length, so that two different lists of values never digest the same
// bytes.
func entityTag(r *http.Request, parts []string) string {
	d := sha256.New()
	for _, p := range append([]string{r.URL.EscapedPath(), r.URL.RawQuery}, parts...) {
		_ = binary.Write(d, binary.BigEndian, uint64(len(p)))
		d.Write([]byte(p))
	}
	return `"` + base64.RawURLEncoding.EncodeToString(d.Sum(nil)) + `"`
}

// tagListed reports whether the If-None-Match fields hold tag, alone or in
// a comma-separated list, compared weakly (a W/ prefix is ignored). A *
// matches nothing here: a read decides before it knows that a path exists,
// and a tag the caller holds came from an answer that was served.
func tagListed(fields []string, tag string) bool {
	for _, f := range fields {
		for t := range strings.SplitSeq(f, ",") {
			t = strings.TrimSpace(t)
			if strings.TrimPrefix(t, "W/") == tag {
				return true
			}
		}
	}
	return false
}

// fullID reports whether rev, a revision as the caller wrote it, is a full
// commit id.
func fullID(rev string) bool { return len(rev) == commitIDLen && isCommitID(rev) }

// uncached is what restores the caching headers of h, as they are before a
// read's handler runs, for an error answer: no entity tag, and the
// Cache-Control and Vary the server set for every answer.
func uncached(h http.Header) func() {
	before := map[string][]string{}
	for _, name := range []string{"Cache-Control", "Vary"} {
		before[name] = h.Values(name)
	}
	return func() {
		h.Del("ETag")
		for name, values := range before {
			h.Del(name)
			for _, v := range values {
				h.Add(name, v)
			}
		}
	}
}
