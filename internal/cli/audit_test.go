package cli

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// auditPath is the audit route as sent.
const auditPath = "/api/v1/audit"

// auditPage is one page of the audit query as the hub answers it.
const auditPage = `{"items": [{"source": "hub", "id": "e1", "type": "parley.change.opened"}], "next_cursor": "c2"}`

func TestAuditQuery_SendsAParameterForExactlyTheFlagsGiven(t *testing.T) {
	t.Parallel()
	// Each flag and the parameter it is sent as, with a value the hub would
	// refuse where one exists, which the command passes on unchanged.
	flags := []struct{ flag, param, value string }{
		{"source", "source", "neither"},
		{"workspace", "workspace", "demo"},
		{"types", "types", "parley.change.*"},
		{"session", "session_id", "S"},
		{"run-id", "run_id", "not-a-run"},
		{"actor-id", "actor_id", "u-1"},
		{"resource-type", "resource_type", "change"},
		{"resource-id", "resource_id", "r-1"},
		{"action", "action", "opened"},
		{"since", "since", "2026-11-20T00:00:00Z"},
		{"until", "until", "yesterday"},
		{"limit", "limit", "0"},
		{"cursor", "cursor", "c1"},
	}
	type row struct {
		name  string
		args  []string
		query url.Values
		// rawQuery, when set, is the query string exactly as sent.
		rawQuery string
	}
	rows := []row{
		{name: "no flag", query: url.Values{}},
		{
			name:     "a list of types",
			args:     []string{"--types", "parley.change.*,tool.started"},
			query:    url.Values{"types": {"parley.change.*,tool.started"}},
			rawQuery: "types=parley.change.%2A%2Ctool.started",
		},
		{name: "an empty value", args: []string{"--workspace", ""}, query: url.Values{"workspace": {""}}},
	}
	every := row{name: "every flag", query: url.Values{}}
	for _, f := range flags {
		rows = append(rows, row{name: "--" + f.flag, args: []string{"--" + f.flag, f.value}, query: url.Values{f.param: {f.value}}})
		every.args = append(every.args, "--"+f.flag, f.value)
		every.query.Set(f.param, f.value)
	}
	rows = append(rows, every)
	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodGet, auditPath, http.StatusOK, auditPage)

			code, stdout, stderr, sent := runOnHub(t, hub, append([]string{"audit", "query"}, tt.args...)...)

			wantResult(t, code, stdout, stderr, auditPage)
			got := wantOnlyRequest(t, sent, http.MethodGet, auditPath)
			wantQuery(t, got.query, tt.query)
			if tt.rawQuery != "" && got.query != tt.rawQuery {
				t.Errorf("query string = %q, want %q", got.query, tt.rawQuery)
			}
		})
	}
}

func TestAuditQuery_PrintsTheHubsRefusalAsReceived(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status    int
		errorType string
	}{
		{http.StatusBadRequest, "invalid_request"},
		{http.StatusNotFound, "not_found"},
		{http.StatusForbidden, "insufficient_scope"},
	}
	for _, tt := range tests {
		t.Run(tt.errorType, func(t *testing.T) {
			t.Parallel()
			refusal := envelope(tt.status, tt.errorType)
			hub := newStubHub(t).answer(http.MethodGet, auditPath, tt.status, refusal)

			code, stdout, stderr, sent := runOnHub(t, hub, "audit", "query", "--source", "both")

			wantOnlyRequest(t, sent, http.MethodGet, auditPath)
			wantHubFailure(t, code, stdout, stderr, refusal)
		})
	}
}

func TestAuditQuery_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t)
	tests := []struct {
		args      []string
		inMessage string
	}{
		{[]string{"audit", "query", "extra"}, `got "extra"`},
		{[]string{"audit", "query", "--limit", "ten"}, `"ten" for "--limit" flag`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr, _ := runOnHub(t, hub, tt.args...)

			if message := wantClientFailure(t, code, stdout, stderr, "usage_error"); !strings.Contains(message, tt.inMessage) {
				t.Errorf("message = %q, want it to contain %q", message, tt.inMessage)
			}
		})
	}
	// The group alone prints its help, which lists the command.
	code, stdout, stderr, _ := runOnHub(t, hub, "audit")
	if code != 0 || stderr != "" || !regexp.MustCompile(`(?m)^\s+query\s+\S`).MatchString(stdout) {
		t.Errorf("parley audit: exit status %d, stdout %q, stderr %q; want 0, a help listing query and nothing", code, stdout, stderr)
	}
	if sent := hub.recorded(); len(sent) != 0 {
		t.Errorf("the hub got %v, want no request", sent)
	}
}
