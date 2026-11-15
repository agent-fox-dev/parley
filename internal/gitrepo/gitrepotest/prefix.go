package gitrepotest

import (
	"fmt"
	"strings"
	"testing"
)

// prefixCommits bounds the commits SharedPrefix writes: among this many ids
// two share their first four hexadecimal characters but with a chance of
// about e^-17, and the fixed dates make the ids, and so the outcome, the
// same on every run.
const prefixCommits = 1500

// SharedPrefix writes commits to the repository whose git directory is dir,
// on the ref refs/keep/prefix, until two commits of the repository have ids
// that begin with the same four hexadecimal characters, and returns those
// four characters: the abbreviated id that names more than one commit.
func SharedPrefix(t testing.TB, dir string) string {
	t.Helper()
	var stream strings.Builder
	for i := range prefixCommits {
		msg := fmt.Sprint(i)
		fmt.Fprintf(&stream, "commit refs/keep/prefix\ncommitter T <t@example.invalid> 1700000000 +0000\ndata %d\n%s\n\n", len(msg), msg)
	}
	if _, err := Run(dir, stream.String(), "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for id := range strings.Lines(Git(t, dir, "rev-list", "--all")) {
		p := id[:4]
		if seen[p] {
			return p
		}
		seen[p] = true
	}
	t.Fatalf("no two of %d commits share an abbreviated id", prefixCommits)
	return ""
}
