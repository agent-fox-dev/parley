package sandbox_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/sandbox"
)

func TestScrub(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		values sandbox.Values
		text   string
		want   string
	}{
		{"an exposed secret of 8 bytes", sandbox.Values{Exposed: []string{"abcdefgh"}}, "x abcdefgh y abcdefgh", "x [redacted] y [redacted]"},
		{"an exposed secret of 7 bytes", sandbox.Values{Exposed: []string{"abcdefg"}}, "x abcdefg y", "x abcdefg y"},
		{"a credential's secret of any length", sandbox.Values{Credentials: []string{"k1"}}, "key k1!", "key [redacted]!"},
		{"the hub credential", sandbox.Values{HubCredential: "pat_abc123"}, "Bearer pat_abc123", "Bearer [redacted]"},
		{"a value inside a longer one", sandbox.Values{Credentials: []string{"token"}, Exposed: []string{"token-longer-1"}},
			"a token-longer-1 b token", "a [redacted] b [redacted]"},
		{"no value", sandbox.Values{}, "plain", "plain"},
		{"no value in 1024 bytes", sandbox.Values{}, strings.Repeat("p", 1024), strings.Repeat("p", 1024)},
		{"2000 bytes of a two-byte character", sandbox.Values{}, strings.Repeat("é", 1000), strings.Repeat("é", 512)},
		{"a cut inside a character", sandbox.Values{}, "a" + strings.Repeat("é", 1000), "a" + strings.Repeat("é", 511)},
		{"a value across the cut", sandbox.Values{Exposed: []string{"SECRET1234"}},
			strings.Repeat("x", 1020) + "SECRET1234 tail", strings.Repeat("x", 1020) + "[red"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sandbox.Scrub(tc.values, tc.text)
			if got != tc.want {
				t.Errorf("Scrub = %q; want %q", got, tc.want)
			}
			if len(got) > 1024 || !utf8.ValidString(got) {
				t.Errorf("Scrub returned %d bytes, valid UTF-8 %t; want at most 1024 bytes of valid UTF-8", len(got), utf8.ValidString(got))
			}
		})
	}
}
