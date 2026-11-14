package event

import (
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The [events] section of the configuration file.
const (
	// SectionName is the section the event keys live in.
	SectionName = "events"
	// DefaultRetention is [events].retention when the file does not set it.
	DefaultRetention = 30 * 24 * time.Hour
	// DefaultMaxStreams is [events].max_sse_connections when the file does
	// not set it.
	DefaultMaxStreams = 500
	// minRetention is the shortest retention the file may set: a consumer
	// must be able to stay away for at least that long.
	minRetention = time.Hour
)

// Settings are the values of the [events] section.
type Settings struct {
	// Retention is how long an event stays in the event log
	// ([events].retention).
	Retention time.Duration
	// MaxStreams is the number of event streams that may be open at once
	// ([events].max_sse_connections).
	MaxStreams int
}

// Register registers the keys of [events] with r: retention, a duration
// with a unit of s, m, h or d of at least 1h (default 30d), and
// max_sse_connections, an integer of at least 1 (default 500). A value the
// file sets that breaks its rule fails the load with an error naming the key,
// which stops the start.
func Register(r *config.Registry) error {
	return errors.Join(
		r.Register(config.Key{Section: SectionName, Name: "retention", Type: config.String, Default: "30d",
			Validate: validRetention}),
		r.Register(config.Key{Section: SectionName, Name: "max_sse_connections", Type: config.Int,
			Default: DefaultMaxStreams, Validate: validMaxStreams}),
	)
}

func validRetention(v any) error {
	d, err := timefmt.ParseDuration(v.(string))
	if err != nil {
		return fmt.Errorf("must be a duration with a unit of s, m, h or d, such as 30d: %w", err)
	}
	if d < minRetention {
		return errors.New("must be at least 1h")
	}
	return nil
}

func validMaxStreams(v any) error {
	if v.(int) < 1 {
		return errors.New("must be at least 1")
	}
	return nil
}

// SettingsFrom reads the [events] section of cfg, loaded with the keys
// Register registers; a key that was not loaded has its default.
func SettingsFrom(cfg *config.Config) Settings {
	s := Settings{Retention: DefaultRetention, MaxStreams: DefaultMaxStreams}
	if d, err := timefmt.ParseDuration(cfg.String(SectionName, "retention")); err == nil && d > 0 {
		s.Retention = d
	}
	if n := cfg.Int(SectionName, "max_sse_connections"); n > 0 {
		s.MaxStreams = n
	}
	return s
}

// Retention is the loaded [events].retention: how long an event stays in the
// event log. The retention run, which expires the log, reads it.
func (c *Component) Retention() time.Duration {
	if c.deps.Retention <= 0 {
		return DefaultRetention
	}
	return c.deps.Retention
}
