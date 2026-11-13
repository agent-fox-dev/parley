package hub

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/scrub"
)

// LevelTrace is the level below debug; with [logging].level = "trace" every
// line is written. Its lines carry "level":"TRACE".
const LevelTrace = slog.Level(-8)

// NewLogger returns the hub's logger: one JSON object per line on w, lines
// below level dropped, and the message and every attribute, also in groups,
// passed through s before they are written. A string is written scrubbed and
// an error as its scrubbed text. Any other value is scrubbed as its fmt text
// ([]byte as text): when scrubbing changes that text, removing a secret or
// cutting it to scrub.MaxLen, the scrubbed text is written in its place, and
// otherwise the value keeps its JSON form. The values of an http.Header's
// Authorization and Proxy-Authorization headers are never written. Attribute
// keys, the time and the level are written as they are. level is trace,
// debug, info, warn or error, in any letter case; anything else means info.
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
				default:
					if text, changed := scrubValue(s, v); changed {
						a.Value = slog.StringValue(text)
					}
				}
			}
			return a
		},
	}))
}

// credentialHeaders are the headers whose values are credentials whatever
// their shape: a Basic or a foreign bearer credential is not one the
// scrubber knows.
var credentialHeaders = []string{"Authorization", "Proxy-Authorization"}

// scrubValue returns v's text scrubbed, and whether that is not v's own
// text, so that a value holding no secret can keep its JSON form.
func scrubValue(s *scrub.Scrubber, v any) (string, bool) {
	changed := false
	var text string
	switch x := v.(type) {
	case []byte:
		text = string(x)
	case http.Header:
		for name := range x {
			if slices.Contains(credentialHeaders, http.CanonicalHeaderKey(name)) {
				if !changed {
					x, changed = x.Clone(), true
				}
				x[name] = []string{scrub.Marker}
			}
		}
		text = fmt.Sprint(x)
	default:
		text = fmt.Sprint(v)
	}
	scrubbed := s.Scrub(text)
	return scrubbed, changed || scrubbed != text
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
