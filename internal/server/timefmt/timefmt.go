// Package timefmt holds the hub's one timestamp format and one duration
// grammar. It imports nothing of the project, so that the configuration
// loader and the server can both use it.
package timefmt

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Layout is RFC 3339 in UTC with whole seconds: 2026-01-02T03:04:05Z.
const Layout = "2006-01-02T15:04:05Z"

// Format writes an instant as RFC 3339 in UTC with whole seconds, cutting
// any fraction of a second off rather than rounding it.
func Format(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(Layout)
}

// Time is a timestamp in a response body; it marshals as Format writes it.
type Time time.Time

// MarshalJSON implements json.Marshaler.
func (t Time) MarshalJSON() ([]byte, error) {
	return []byte(`"` + Format(time.Time(t)) + `"`), nil
}

// UnmarshalJSON implements json.Unmarshaler, reading RFC 3339.
func (t *Time) UnmarshalJSON(b []byte) error {
	s, err := strconv.Unquote(string(b))
	if err != nil {
		return fmt.Errorf("timestamp %s is not a string", b)
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fmt.Errorf("timestamp %q is not RFC 3339", s)
	}
	*t = Time(parsed)
	return nil
}

var units = map[byte]time.Duration{
	's': time.Second,
	'm': time.Minute,
	'h': time.Hour,
	'd': 24 * time.Hour,
}

// ParseDuration reads a duration written as a non-negative whole number
// followed by exactly one of the units s, m, h and d (24 hours): 30s, 15m,
// 24h, 30d. Every other form — empty, no unit, another unit, an upper-case
// unit, a sign, a fraction, spaces, a compound such as 1h30m, or a value too
// large to represent — is an error naming the value, with a zero duration.
func ParseDuration(s string) (time.Duration, error) {
	fail := func() (time.Duration, error) {
		return 0, fmt.Errorf("invalid duration %q: want a whole number followed by s, m, h or d", s)
	}
	if len(s) < 2 {
		return fail()
	}
	unit, ok := units[s[len(s)-1]]
	if !ok {
		return fail()
	}
	digits := s[:len(s)-1]
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return fail()
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n > math.MaxInt64/int64(unit) {
		return fail()
	}
	return time.Duration(n) * unit, nil
}
