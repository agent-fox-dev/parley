package cli

import (
	"net/url"
	"slices"
	"testing"
)

func TestIssue_ReadsSendOneGetWithTheFlagsGivenAndPrintTheAnswer(t *testing.T) {
	t.Parallel()
	wantReads(t, []sentRead{
		{[]string{"issue", "list", "widgets"}, "widgets/issues", nil},
		{[]string{"issue", "list", "a/b"}, "a%2Fb/issues", nil},
		{
			[]string{"issue", "list", "widgets", "--state", "closed", "--label", "bug", "--label", "ui", "--assignee", "kim",
				"--limit", "10", "--cursor", "c1", "--target", "upstream"},
			"widgets/issues",
			url.Values{"state": {"closed"}, "labels": {"bug,ui"}, "assignee": {"kim"}, "limit": {"10"}, "cursor": {"c1"}, "target": {"upstream"}},
		},
		{[]string{"issue", "list", "widgets", "--label", "good first issue"}, "widgets/issues", url.Values{"labels": {"good first issue"}}},
		{[]string{"issue", "get", "widgets", "7"}, "widgets/issues/7", nil},
		{[]string{"issue", "get", "a/b", "7"}, "a%2Fb/issues/7", nil},
		{[]string{"issue", "get", "widgets", "2147483647"}, "widgets/issues/2147483647", nil},
		{[]string{"issue", "get", "widgets", "7", "--target", "upstream"}, "widgets/issues/7", url.Values{"target": {"upstream"}}},
		{[]string{"issue", "comments", "widgets", "7"}, "widgets/issues/7/comments", nil},
		{[]string{"issue", "comments", "a/b", "7"}, "a%2Fb/issues/7/comments", nil},
		{[]string{"issue", "comments", "widgets", "7", "--limit", "5", "--cursor", "c"}, "widgets/issues/7/comments", url.Values{"limit": {"5"}, "cursor": {"c"}}},
		{[]string{"issue", "comments", "widgets", "7", "--target", "upstream"}, "widgets/issues/7/comments", url.Values{"target": {"upstream"}}},
	})
}

func TestIssue_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	wantRefused(t, slices.Concat(
		[][]string{
			{"issue", "list"},
			{"issue", "list", "widgets", "7"},
			{"issue", "get", "widgets"},
			{"issue", "get", "widgets", "7", "8"},
			{"issue", "comments", "widgets"},
			{"issue", "list", "widgets", "--sort", "updated"},
			{"issue", "list", "widgets", "--limit", "ten"},
			{"issue", "get", "widgets", "7", "--limit", "5"},
			{"issue", "get", "widgets", "7", "--state", "open"},
		},
		withBadNumbers("issue", "get"),
		withBadNumbers("issue", "comments"),
	))
}
