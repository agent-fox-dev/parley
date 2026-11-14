package fork_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/server"
)

func TestResolveTTL_AcceptsFiveMinutesToMaxTTLInWholeUnits(t *testing.T) {
	t.Parallel()
	// A zone east of UTC and a fraction of a second: the answer is still UTC
	// in whole seconds.
	now := time.Date(2026, 1, 1, 2, 0, 0, 999_999_999, time.FixedZone("+02:00", 2*60*60))
	month := config.Forks{DefaultTTL: 168 * time.Hour, MaxTTL: 720 * time.Hour, MaxPerWorkspace: 200}
	day := config.Forks{DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour, MaxPerWorkspace: 200}
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		ttl    string
		limits config.Forks
		want   time.Time // zero when refused
	}{
		{"5m", month, at("2026-01-01T00:05:00Z")},
		{"30d", month, at("2026-01-31T00:00:00Z")},
		{"720h", month, at("2026-01-31T00:00:00Z")},
		{"43200m", month, at("2026-01-31T00:00:00Z")},
		{"90m", month, at("2026-01-01T01:30:00Z")},
		{"4m", month, time.Time{}},
		{"31d", month, time.Time{}},
		{"721h", month, time.Time{}},
		{"43201m", month, time.Time{}},
		{"300s", month, time.Time{}},
		{"0m", month, time.Time{}},
		{"-1h", month, time.Time{}},
		{"1.5h", month, time.Time{}},
		{"1h30m", month, time.Time{}},
		{"10", month, time.Time{}},
		{"", month, time.Time{}},
		{"24h", day, at("2026-01-02T00:00:00Z")},
		{"25h", day, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.ttl+" of "+tc.limits.MaxTTL.String(), func(t *testing.T) {
			t.Parallel()
			got, err := fork.ResolveTTL(tc.ttl, now, tc.limits)
			if tc.want.IsZero() {
				answer, ok := errors.AsType[*server.Error](err)
				if !ok || answer.Status() != 400 || answer.Type() != "invalid_request" || !strings.HasPrefix(answer.Message(), "ttl: ") {
					t.Fatalf("ResolveTTL(%q) = %v, %v; want a 400 invalid_request naming ttl", tc.ttl, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveTTL(%q) error = %v", tc.ttl, err)
			}
			if !got.Equal(tc.want) || got.Location() != time.UTC || got.Nanosecond() != 0 {
				t.Errorf("ResolveTTL(%q) = %v, want %v in whole seconds UTC", tc.ttl, got, tc.want)
			}
		})
	}
}

func TestDefaultExpiry_AddsTheDefaultTTL(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 500_000_000, time.UTC)
	got := fork.DefaultExpiry(now, config.Forks{DefaultTTL: 168 * time.Hour, MaxTTL: 720 * time.Hour})
	if want := time.Date(2026, 1, 8, 0, 0, 0, 0, time.UTC); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("DefaultExpiry = %v, want %v", got, want)
	}
}
