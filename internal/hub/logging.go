package hub

import (
	"io"
	"log/slog"
	"strings"

	"github.com/agent-fox-dev/parley/internal/scrub"
)

// LevelTrace is the level below debug; with [logging].level = "trace" every
// line is written. Its lines carry "level":"TRACE".
const LevelTrace = slog.Level(-8)

// NewLogger returns the hub's logger: one JSON object per line on w, lines
// below level dropped, and the message and every string attribute, also in
// groups and also an error's text, passed through s before they are written.
// Attribute keys, the time and the level are written as they are. level is
// trace, debug, info, warn or error, in any letter case; anything else means
// info.
func NewLogger(w io.Writer, level string, s *scrub.Scrubber) *slog.Logger {
	var v slog.LevelVar
	v.Set(parseLevel(level))
	return newLogger(w, &v, s)
}

// newLogger is NewLogger with a level that can change while the logger is in
// use: the hub logs a failed configuration load at info before it knows the
// configured level.
func newLogger(w io.Writer, level slog.Leveler, s *scrub.Scrubber) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		// The JSON handler hands every attribute to ReplaceAttr, the message
		// and the attributes of With and of groups included, with LogValuers
		// already resolved, so this one place sees every string it writes.
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			switch a.Value.Kind() {
			case slog.KindString:
				a.Value = slog.StringValue(s.Scrub(a.Value.String()))
			case slog.KindAny:
				switch v := a.Value.Any().(type) {
				case slog.Level:
					if len(groups) == 0 && a.Key == slog.LevelKey && v == LevelTrace {
						a.Value = slog.StringValue("TRACE")
					}
				case error:
					a.Value = slog.StringValue(s.Scrub(v.Error()))
				}
			}
			return a
		},
	}))
}

// parseLevel reads a [logging].level value; anything it does not know is
// info.
func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "trace":
		return LevelTrace
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}
