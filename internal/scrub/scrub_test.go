package scrub_test

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/scrub"
)

// The shapes of the hub's credentials and of a URL authority with user
// information, written independently of the scrubber so that the tests do
// not check the scrubber against itself.
var (
	credentialShape = regexp.MustCompile(`parley_admin_[0-9a-f]{64}|parley_pat_[a-z0-9]{8}_[a-z0-9]{32}|parley_[0-9A-Za-z]{8}_[0-9A-Za-z]{32}`)
	userinfoShape   = regexp.MustCompile(`://[^/ \t\n\v\f\r]*@`)
)

func randomOver(r *rand.Rand, alphabet string, n int) string {
	var b strings.Builder
	for range n {
		b.WriteByte(alphabet[r.IntN(len(alphabet))])
	}
	return b.String()
}

func adminToken(r *rand.Rand) string {
	return "parley_admin_" + randomOver(r, "0123456789abcdef", 64)
}

func personalAccessToken(r *rand.Rand) string {
	const lower = "abcdefghijklmnopqrstuvwxyz0123456789"
	return "parley_pat_" + randomOver(r, lower, 8) + "_" + randomOver(r, lower, 32)
}

func apiKey(r *rand.Rand) string {
	const alnum = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	return "parley_" + randomOver(r, alnum, 8) + "_" + randomOver(r, alnum, 32)
}

func TestScrubReplacesSecretValues(t *testing.T) {
	t.Parallel()
	s := scrub.New()
	for _, v := range []string{"hunter2-token", "hunter2", "", "red"} {
		s.Add(v)
	}
	tests := []struct {
		name, input, want string
	}{
		{"a registered value", "auth failed for hunter2", "auth failed for [redacted]"},
		{"the longer value first", "hunter2 and hunter2-token", "[redacted] and [redacted]"},
		{"a value passed with the call", "x once-only y", "x [redacted] y"},
		{"text without a value", "nothing here", "nothing here"},
		{"a value inside the marker is ignored", "a red door", "a red door"},
		{"empty text", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := s.Scrub(tt.input, "once-only"); got != tt.want {
				t.Errorf("Scrub(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
	t.Run("a call value applies to its call only", func(t *testing.T) {
		t.Parallel()
		if got := s.Scrub("x once-only y"); got != "x once-only y" {
			t.Errorf("Scrub without the call value = %q, want the text unchanged", got)
		}
	})
}

func TestScrubReplacesHubCredentials(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1, 2))
	a, p, k := adminToken(r), personalAccessToken(r), apiKey(r)
	tests := []struct {
		name, input, want string
	}{
		{"an admin token", "token " + a + " rejected", "token [redacted] rejected"},
		{"a personal access token", "Bearer " + p, "Bearer [redacted]"},
		{"an API key", "key=" + k + ";", "key=[redacted];"},
		{"all three", a + " " + p + " " + k, "[redacted] [redacted] [redacted]"},
		{"a word that starts like an admin token", "parley_admin_token is a file", "parley_admin_token is a file"},
		{"a word that starts like a personal access token", "parley_pat_short_1", "parley_pat_short_1"},
		{"a word that starts like an API key", "see parley_docs", "see parley_docs"},
		{"a credential joined to other characters", "x" + a + "f", "x[redacted]f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := scrub.New().Scrub(tt.input); got != tt.want {
				t.Errorf("Scrub(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestScrubRemovesURLUserInformation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, input, want string
	}{
		{"user and password", "https://user:pass@host/x", "https://host/x"},
		{"in an error message", "fatal: unable to access https://x-token:abc@github.com/o/r.git/: 403", "fatal: unable to access https://github.com/o/r.git/: 403"},
		{"another scheme with a port", "ssh://git@host:22/r", "ssh://host:22/r"},
		{"at the end of the text", "http://u@h", "http://h"},
		{"up to the last @", "https://user:p@ss@host/x", "https://host/x"},
		{"every URL", "a https://u:p@h1/ b https://v:q@h2/ c", "a https://h1/ b https://h2/ c"},
		{"an @ in the path", "https://host/a@b", "https://host/a@b"},
		{"an @ in plain text", "mail bob@example.com", "mail bob@example.com"},
		{"a URL without user information", "https://host/x", "https://host/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := scrub.New().Scrub(tt.input); got != tt.want {
				t.Errorf("Scrub(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestScrubCutsLongResults(t *testing.T) {
	t.Parallel()
	a := func(n int) string { return strings.Repeat("a", n) }
	tests := []struct {
		name    string
		input   string
		wantLen int
	}{
		{"exactly the limit", a(1024), 1024},
		{"one byte over", a(1025), 1024},
		{"a two-byte character across the limit", a(1023) + "é", 1023},
		{"a three-byte character across the limit", a(1022) + "€z", 1022},
		{"a three-byte character ending on the limit", a(1021) + "€", 1024},
		{"only two-byte characters", strings.Repeat("é", 5000), 1024},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := scrub.New().Scrub(tt.input)
			if len(out) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(out), tt.wantLen)
			}
			if !utf8.ValidString(out) {
				t.Errorf("result is not valid UTF-8")
			}
			if !strings.HasPrefix(tt.input, out) {
				t.Errorf("result is not a prefix of the input")
			}
		})
	}
	t.Run("a value across the limit is replaced before the cut", func(t *testing.T) {
		t.Parallel()
		s := scrub.New()
		s.Add("SECRETSECRET")
		out := s.Scrub(a(1018) + "SECRETSECRET" + "tail")
		if strings.Contains(out, "SECRET") {
			t.Errorf("result holds part of the value: %q", out[1000:])
		}
		if len(out) > 1024 {
			t.Errorf("len = %d, want at most 1024", len(out))
		}
	})
}

func TestScrubReturnsTheMarkerWhenARemovalLeavesASecret(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		values []string
		input  string
	}{
		{"a replacement produces a value", []string{"mid", "x[redacted]y"}, "say xmidy now"},
		{"removing user information produces a value", []string{"https://h/secretpath"}, "go https://u:p@h/secretpath"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := scrub.New()
			for _, v := range tt.values {
				s.Add(v)
			}
			if got := s.Scrub(tt.input); got != scrub.Marker {
				t.Errorf("Scrub(%q) = %q, want %q", tt.input, got, scrub.Marker)
			}
		})
	}
}

// propertyCase is one input of the scrubbing property: the values registered
// with the scrubber and the text scrubbed.
type propertyCase struct {
	values []string
	text   string
}

const propertyAlphabet = "abc[]redt:/@ é"

// randomPropertyText returns up to maxBytes bytes over the property alphabet,
// drawn character by character so that é is never split.
func randomPropertyText(r *rand.Rand, minBytes, maxBytes int) string {
	chars := []rune(propertyAlphabet)
	want := minBytes + r.IntN(maxBytes-minBytes+1)
	var b strings.Builder
	for b.Len() < want {
		c := chars[r.IntN(len(chars))]
		if b.Len()+utf8.RuneLen(c) > maxBytes {
			c = 'a'
		}
		b.WriteRune(c)
	}
	return b.String()
}

func generatePropertyCase(r *rand.Rand) propertyCase {
	var c propertyCase
	for range r.IntN(6) {
		c.values = append(c.values, randomPropertyText(r, 1, 40))
	}
	var b strings.Builder
	for range r.IntN(301) {
		switch r.IntN(6) {
		case 0:
			if len(c.values) > 0 {
				b.WriteString(c.values[r.IntN(len(c.values))])
			}
		case 1:
			b.WriteString(adminToken(r))
		case 2:
			b.WriteString(personalAccessToken(r))
		case 3:
			b.WriteString(apiKey(r))
		case 4:
			b.WriteString("https://u:p@h/x")
		default:
			b.WriteString(randomPropertyText(r, 1, 20))
		}
	}
	c.text = b.String()
	return c
}

// cornerCases are inputs where the replacements, the removal of user
// information and the cut meet, kept beside the generated ones so that the
// property is held on them whatever the seed draws.
func cornerCases() []propertyCase {
	r := rand.New(rand.NewPCG(3, 4))
	a := strings.Repeat("a", 1000)
	return []propertyCase{
		{nil, a + strings.Repeat("b", 20) + "https://u:p@h/x"},
		{nil, a + adminToken(r)},
		{nil, a + strings.Repeat("b", 14) + "parley_admin_" + strings.Repeat("f", 65)},
		{[]string{"d]x"}, apiKey(r) + "x"},
		{[]string{"://h"}, "https://u@h"},
		{[]string{"é"}, strings.Repeat("a", 1023) + "éé"},
		{[]string{"[redacted]x", "y"}, "y x"},
		{[]string{"a@"}, "https://ua@@h/x"},
		{[]string{"tail"}, a + strings.Repeat("b", 20) + "tail"},
		{[]string{"://u@"}, "x:/" + "/u@h"},
		{[]string{"redacted]"}, "[redacted] " + adminToken(r)},
		{nil, "x://a@://b@c"},
	}
}

func TestScrubbedTextIsBoundedCleanAndStable(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(2026, 11))
	cases := cornerCases()
	for range 2000 {
		cases = append(cases, generatePropertyCase(r))
	}
	for i, c := range cases {
		s := scrub.New()
		for _, v := range c.values {
			s.Add(v)
		}
		out := s.Scrub(c.text)
		if len(out) > 1024 {
			t.Fatalf("case %d: len = %d, want at most 1024", i, len(out))
		}
		if again := s.Scrub(out); again != out {
			t.Fatalf("case %d: scrubbing again changed\n%q\ninto\n%q", i, out, again)
		}
		for _, v := range c.values {
			if v != "" && !strings.Contains(scrub.Marker, v) && strings.Contains(out, v) {
				t.Fatalf("case %d: result holds the value %q: %q", i, v, out)
			}
		}
		if m := credentialShape.FindString(out); m != "" {
			t.Fatalf("case %d: result holds the credential %q", i, m)
		}
		if m := userinfoShape.FindString(out); m != "" {
			t.Fatalf("case %d: result holds user information %q", i, m)
		}
	}
}
