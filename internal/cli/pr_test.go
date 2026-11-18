package cli

import (
	"net/http"
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

func TestPR_WritesSendTheFlagsGivenAndPrintTheAnswer(t *testing.T) {
	t.Parallel()
	wantWrites(t, []sentWrite{
		{args: []string{"pr", "create", "demo", "--title", "T", "--head", "f", "--base", "main"}, method: http.MethodPost, path: "demo/pulls", body: `{"title":"T","head":"f","base":"main"}`, status: http.StatusCreated, answer: hubAnswer},
		{
			args:   []string{"pr", "create", "demo", "--title", "T", "--head", "bob:f", "--base", "main", "--body", "B", "--draft", "--target", "upstream"},
			method: http.MethodPost, path: "demo/pulls", query: url.Values{"target": {"upstream"}},
			body: `{"title":"T","head":"bob:f","base":"main","body":"B","draft":true}`,
		},
		{args: []string{"pr", "create", "demo", "--title", "T", "--head", "f", "--base", "main", "--draft=false"}, method: http.MethodPost, path: "demo/pulls", body: `{"title":"T","head":"f","base":"main","draft":false}`},
		{args: []string{"pr", "comment", "demo", "12", "--body", "ok"}, method: http.MethodPost, path: "demo/pulls/12/comments", body: `{"body":"ok"}`, status: http.StatusCreated, answer: hubAnswer},
		{args: []string{"pr", "comment", "a/b", "12", "--body", "ok", "--target", "upstream"}, method: http.MethodPost, path: "a%2Fb/pulls/12/comments", query: url.Values{"target": {"upstream"}}, body: `{"body":"ok"}`},
	})
}

func TestPR_WritesSendTheBodyFileAsRead(t *testing.T) {
	t.Parallel()
	wantBodyRead(t, []sentWrite{
		{args: []string{"pr", "create", "demo", "--title", "T", "--head", "f", "--base", "main"}, method: http.MethodPost, path: "demo/pulls", status: http.StatusCreated},
		{args: []string{"pr", "comment", "demo", "12"}, method: http.MethodPost, path: "demo/pulls/12/comments", status: http.StatusCreated},
	})
}

func TestPR_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	lines := [][]string{
		{"pr", "get", "widgets", "7", "--limit", "5"},
		{"pr", "state", "widgets", "7", "--cursor", "c"},
		{"pr", "files", "widgets", "7", "--state", "open"},
		{"pr", "comment", "demo", "abc", "--body", "x"},
		{"pr", "comment", "demo", "0", "--body", "x"},
		{"pr", "create", "demo", "--title", "T", "--head", "f", "--base", "main", "--body-file", "/nonexistent"},
		{"pr", "create", "demo", "--title", "T", "--head", "f", "--base", "main", "--body", "a", "--body-file", "-"},
		{"pr", "create", "demo", "--title", "T", "--head", "f"},
		{"pr", "create", "demo", "--title", "T", "--base", "main"},
		{"pr", "create", "demo", "--head", "f", "--base", "main"},
		{"pr", "create", "demo", "--title", "T", "--head", "f", "--base", "main", "--draft=maybe"},
		{"pr", "comment", "demo", "12"},
		{"pr", "comment", "demo", "--body", "x"},
	}
	for _, sub := range []string{"get", "state", "files", "checks", "reviews"} {
		lines = slices.Concat(lines,
			[][]string{{"pr", sub, "widgets"}, {"pr", sub, "widgets", "7", "8"}},
			withBadNumbers("pr", sub),
		)
	}
	wantRefused(t, lines)
}
