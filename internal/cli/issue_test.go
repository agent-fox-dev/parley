package cli

import (
	"net/http"
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

func TestIssue_WritesSendTheFlagsGivenAndPrintTheAnswer(t *testing.T) {
	t.Parallel()
	upstream := url.Values{"target": {"upstream"}}
	wantWrites(t, []sentWrite{
		{args: []string{"issue", "create", "demo", "--title", "T"}, method: http.MethodPost, path: "demo/issues", body: `{"title":"T"}`, status: http.StatusCreated, answer: hubAnswer},
		{
			args:   []string{"issue", "create", "demo", "--title", "T", "--body", "B", "--label", "a", "--label", "b", "--assignee", "ann", "--target", "upstream"},
			method: http.MethodPost, path: "demo/issues", query: upstream,
			body: `{"title":"T","body":"B","labels":["a","b"],"assignees":["ann"]}`,
		},
		{args: []string{"issue", "create", "a/b", "--title", "T", "--label", "good first issue"}, method: http.MethodPost, path: "a%2Fb/issues", body: `{"title":"T","labels":["good first issue"]}`},
		{args: []string{"issue", "update", "demo", "7", "--state", "closed"}, method: http.MethodPatch, path: "demo/issues/7", body: `{"state":"closed"}`},
		{args: []string{"issue", "update", "demo", "7", "--body", ""}, method: http.MethodPatch, path: "demo/issues/7", body: `{"body":""}`},
		{args: []string{"issue", "update", "demo", "7", "--title", "N", "--label", "x"}, method: http.MethodPatch, path: "demo/issues/7", body: `{"title":"N","labels":["x"]}`},
		{args: []string{"issue", "update", "demo", "7", "--assignee", "ann", "--target", "origin"}, method: http.MethodPatch, path: "demo/issues/7", query: url.Values{"target": {"origin"}}, body: `{"assignees":["ann"]}`},
		{args: []string{"issue", "close", "demo", "7"}, method: http.MethodPost, path: "demo/issues/7/close"},
		{args: []string{"issue", "close", "demo", "2147483647"}, method: http.MethodPost, path: "demo/issues/2147483647/close"},
		{args: []string{"issue", "close", "demo", "7", "--comment", "done"}, method: http.MethodPost, path: "demo/issues/7/close", body: `{"comment":"done"}`},
		{args: []string{"issue", "comment", "demo", "7", "--body", "hi", "--target", "upstream"}, method: http.MethodPost, path: "demo/issues/7/comments", query: upstream, body: `{"body":"hi"}`, status: http.StatusCreated, answer: hubAnswer},
		{args: []string{"issue", "comment", "demo", "7", "--body", "hi"}, method: http.MethodPost, path: "demo/issues/7/comments", body: `{"body":"hi"}`},
	})
}

func TestIssue_LabelAndUnlabelSendTheLabelsAndPrintAConfirmation(t *testing.T) {
	t.Parallel()
	wantWrites(t, []sentWrite{
		{
			args: []string{"issue", "label", "demo", "7", "triage", "type/bug"}, method: http.MethodPost, path: "demo/issues/7/labels",
			body: `["triage","type/bug"]`, status: http.StatusNoContent,
			printed: `{"status":"labeled","number":7,"labels":["triage","type/bug"]}`,
		},
		{
			args: []string{"issue", "label", "demo", "7", "good first issue"}, method: http.MethodPost, path: "demo/issues/7/labels",
			body: `["good first issue"]`, status: http.StatusNoContent,
			printed: `{"status":"labeled","number":7,"labels":["good first issue"]}`,
		},
		{
			args: []string{"issue", "unlabel", "demo", "7", "type/bug", "--target", "upstream"}, method: http.MethodDelete, path: "demo/issues/7/labels/type%2Fbug",
			query: url.Values{"target": {"upstream"}}, status: http.StatusNoContent,
			printed: `{"status":"unlabeled","number":7,"label":"type/bug"}`,
		},
	})
}

func TestIssue_WritesSendTheBodyFileAsRead(t *testing.T) {
	t.Parallel()
	wantBodyRead(t, []sentWrite{
		{args: []string{"issue", "create", "demo", "--title", "T"}, method: http.MethodPost, path: "demo/issues", status: http.StatusCreated},
		{args: []string{"issue", "update", "demo", "7"}, method: http.MethodPatch, path: "demo/issues/7"},
		{args: []string{"issue", "comment", "demo", "7"}, method: http.MethodPost, path: "demo/issues/7/comments", status: http.StatusCreated},
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
			{"issue", "update", "demo", "0", "--title", "x"},
			{"issue", "close", "demo", "07"},
			{"issue", "close", "demo", "+7"},
			{"issue", "comment", "demo", "2147483648", "--body", "x"},
			{"issue", "unlabel", "demo", "-1", "bug"},
			{"issue", "label", "demo", "abc", "bug"},
			{"issue", "create", "demo", "--title", "T", "--body", "a", "--body-file", "f"},
			{"issue", "comment", "demo", "7", "--body-file", "/nonexistent"},
			{"issue", "create", "demo"},
			{"issue", "create", "demo", "--body", "B"},
			{"issue", "comment", "demo", "7"},
			{"issue", "update", "demo", "7"},
			{"issue", "update", "demo", "7", "--target", "upstream"},
			{"issue", "label", "demo", "7"},
			{"issue", "unlabel", "demo", "7"},
			{"issue", "unlabel", "demo", "7", "a", "b"},
			{"issue", "close", "demo"},
			{"issue", "close", "demo", "7", "8"},
			{"issue", "create", "demo", "7", "--title", "T"},
		},
		withBadNumbers("issue", "get"),
		withBadNumbers("issue", "comments"),
		withBadNumbers("issue", "close"),
	))
}
