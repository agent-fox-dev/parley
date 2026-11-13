// Package scrub removes credentials from text before it leaves the hub in a
// log line, an event or an error message.
//
// A Scrubber knows a secret in two ways: values registered with Add for the
// life of the process, or passed to one Scrub call, and the hub's own
// credential formats, which it recognises by shape because the hub keeps only
// their hashes. Scrub also removes the user information from URLs and cuts
// the result to MaxLen bytes.
//
// The package imports nothing of the project, so that every package can use
// it without an import cycle.
package scrub

import (
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	// Marker stands in the place of every secret Scrub replaces, and is the
	// whole result when a secret cannot be removed cleanly.
	Marker = "[redacted]"

	// MaxLen is the longest result Scrub returns, in bytes.
	MaxLen = 1024
)

// credential matches the hub's own credential formats: an admin token, a
// personal access token and an API key. A word that only begins like one is
// not a match; a full credential is, whatever characters stand around it,
// because those characters do not make it less of a credential.
var credential = regexp.MustCompile(
	`parley_admin_[0-9a-f]{64}` +
		`|parley_pat_[a-z0-9]{8}_[a-z0-9]{32}` +
		`|parley_[0-9A-Za-z]{8}_[0-9A-Za-z]{32}`)

// Scrubber removes credentials from text. Its methods are safe for
// concurrent use; the zero value is not ready, use New.
type Scrubber struct {
	mu     sync.RWMutex
	values map[string]struct{}
	sorted []string // the values, longest first
}

// New returns a Scrubber that knows no secret value yet.
func New() *Scrubber {
	return &Scrubber{values: map[string]struct{}{}}
}

// Add registers a secret value for the life of the process. An empty value,
// and a value that is a part of Marker, are ignored.
func (s *Scrubber) Add(value string) {
	if ignored(value) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.values[value]; ok {
		return
	}
	s.values[value] = struct{}{}
	s.sorted = sortValues(append(slices.Clone(s.sorted), value))
}

// Scrub returns text without its credentials. It replaces every registered
// value and every value of perCall by Marker, longer values first, then every
// credential in one of the hub's formats, and removes the user information
// from every URL. If the result still holds any of those, Scrub returns Marker
// alone. Last, it cuts the result to the longest prefix of at most MaxLen
// bytes that ends on a UTF-8 character boundary.
//
// The order is fixed so that scrubbing a scrubbed text changes nothing: a
// result other than Marker holds none of the secrets, and no prefix of such a
// text holds one either.
func (s *Scrubber) Scrub(text string, perCall ...string) string {
	values := s.valuesFor(perCall)
	for _, v := range values {
		text = strings.ReplaceAll(text, v, Marker)
	}
	text = credential.ReplaceAllLiteralString(text, Marker)
	text = removeUserinfo(text)
	if holdsSecret(text, values) {
		return Marker
	}
	return cut(text)
}

// valuesFor returns the registered values and those of one call, without the
// ignored ones, longest first.
func (s *Scrubber) valuesFor(perCall []string) []string {
	s.mu.RLock()
	registered := s.sorted
	s.mu.RUnlock()
	if len(perCall) == 0 {
		return registered
	}
	values := slices.Clone(registered)
	for _, v := range perCall {
		if !ignored(v) {
			values = append(values, v)
		}
	}
	return sortValues(values)
}

func ignored(value string) bool {
	return value == "" || strings.Contains(Marker, value)
}

// sortValues orders values longest first, and values of one length by their
// bytes, so that the replacements do not depend on the order of registration.
func sortValues(values []string) []string {
	slices.SortFunc(values, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	return slices.Compact(values)
}

func holdsSecret(text string, values []string) bool {
	for _, v := range values {
		if strings.Contains(text, v) {
			return true
		}
	}
	return credential.MatchString(text) || userinfoAt(text, 0) >= 0
}

// userinfoAt returns the index of the first "://" at or after from whose
// authority holds user information, or -1.
func userinfoAt(text string, from int) int {
	for {
		i := strings.Index(text[from:], "://")
		if i < 0 {
			return -1
		}
		i += from
		if _, at := authority(text, i+3); at >= 0 {
			return i
		}
		from = i + 1
	}
}

// authority returns the end of the authority that starts at start (the
// index of the next '/', whitespace or the end of text) and the index of its
// last '@', or -1 when it has none.
func authority(text string, start int) (end, at int) {
	end = start + strings.IndexFunc(text[start:], func(r rune) bool {
		return r == '/' || isSpace(r)
	})
	if end < start {
		end = len(text)
	}
	at = strings.LastIndexByte(text[start:end], '@')
	if at >= 0 {
		at += start
	}
	return end, at
}

func isSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}

// removeUserinfo removes, for every URL of any scheme, everything between
// "://" and the last '@' of the authority, with that '@'.
func removeUserinfo(text string) string {
	i := userinfoAt(text, 0)
	if i < 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	kept := 0
	for i >= 0 {
		start := i + 3
		_, at := authority(text, start)
		b.WriteString(text[kept:start])
		kept = at + 1
		// A later "://" begins at the authority's last character at the
		// earliest, never inside the part removed.
		i = userinfoAt(text, max(kept, start))
	}
	b.WriteString(text[kept:])
	return b.String()
}

// cut returns the longest prefix of text of at most MaxLen bytes that does
// not end inside a UTF-8 encoded character.
func cut(text string) string {
	if len(text) <= MaxLen {
		return text
	}
	if utf8.RuneStart(text[MaxLen]) {
		return text[:MaxLen]
	}
	for p := MaxLen - 1; p >= 0 && p > MaxLen-utf8.UTFMax; p-- {
		if utf8.RuneStart(text[p]) {
			if _, size := utf8.DecodeRuneInString(text[p:]); p+size > MaxLen {
				return text[:p]
			}
			break
		}
	}
	return text[:MaxLen]
}
