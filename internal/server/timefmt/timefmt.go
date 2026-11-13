// Package timefmt holds the one timestamp format and the one duration
// grammar of Parley: timestamps are written as RFC 3339 in UTC with whole
// seconds, and durations are read as a whole number with one unit suffix.
//
// It imports nothing of the project, so that the configuration loader can
// use it as well as the REST API without an import cycle.
package timefmt

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// layout is RFC 3339 without a fraction of a second; applied to a UTC time it
// always ends in "Z".
const layout = "2006-01-02T15:04:05Z07:00"

// Format writes t as RFC 3339 in UTC with whole seconds and the suffix Z,
// such as 2026-01-02T03:04:05Z. A fraction of a second is cut off, never
// rounded, so a timestamp is never later than the instant it stands for.
func Format(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(layout)
}

// Time is a timestamp member of a response body. It marshals to JSON as the
// string Format writes, so every response writes its timestamps the same
// way. Convert with Time(t) and time.Time(ts).
type Time time.Time

// MarshalJSON writes the timestamp as a JSON string in the form of Format.
func (t Time) MarshalJSON() ([]byte, error) {
	return strconv.AppendQuote(nil, Format(time.Time(t))), nil
}

// units maps each accepted suffix to its length; a day is 24 hours.
var units = map[byte]time.Duration{
	's': time.Second,
	'm': time.Minute,
	'h': time.Hour,
	'd': 24 * time.Hour,
}

// ParseDuration reads a duration written as a non-negative whole number
// followed by exactly one of the units s, m, h or d (24 hours), such as 30s,
// 15m, 24h or 30d. Any other form (empty, no unit, an unknown or upper-case
// unit, a sign, a fraction, spaces, a compound such as 1h30m, or a value too
// large for a time.Duration) is refused with an error that quotes the value,
// and a zero duration.
func ParseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, invalidDuration(s, "want a whole number followed by s, m, h or d")
	}
	digits, suffix := s[:len(s)-1], s[len(s)-1]
	unit, ok := units[suffix]
	if !ok {
		return 0, invalidDuration(s, "the unit must be one of s, m, h or d")
	}
	// strconv accepts a leading sign and underscores in some forms; the
	// grammar allows digits only, so check them first.
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, invalidDuration(s, "want a whole number followed by s, m, h or d")
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n > math.MaxInt64/int64(unit) {
		return 0, invalidDuration(s, "too large")
	}
	return time.Duration(n) * unit, nil
}

func invalidDuration(s, reason string) error {
	return fmt.Errorf("invalid duration %q: %s", s, reason)
}
