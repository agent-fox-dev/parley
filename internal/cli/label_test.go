package cli

import (
	"net/http"
	"net/url"
	"testing"
)

func TestLabel_CreateSendsTheFlagsGivenAndPrintsTheAnswer(t *testing.T) {
	t.Parallel()
	wantWrites(t, []sentWrite{
		{args: []string{"label", "create", "demo", "type/bug"}, method: http.MethodPost, path: "demo/labels", body: `{"name":"type/bug"}`, status: http.StatusCreated, answer: hubAnswer},
		{args: []string{"label", "create", "demo", "type/bug"}, method: http.MethodPost, path: "demo/labels", body: `{"name":"type/bug"}`, status: http.StatusOK, answer: hubAnswer},
		{
			args:   []string{"label", "create", "demo", "n", "--color", "#00ff00", "--description", "d", "--target", "upstream"},
			method: http.MethodPost, path: "demo/labels", query: url.Values{"target": {"upstream"}},
			body: `{"name":"n","color":"#00ff00","description":"d"}`,
		},
		{args: []string{"label", "create", "demo", "n", "--description", ""}, method: http.MethodPost, path: "demo/labels", body: `{"name":"n","description":""}`},
	})
}

func TestLabel_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	wantRefused(t, [][]string{
		{"label", "create", "demo"},
		{"label", "create"},
		{"label", "create", "demo", "n", "m"},
		{"label", "create", "demo", "n", "--body", "x"},
		{"label", "delete", "demo", "n"},
	})
}
