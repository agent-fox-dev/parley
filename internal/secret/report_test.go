package secret

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestStartReport_CountsTheValuesNoConfiguredKeyOpens(t *testing.T) {
	cur, prev := newKey(t), newKey(t)
	cases := []struct {
		name              string
		underCurrent      int
		underPrevious     bool
		keys              Keys
		wantUndecryptable float64
		wantWarning       bool
	}{
		{"empty store", 0, false, Keys{Current: cur}, 0, false},
		{"values under the current key", 2, false, Keys{Current: cur}, 0, false},
		{"a value under the configured previous key", 2, true, Keys{Current: cur, Previous: prev}, 0, false},
		{"a value under a previous key no longer configured", 2, true, Keys{Current: cur}, 1, false},
		{"no key configured", 2, true, Keys{}, 3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStoreTest(t, Keys{Current: cur})
			s.fill(ScopeUser, "u1", tc.underCurrent)
			if tc.underPrevious {
				s.c = s.component(Keys{Current: prev})
				s.put(ScopeOrg, "o1", "OLD", "v")
			}
			report, err := s.component(tc.keys).StartReport(s.ctx)
			if err != nil {
				t.Fatalf("StartReport: %v", err)
			}
			var log bytes.Buffer
			report.Log(slog.New(slog.NewJSONHandler(&log, nil)))
			var reports, warnings int
			for line := range strings.SplitSeq(strings.TrimSpace(log.String()), "\n") {
				var rec map[string]any
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatalf("log line %q: %v", line, err)
				}
				switch rec["msg"] {
				case "secret encryption":
					reports++
					want := map[string]any{"level": "INFO", "key_configured": tc.keys.Current != nil,
						"previous_key_present": tc.keys.Previous != nil, "undecryptable_secrets": tc.wantUndecryptable}
					for k, v := range want {
						if rec[k] != v {
							t.Errorf("%s = %v, want %v in %s", k, rec[k], v, line)
						}
					}
				case "secrets cannot be stored: no encryption key configured":
					warnings++
					if rec["level"] != "WARN" {
						t.Errorf("warning at level %v, want WARN", rec["level"])
					}
				}
			}
			if reports != 1 {
				t.Errorf("%d report lines, want 1; log = %s", reports, log.String())
			}
			if want := map[bool]int{true: 1}[tc.wantWarning]; warnings != want {
				t.Errorf("%d warning lines, want %d; log = %s", warnings, want, log.String())
			}
		})
	}
}
