// Package scrub removes credentials from text before the hub emits it in a
// log line, an event, an audit record or an error message. It is a leaf
// package: it imports nothing of the project, so every package can use it.
package scrub

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Redacted is the marker that takes the place of a removed secret.
const Redacted = "[redacted]"

// MaxLength is the longest scrubbed text, in bytes.
const MaxLength = 1024

var (
	// credentials are the hub's own credential formats, recognised by shape
	// because the hub keeps only their hashes: admin tokens, personal
	// access tokens and API keys.
	credentials = regexp.MustCompile(`parley_admin_[0-9a-f]{64}|parley_pat_[a-z0-9]{8}_[a-z0-9]{32}|parley_[0-9A-Za-z]{8}_[0-9A-Za-z]{32}`)
	// userinfo is the user information of a URL of any scheme: everything
	// between :// and the last @ before the next /, whitespace or end of
	// text, with that @.
	userinfo = regexp.MustCompile(`://[^/\s]*@`)
)

// Scrubber removes credentials from text. It is safe for concurrent use.
type Scrubber struct {
	mu     sync.RWMutex
	values map[string]bool
}

// New returns a scrubber that knows no secret value yet; it still removes
// the hub's credential formats and URL user information.
func New() *Scrubber { return &Scrubber{values: map[string]bool{}} }

// Add registers a secret value for the life of the scrubber. An empty value
// and a value that is part of the marker [redacted] are ignored.
func (s *Scrubber) Add(value string) {
	if ignored(value) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[value] = true
}

func ignored(v string) bool { return v == "" || strings.Contains(Redacted, v) }

// Scrub returns text without credentials. In this order it replaces every
// registered value and every value passed with this call by [redacted],
// longer values first; replaces the hub's credential formats; removes URL
// user information; looks again and returns the marker alone when a secret
// is still there; and cuts the result to at most 1024 bytes on a character
// boundary. Scrubbing the result again changes nothing.
func (s *Scrubber) Scrub(text string, values ...string) string {
	all := s.secrets(values)
	out := text
	for _, v := range all {
		out = strings.ReplaceAll(out, v, Redacted)
	}
	out = credentials.ReplaceAllString(out, Redacted)
	out = userinfo.ReplaceAllString(out, "://")
	if holdsSecret(out, all) {
		return Redacted
	}
	return cut(out)
}

// secrets is the registered values and the call's own, ignored ones left
// out, longest first.
func (s *Scrubber) secrets(extra []string) []string {
	s.mu.RLock()
	set := make(map[string]bool, len(s.values)+len(extra))
	for v := range s.values {
		set[v] = true
	}
	s.mu.RUnlock()
	for _, v := range extra {
		if !ignored(v) {
			set[v] = true
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// holdsSecret is the second look: a value, a hub credential or URL user
// information that the replacements left or made.
func holdsSecret(text string, values []string) bool {
	for _, v := range values {
		if strings.Contains(text, v) {
			return true
		}
	}
	return credentials.MatchString(text) || userinfo.MatchString(text)
}

// cut returns the longest prefix of at most MaxLength bytes that ends on a
// character boundary.
func cut(text string) string {
	if len(text) <= MaxLength {
		return text
	}
	end := MaxLength
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}
