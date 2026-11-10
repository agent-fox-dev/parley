package timefmt

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

var timestampForm = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

func TestFormat_UTCWholeSecondsTruncated(t *testing.T) {
	plus2 := time.FixedZone("+02:00", 2*3600)
	minus1 := time.FixedZone("-01:00", -3600)
	tests := []struct {
		in   time.Time
		want string
	}{
		{time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "2026-01-02T03:04:05Z"},
		{time.Date(2026, 1, 2, 5, 4, 5, 999999999, plus2), "2026-01-02T03:04:05Z"},
		{time.Date(2025, 12, 31, 23, 59, 59, 500000000, minus1), "2026-01-01T00:59:59Z"},
		{time.Unix(0, 0), "1970-01-01T00:00:00Z"},
	}
	for _, tt := range tests {
		got := Format(tt.in)
		if got != tt.want || !timestampForm.MatchString(got) {
			t.Errorf("Format(%v) = %q, want %q", tt.in, got, tt.want)
		}
		b, err := json.Marshal(struct {
			At Time `json:"at"`
		}{Time(tt.in)})
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != `{"at":"`+tt.want+`"}` {
			t.Errorf("marshalled %s, want at %q", b, tt.want)
		}
	}
}

func TestParseDuration_NumberWithAUnit(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"30s", 30 * time.Second}, {"15m", 15 * time.Minute}, {"24h", 24 * time.Hour},
		{"30d", 720 * time.Hour}, {"0s", 0}, {"1d", 24 * time.Hour}, {"365d", 8760 * time.Hour},
	}
	for _, tt := range tests {
		d, err := ParseDuration(tt.in)
		if err != nil || d != tt.want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", tt.in, d, err, tt.want)
		}
	}
}

func TestParseDuration_RefusesEveryOtherForm(t *testing.T) {
	for _, in := range []string{"", "30", "s", "30x", "30w", "-5m", "1.5h", "1h30m", " 30s", "30 s", "30S", "99999999999999999999d", "9999999999999999d"} {
		d, err := ParseDuration(in)
		if err == nil {
			t.Errorf("ParseDuration(%q) = %v, want an error", in, d)
			continue
		}
		if d != 0 {
			t.Errorf("ParseDuration(%q) returned %v with its error, want 0", in, d)
		}
		if in != "" && !strings.Contains(err.Error(), in) {
			t.Errorf("error %q does not name %q", err, in)
		}
	}
}
