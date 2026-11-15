package gitserver

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOutcomeLine_WordsEachOutcomeForTheClient(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		outcome Outcome
		want    string
	}{
		{"a new version", Outcome{Branch: "b", Kind: OutcomeVersion, Workspace: "demo", Number: 12, Version: 2}, "change demo#12 version 2"},
		{"a new version of a change in a fork's parent", Outcome{Branch: "b", Kind: OutcomeVersion, Workspace: "demo", Number: 4, Version: 31}, "change demo#4 version 31"},
		{"an opened change", Outcome{Branch: "b", Kind: OutcomeOpened, Workspace: "demo", Number: 12, Version: 1}, "change demo#12 version 1 (opened)"},
		{"an opened draft", Outcome{Branch: "b", Kind: OutcomeOpened, Workspace: "demo", Number: 13, Version: 1, Draft: true}, "change demo#13 version 1 (opened, draft)"},
		{"a warning", Outcome{Branch: "feat/x", Kind: OutcomeWarning, Reason: "internal error"}, "warning: no change for feat/x: internal error"},
		{"nothing", Outcome{Branch: "b"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := c.outcome.Line()
			if got != c.want {
				t.Fatalf("line %q, want %q", got, c.want)
			}
			if got == "" {
				return
			}
			if strings.Contains(got, "http") {
				t.Errorf("line %q holds a URL", got)
			}
			rec := httptest.NewRecorder()
			capabilities{sideBand: true}.write(rec, report{band: []string{got}})
			if body := rec.Body.String(); !strings.Contains(body, "\x02"+got+"\n") || strings.Contains(body, got+"\n\n") {
				t.Errorf("side band %q does not carry %q with one newline", body, got)
			}
		})
	}
}
