package audit

import (
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The sections the retention keys live in. [audit] also holds the
// telemetry's write_timeout, so the keys are registered one by one, never
// the section as a whole.
const (
	AuditSection    = "audit"
	SessionsSection = "sessions"
)

const day = 24 * time.Hour

// Settings are the retention limits the configuration file sets.
type Settings struct {
	// MaxAge is how long a hub event or a run record is kept
	// ([audit].max_age).
	MaxAge time.Duration
	// MaxRuns is how many runs a workspace keeps the records of
	// ([audit].max_runs).
	MaxRuns int
	// TraceMaxAge is how long a trace is kept ([audit].trace_max_age).
	TraceMaxAge time.Duration
	// PostmortemMaxAge is how long a postmortem is kept
	// ([audit].postmortem_max_age).
	PostmortemMaxAge time.Duration
	// OrphanRetention is how long the records of a workspace that is gone
	// are kept ([audit].orphan_retention).
	OrphanRetention time.Duration
	// SessionMaxAge is how long an ended session is kept
	// ([sessions].max_age).
	SessionMaxAge time.Duration
	// SessionMaxActiveAge is how long a session may stay active before it
	// is timed out ([sessions].max_active_age).
	SessionMaxActiveAge time.Duration
}

// durationKey is a retention key whose value is a duration.
type durationKey struct {
	section, name string
	def           time.Duration
	min           time.Duration
	field         func(*Settings) *time.Duration
}

var durationKeys = []durationKey{
	{AuditSection, "max_age", 90 * day, day, func(s *Settings) *time.Duration { return &s.MaxAge }},
	{AuditSection, "trace_max_age", 30 * day, day, func(s *Settings) *time.Duration { return &s.TraceMaxAge }},
	{AuditSection, "postmortem_max_age", 180 * day, day, func(s *Settings) *time.Duration { return &s.PostmortemMaxAge }},
	{AuditSection, "orphan_retention", 30 * day, day, func(s *Settings) *time.Duration { return &s.OrphanRetention }},
	{SessionsSection, "max_age", 90 * day, day, func(s *Settings) *time.Duration { return &s.SessionMaxAge }},
	{SessionsSection, "max_active_age", 7 * day, time.Hour, func(s *Settings) *time.Duration { return &s.SessionMaxActiveAge }},
}

// The integer key.
const (
	maxRunsKey     = "max_runs"
	defaultMaxRuns = 50
)

// Register registers the seven retention keys with r: in [audit] max_age
// (default 90d), trace_max_age (30d), postmortem_max_age (180d) and
// orphan_retention (30d), durations of at least 1d, and max_runs, an integer
// of at least 1 (default 50); in [sessions] max_age (90d), a duration of at
// least 1d, and max_active_age (7d), a duration of at least 1h. A duration
// is a whole number with a unit of s, m, h or d. A value the file sets that
// breaks its rule fails the load with an error naming the key, which stops
// the start.
func Register(r *config.Registry) error {
	errs := make([]error, 0, len(durationKeys)+1)
	for _, k := range durationKeys {
		errs = append(errs, r.Register(config.Key{Section: k.section, Name: k.name, Type: config.String,
			Default: formatDuration(k.def), Validate: atLeast(k.min)}))
	}
	errs = append(errs, r.Register(config.Key{Section: AuditSection, Name: maxRunsKey, Type: config.Int,
		Default: defaultMaxRuns, Validate: positive}))
	return errors.Join(errs...)
}

// atLeast is the rule of a duration key whose shortest value is limit.
func atLeast(limit time.Duration) func(any) error {
	return func(v any) error {
		d, err := timefmt.ParseDuration(v.(string))
		if err != nil {
			return fmt.Errorf("must be a duration with a unit of s, m, h or d, such as 90d: %w", err)
		}
		if d < limit {
			return fmt.Errorf("must be at least %s", formatDuration(limit))
		}
		return nil
	}
}

func positive(v any) error {
	if v.(int) < 1 {
		return errors.New("must be at least 1")
	}
	return nil
}

// SettingsFrom reads the retention keys of cfg, loaded with the keys
// Register registers; a key that was not loaded has its default.
func SettingsFrom(cfg *config.Config) Settings {
	s := Settings{MaxRuns: defaultMaxRuns}
	for _, k := range durationKeys {
		d, err := timefmt.ParseDuration(cfg.String(k.section, k.name))
		if err != nil || d <= 0 {
			d = k.def
		}
		*k.field(&s) = d
	}
	if n := cfg.Int(AuditSection, maxRunsKey); n > 0 {
		s.MaxRuns = n
	}
	return s
}

// NewSettings reads the retention keys of cfg and checks them against
// eventRetention, the loaded [events].retention: the event log is a part of
// the audit log, so an event must not stay in the log longer than the audit
// keeps it. A longer event retention is an error naming both keys, which
// stops the start.
func NewSettings(cfg *config.Config, eventRetention time.Duration) (Settings, error) {
	s := SettingsFrom(cfg)
	if eventRetention > s.MaxAge {
		return Settings{}, fmt.Errorf("events.retention (%s) must not be longer than audit.max_age (%s)",
			formatDuration(eventRetention), formatDuration(s.MaxAge))
	}
	return s, nil
}

// formatDuration writes d in the form the configuration file takes, in the
// largest unit that divides it.
func formatDuration(d time.Duration) string {
	for _, u := range []struct {
		unit   time.Duration
		suffix string
	}{{day, "d"}, {time.Hour, "h"}, {time.Minute, "m"}} {
		if d%u.unit == 0 {
			return fmt.Sprintf("%d%s", d/u.unit, u.suffix)
		}
	}
	return fmt.Sprintf("%ds", d/time.Second)
}
