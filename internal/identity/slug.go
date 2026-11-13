package identity

import "strings"

// slug derives the slug of a personal org from its user's username:
// lowercased, every character outside [a-z0-9_-] replaced with -, runs of -
// collapsed and - and _ trimmed from both ends. What is left of fewer than
// two characters becomes u-<the first 8 characters of userID>, and a slug
// that starts with a digit gets the prefix u-. Whether the slug is free is
// for the caller to find out.
func slug(username, userID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(username) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	s := b.String()
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-_")
	switch {
	case len(s) < 2:
		return "u-" + userID[:min(8, len(userID))]
	case s[0] >= '0' && s[0] <= '9':
		return "u-" + s
	}
	return s
}
