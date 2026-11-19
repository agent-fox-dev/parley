package telemetry

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The key [audit].write_timeout: how long one ingestion write may take. The
// other keys of [audit] are the audit retention's.
const (
	// ConfigSection is the section the key lives in.
	ConfigSection = "audit"
	// DefaultWriteTimeout is [audit].write_timeout when the file does not
	// set it.
	DefaultWriteTimeout = 2 * time.Second
	// minWriteTimeout is the shortest write limit the file may set.
	minWriteTimeout = 100 * time.Millisecond

	keyWriteTimeout = "write_timeout"
)

// ConfigKey is the key [audit].write_timeout: a duration of at least 100ms
// with a unit of ms, s, m, h or d (default 2s). A value the file sets that
// breaks the rule fails the load with an error naming the key, which stops
// the start.
func ConfigKey() config.Key {
	return config.Key{Section: ConfigSection, Name: keyWriteTimeout, Type: config.String, Default: "2s",
		Validate: validWriteTimeout}
}

func validWriteTimeout(v any) error {
	d, err := parseWriteTimeout(v.(string))
	if err != nil {
		return fmt.Errorf("must be a duration with a unit of ms, s, m, h or d, such as 2s: %w", err)
	}
	if d < minWriteTimeout {
		return errors.New("must be at least 100ms")
	}
	return nil
}

// parseWriteTimeout reads a duration in whole milliseconds (500ms), or in
// the hub's units s, m, h or d, which timefmt.ParseDuration reads.
func parseWriteTimeout(s string) (time.Duration, error) {
	digits, ok := strings.CutSuffix(s, "ms")
	if !ok {
		return timefmt.ParseDuration(s)
	}
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, fmt.Errorf("invalid duration %q: want a whole number followed by ms, s, m, h or d", s)
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n > math.MaxInt64/int64(time.Millisecond) {
		return 0, fmt.Errorf("invalid duration %q: too large", s)
	}
	return time.Duration(n) * time.Millisecond, nil
}

// WriteTimeoutFrom reads [audit].write_timeout from cfg, loaded with
// ConfigKey registered; a key that was not loaded has its default.
func WriteTimeoutFrom(cfg *config.Config) time.Duration {
	if d, err := parseWriteTimeout(cfg.String(ConfigSection, keyWriteTimeout)); err == nil && d > 0 {
		return d
	}
	return DefaultWriteTimeout
}
