package cli

import (
	"net/http"
	"strings"
	"testing"
)

// overlapsPath is the overlaps route of change 7 of demo, as sent.
const overlapsPath = changesPath + "/7/overlaps"

func TestChangeOverlaps_SendsOneGetAndPrintsTheAnswer(t *testing.T) {
	t.Parallel()
	answer := `{"items": [{"change": {"number": 2}, "paths": ["api/handler.go"], "paths_truncated": false, "count": 1}], "next_cursor": null}`
	tests := []struct {
		name    string
		status  int
		answers string
	}{
		{"an overlap", http.StatusOK, answer},
		{"an unknown change", http.StatusNotFound, changeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodGet, overlapsPath, tt.status, tt.answers)

			code, stdout, stderr, sent := runOnHub(t, hub, "change", "overlaps", "demo", "7")

			got := wantOnlyRequest(t, sent, http.MethodGet, overlapsPath)
			wantQuery(t, got.query, nil)
			if tt.status == http.StatusOK {
				wantResult(t, code, stdout, stderr, tt.answers)
				return
			}
			if code != 1 || stdout != "" {
				t.Errorf("exit status %d, stdout %q; want 1 and nothing", code, stdout)
			}
			if want := indented(t, tt.answers); stderr != want {
				t.Errorf("stderr = %q, want the hub's envelope %q", stderr, want)
			}
		})
	}
}

func TestChangeOverlaps_FailsOnOverlapWhenTheListHoldsAnEntry(t *testing.T) {
	t.Parallel()
	one := `{"items": [{"change": {"number": 2}, "paths": ["api/handler.go"], "paths_truncated": false, "count": 1}], "next_cursor": null}`
	none := `{"items": [], "next_cursor": null}`
	tests := []struct {
		name, body string
		flag       bool
		want       int
	}{
		{"an entry", one, true, 3},
		{"no entry", none, true, 0},
		{"an entry without the flag", one, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodGet, overlapsPath, http.StatusOK, tt.body)
			args := []string{"change", "overlaps", "demo", "7"}
			if tt.flag {
				args = append(args, "--fail-on-overlap")
			}

			code, stdout, stderr, sent := runOnHub(t, hub, args...)

			wantOnlyRequest(t, sent, http.MethodGet, overlapsPath)
			wantFailOnOverlap(t, code, stdout, stderr, tt.body, tt.want)
		})
	}
}

func TestChangeOverlaps_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t)
	tests := []struct {
		args      []string
		inMessage string
	}{
		{[]string{"change", "overlaps", "demo"}, "parley change overlaps needs the argument <number>"},
		{[]string{"change", "overlaps", "demo", "abc"}, `parley change overlaps takes a <number> that is a positive integer, got "abc"`},
		{[]string{"change", "overlaps", "demo", "0"}, `parley change overlaps takes a <number> that is a positive integer, got "0"`},
		{[]string{"change", "overlaps", "demo", "7", "8"}, `got the extra argument "8"`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr, _ := runOnHub(t, hub, tt.args...)

			if message := wantClientFailure(t, code, stdout, stderr, "usage_error"); !strings.Contains(message, tt.inMessage) {
				t.Errorf("message = %q, want it to contain %q", message, tt.inMessage)
			}
		})
	}
	if sent := hub.recorded(); len(sent) != 0 {
		t.Errorf("the hub got %v, want no request", sent)
	}
}
