package timefmt

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

var timestampPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

func TestFormat_WritesUTCWithWholeSecondsTruncated(t *testing.T) {
	t.Parallel()
	plusTwo := time.FixedZone("+02:00", 2*60*60)
	minusOne := time.FixedZone("-01:00", -60*60)
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"already UTC", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "2026-01-02T03:04:05Z"},
		{"offset with a fraction just under a second", time.Date(2026, 1, 2, 5, 4, 5, 999999999, plusTwo), "2026-01-02T03:04:05Z"},
		{"half a second before the new year, west of UTC", time.Date(2025, 12, 31, 23, 59, 59, 500000000, minusOne), "2026-01-01T00:59:59Z"},
		{"Unix epoch", time.Unix(0, 0), "1970-01-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Format(tc.in)
			if got != tc.want {
				t.Errorf("Format(%v) = %q, want %q", tc.in, got, tc.want)
			}
			if !timestampPattern.MatchString(got) {
				t.Errorf("Format(%v) = %q, does not match %s", tc.in, got, timestampPattern)
			}

			body, err := json.Marshal(struct {
				CreatedAt Time `json:"created_at"`
			}{Time(tc.in)})
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var decoded map[string]string
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("json.Unmarshal(%s): %v", body, err)
			}
			if decoded["created_at"] != tc.want {
				t.Errorf("created_at marshalled as %s, want %q", body, tc.want)
			}
		})
	}
}

func TestParseDuration_ReadsAWholeNumberWithAUnit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"30s", 30 * time.Second},
		{"15m", 15 * time.Minute},
		{"24h", 24 * time.Hour},
		{"30d", 720 * time.Hour},
		{"0s", 0},
		{"1d", 24 * time.Hour},
		{"365d", 8760 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseDuration(tc.in)
			if err != nil {
				t.Fatalf("ParseDuration(%q) error = %v, want nil", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseDuration(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseDuration_RefusesEveryOtherForm(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"no unit", "30"},
		{"unit without a number", "s"},
		{"unknown unit", "30x"},
		{"weeks", "30w"},
		{"negative", "-5m"},
		{"explicit plus sign", "+5m"},
		{"fraction", "1.5h"},
		{"compound", "1h30m"},
		{"leading space", " 30s"},
		{"space before the unit", "30 s"},
		{"trailing space", "30s "},
		{"upper-case unit", "30S"},
		{"two units", "30ss"},
		{"too large to parse", "99999999999999999999d"},
		{"too large to represent in days", "106752d"},
		{"too large to represent in seconds", "9223372036854775807s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseDuration(tc.in)
			if err == nil {
				t.Fatalf("ParseDuration(%q) = %v, nil; want an error", tc.in, got)
			}
			if got != 0 {
				t.Errorf("ParseDuration(%q) duration = %v, want 0", tc.in, got)
			}
			if tc.in != "" && !strings.Contains(err.Error(), tc.in) {
				t.Errorf("ParseDuration(%q) error = %q, want it to contain the value", tc.in, err)
			}
		})
	}
}
