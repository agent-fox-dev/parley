package gitrepo_test

import (
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

func TestSanitizeGitText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		text    string
		secrets []string
		want    string
	}{
		{"user and password of a URL",
			"fatal: unable to access 'https://bob:hunter2@git.example/x.git/': 401", nil,
			"fatal: unable to access 'https://***@git.example/x.git/': 401"},
		{"a user name alone",
			"fetching http://bob@git.example:8080/x.git", nil,
			"fetching http://***@git.example:8080/x.git"},
		{"every URL of the text",
			"https://a:b@one.example and ssh://git@two.example/r", nil,
			"https://***@one.example and ssh://***@two.example/r"},
		{"an @ in a URL's path is no user information",
			"https://git.example/@scope/pkg", nil,
			"https://git.example/@scope/pkg"},
		{"each secret value removed",
			"sent tok-1 then tok-2, tok-1 again", []string{"tok-1", "tok-2"},
			"sent  then ,  again"},
		{"an empty secret removes nothing",
			"nothing secret", []string{""},
			"nothing secret"},
		{"a secret within a URL",
			"https://git.example/x.git?token=tok-9", []string{"tok-9"},
			"https://git.example/x.git?token="},
		{"text without either is kept", "error: exit status 128", []string{"tok-3"}, "error: exit status 128"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := gitrepo.SanitizeGitText(c.text, c.secrets...); got != c.want {
				t.Errorf("SanitizeGitText(%q, %q) = %q, want %q", c.text, c.secrets, got, c.want)
			}
		})
	}
}
