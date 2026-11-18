package cli

import (
	"net/url"
	"slices"
	"testing"
)

func TestPR_ReadsSendOneGetWithTheFlagsGivenAndPrintTheAnswer(t *testing.T) {
	t.Parallel()
	upstream := url.Values{"target": {"upstream"}}
	page := url.Values{"limit": {"5"}, "cursor": {"c"}, "target": {"upstream"}}
	reads := []sentRead{
		{[]string{"pr", "get", "widgets", "7"}, "widgets/pulls/7", nil},
		{[]string{"pr", "get", "a/b", "7"}, "a%2Fb/pulls/7", nil},
		{[]string{"pr", "get", "widgets", "2147483647"}, "widgets/pulls/2147483647", nil},
		{[]string{"pr", "get", "widgets", "7", "--target", "upstream"}, "widgets/pulls/7", upstream},
	}
	for _, sub := range []string{"state", "files", "checks", "reviews"} {
		reads = append(reads,
			sentRead{[]string{"pr", sub, "widgets", "7"}, "widgets/pulls/7/" + sub, nil},
			sentRead{[]string{"pr", sub, "a/b", "7"}, "a%2Fb/pulls/7/" + sub, nil},
		)
	}
	reads = append(reads, sentRead{[]string{"pr", "state", "widgets", "7", "--target", "upstream"}, "widgets/pulls/7/state", upstream})
	for _, sub := range []string{"files", "checks", "reviews"} {
		reads = append(reads, sentRead{[]string{"pr", sub, "widgets", "7", "--limit", "5", "--cursor", "c", "--target", "upstream"}, "widgets/pulls/7/" + sub, page})
	}
	wantReads(t, reads)
}

func TestPR_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	lines := [][]string{
		{"pr", "get", "widgets", "7", "--limit", "5"},
		{"pr", "state", "widgets", "7", "--cursor", "c"},
		{"pr", "files", "widgets", "7", "--state", "open"},
	}
	for _, sub := range []string{"get", "state", "files", "checks", "reviews"} {
		lines = slices.Concat(lines,
			[][]string{{"pr", sub, "widgets"}, {"pr", sub, "widgets", "7", "8"}},
			withBadNumbers("pr", sub),
		)
	}
	wantRefused(t, lines)
}
