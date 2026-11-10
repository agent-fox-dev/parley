package identity

import (
	"context"
	"fmt"
	"strings"
)

// baseSlug derives a personal org's slug from a username: lowercase it;
// replace every character outside [a-z0-9_-] with -; collapse runs of -;
// trim leading and trailing - and _; with fewer than 2 characters left use
// u-<first 8 characters of the user id>; prefix a leading digit with u-.
func baseSlug(username, userID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(username) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := b.String()
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-_")
	if len(s) < 2 {
		id := userID
		if len(id) > 8 {
			id = id[:8]
		}
		return "u-" + id
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = "u-" + s
	}
	return s
}

// freeSlug returns the base slug, or the first free of <slug>-1 to
// <slug>-10; ErrConflict when none is free.
func freeSlug(ctx context.Context, q Querier, base string) (string, error) {
	candidates := []string{base}
	for i := 1; i <= 10; i++ {
		candidates = append(candidates, fmt.Sprintf("%s-%d", base, i))
	}
	for _, c := range candidates {
		taken, err := exists(ctx, q, `SELECT 1 FROM orgs WHERE slug = ?`, c)
		if err != nil {
			return "", err
		}
		if !taken {
			return c, nil
		}
	}
	return "", ErrConflict
}
