package telemetry_test

import (
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/telemetry"
)

func TestNormaliseUUID_AnyLetterCaseBecomesLowerCase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"},
		{"aAaAaAaA-bBbB-4cCc-8DdD-eEeEeEeEeEeE", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"},
		{"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"},
	}
	for _, c := range cases {
		if got, ok := telemetry.NormaliseUUID(c.in); !ok || got != c.want {
			t.Errorf("NormaliseUUID(%q) = %q, %v, want %q", c.in, got, ok, c.want)
		}
	}
	for _, bad := range []string{"", "not-a-uuid", "0123456789abcdef0123456789abcdef", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeeg", "{aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeee}"} {
		if got, ok := telemetry.NormaliseUUID(bad); ok {
			t.Errorf("NormaliseUUID(%q) = %q, want refused", bad, got)
		}
	}
}

func TestNormaliseTimestamp_AnyOffsetBecomesUTCCutToMicroseconds(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026-01-02T05:04:05.1234567+02:00", "2026-01-02T03:04:05.123456Z"},
		{"2026-01-02T03:04:05.500Z", "2026-01-02T03:04:05.5Z"},
		{"2026-01-02T03:04:05Z", "2026-01-02T03:04:05Z"},
		{"2026-01-01T22:04:05-05:00", "2026-01-02T03:04:05Z"},
		{"2026-01-02T03:04:05.0000009Z", "2026-01-02T03:04:05Z"},
		{"9999-12-31T23:59:59.999999Z", "9999-12-31T23:59:59.999999Z"},
		{"0000-01-01T00:30:00-01:00", "0000-01-01T01:30:00Z"},
	}
	for _, c := range cases {
		if got, ok := telemetry.NormaliseTimestamp(c.in); !ok || got != c.want {
			t.Errorf("NormaliseTimestamp(%q) = %q, %v, want %q", c.in, got, ok, c.want)
		}
	}
	for _, bad := range []string{
		"", "2026-01-02 03:04:05", "2026-01-02", "yesterday", "2026-01-02T03:04:05",
		// In UTC, these fall after the year 9999 and before the year 0000.
		"9999-12-31T23:59:59-01:00", "0000-01-01T00:30:00+01:00",
		// RFC 3339 separates the fraction of a second with a full stop only.
		"2026-01-02T03:04:05,5Z",
	} {
		if got, ok := telemetry.NormaliseTimestamp(bad); ok {
			t.Errorf("NormaliseTimestamp(%q) = %q, want refused", bad, got)
		}
	}
}

func TestCount_TakesWholeNumbersOfAtLeastZero(t *testing.T) {
	for raw, want := range map[string]int64{"0": 0, "12": 12, "1500": 1500} {
		got, err := telemetry.Count(raw)
		if err != nil || got == nil || *got != want {
			t.Errorf("Count(%s) = %v, %v, want %d", raw, got, err, want)
		}
	}
	if got, err := telemetry.Count("null"); err != nil || got != nil {
		t.Errorf("Count(null) = %v, %v, want absent", got, err)
	}
	for _, raw := range []string{"-1", "-0", "1.5", `"3"`, "true", "{}"} {
		if _, err := telemetry.Count(raw); err == nil || !strings.Contains(err.Error(), "duration_ms") {
			t.Errorf("Count(%s) = %v, want an error naming the member", raw, err)
		}
	}
}
