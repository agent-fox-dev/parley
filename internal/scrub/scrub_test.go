package scrub

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	lowerAlnum = "abcdefghijklmnopqrstuvwxyz0123456789"
	alnum      = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	hexDigits  = "0123456789abcdef"
)

func randomFrom(r *rand.Rand, alphabet string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(b)
}

func adminToken(r *rand.Rand) string { return "parley_admin_" + randomFrom(r, hexDigits, 64) }
func pat(r *rand.Rand) string {
	return "parley_pat_" + randomFrom(r, lowerAlnum, 8) + "_" + randomFrom(r, lowerAlnum, 32)
}
func apiKey(r *rand.Rand) string {
	return "parley_" + randomFrom(r, alnum, 8) + "_" + randomFrom(r, alnum, 32)
}

func TestScrub_ReplacesRegisteredAndPassedValues(t *testing.T) {
	s := New()
	for _, v := range []string{"hunter2-token", "hunter2", "", "red"} {
		s.Add(v)
	}
	tests := []struct{ in, want string }{
		{"auth failed for hunter2", "auth failed for [redacted]"},
		{"hunter2 and hunter2-token", "[redacted] and [redacted]"},
		{"x once-only y", "x [redacted] y"},
		{"nothing here", "nothing here"},
		{"a red door", "a red door"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := s.Scrub(tt.in, "once-only"); got != tt.want {
			t.Errorf("Scrub(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := s.Scrub("x once-only y"); got != "x once-only y" {
		t.Errorf("a value passed with one call was remembered: %q", got)
	}
}

func TestScrub_ReplacesTheHubsOwnCredentialFormats(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	a, p, k := adminToken(r), pat(r), apiKey(r)
	tests := []struct{ in, want string }{
		{"token " + a + " rejected", "token [redacted] rejected"},
		{"Bearer " + p, "Bearer [redacted]"},
		{"key=" + k + ";", "key=[redacted];"},
		{a + " " + p + " " + k, "[redacted] [redacted] [redacted]"},
		{"parley_admin_token is a file", "parley_admin_token is a file"},
		{"parley_pat_short_1", "parley_pat_short_1"},
		{"see parley_docs", "see parley_docs"},
	}
	for _, tt := range tests {
		if got := New().Scrub(tt.in); got != tt.want {
			t.Errorf("Scrub(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestScrub_RemovesUserInformationFromURLs(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://user:pass@host/x", "https://host/x"},
		{"fatal: unable to access https://x-token:abc@github.com/o/r.git/: 403", "fatal: unable to access https://github.com/o/r.git/: 403"},
		{"ssh://git@host:22/r", "ssh://host:22/r"},
		{"http://u@h", "http://h"},
		{"https://user:p@ss@host/x", "https://host/x"},
		{"a https://u:p@h1/ b https://v:q@h2/ c", "a https://h1/ b https://h2/ c"},
		{"https://host/a@b", "https://host/a@b"},
		{"mail bob@example.com", "mail bob@example.com"},
		{"https://host/x", "https://host/x"},
	}
	for _, tt := range tests {
		if got := New().Scrub(tt.in); got != tt.want {
			t.Errorf("Scrub(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestScrub_CutsTheResultTo1024BytesOnACharacterBoundary(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{strings.Repeat("a", 1024), 1024},
		{strings.Repeat("a", 1025), 1024},
		{strings.Repeat("a", 1023) + "é", 1023},
		{strings.Repeat("a", 1022) + "€z", 1022},
		{strings.Repeat("a", 1021) + "€", 1024},
		{strings.Repeat("é", 5000), 1024},
	}
	for _, tt := range tests {
		out := New().Scrub(tt.in)
		if len(out) != tt.want || !utf8.ValidString(out) || !strings.HasPrefix(tt.in, out) {
			t.Errorf("input of %d bytes: got %d bytes (valid %v), want %d", len(tt.in), len(out), utf8.ValidString(out), tt.want)
		}
	}
	s := New()
	s.Add("SECRETSECRET")
	out := s.Scrub(strings.Repeat("a", 1018) + "SECRETSECRET" + "tail")
	if strings.Contains(out, "SECRET") || len(out) > 1024 {
		t.Errorf("a secret across the cut survived in part: %q", out[len(out)-20:])
	}
}

func TestScrub_DiscardsTextThatStillHoldsASecret(t *testing.T) {
	tests := []struct {
		values []string
		in     string
	}{
		{[]string{"mid", "x[redacted]y"}, "say xmidy now"},
		{[]string{"https://h/secretpath"}, "go https://u:p@h/secretpath"},
	}
	for _, tt := range tests {
		s := New()
		for _, v := range tt.values {
			s.Add(v)
		}
		if got := s.Scrub(tt.in); got != Redacted {
			t.Errorf("Scrub(%q) = %q, want the marker alone", tt.in, got)
		}
	}
}

var (
	credentialPattern = regexp.MustCompile(`parley_admin_[0-9a-f]{64}|parley_pat_[a-z0-9]{8}_[a-z0-9]{32}|parley_[0-9A-Za-z]{8}_[0-9A-Za-z]{32}`)
	userinfoPattern   = regexp.MustCompile(`://[^/\s]*@`)
)

func TestScrub_ResultIsShortCleanAndStable(t *testing.T) {
	const alphabet = "abc[]redt:/@ é"
	r := rand.New(rand.NewSource(42))
	for i := 0; i < 2000; i++ {
		s := New()
		var values []string
		for range r.Intn(6) {
			v := randomFrom(r, alphabet, 1+r.Intn(40))
			values = append(values, v)
			s.Add(v)
		}
		var b strings.Builder
		for range r.Intn(301) {
			switch r.Intn(6) {
			case 0:
				if len(values) > 0 {
					b.WriteString(values[r.Intn(len(values))])
				}
			case 1:
				b.WriteString(adminToken(r))
			case 2:
				b.WriteString(pat(r))
			case 3:
				b.WriteString(apiKey(r))
			case 4:
				b.WriteString("https://u:p@h/x")
			default:
				b.WriteString(randomFrom(r, alphabet, 1+r.Intn(20)))
			}
		}
		in := b.String()
		out := s.Scrub(in)
		if len(out) > MaxLength {
			t.Fatalf("case %d: %d bytes", i, len(out))
		}
		if again := s.Scrub(out); again != out {
			t.Fatalf("case %d: scrubbing again changed %q into %q", i, out, again)
		}
		for _, v := range values {
			if v != "" && !strings.Contains(Redacted, v) && strings.Contains(out, v) {
				t.Fatalf("case %d: value %q survived in %q", i, v, out)
			}
		}
		if credentialPattern.MatchString(out) || userinfoPattern.MatchString(out) {
			t.Fatalf("case %d: a credential or user information survived in %q", i, out)
		}
	}
}
