package gitrepo

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
)

// userinfo matches the user information of every URL of a text: what lies
// between a scheme's "://" and an "@" before the URL's path, query or end.
var userinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)[^/?#\s'"]*@`)

// SanitizeGitText is text, the output or error text of a git command, made
// fit to show to a client or to log: the user information of every URL in
// it reads ***@, and every non-empty value of secrets, such as the
// credential the command presented, is removed. Longer values are removed
// first, so that one that holds another goes whole.
func SanitizeGitText(text string, secrets ...string) string {
	text = userinfo.ReplaceAllString(text, "${1}***@")
	sorted := slices.SortedFunc(slices.Values(secrets), func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, s := range sorted {
		if s != "" {
			text = strings.ReplaceAll(text, s, "")
		}
	}
	return text
}
