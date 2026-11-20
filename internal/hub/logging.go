package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/server"
)

// LevelTrace is the level below debug; with [logging].level = "trace" every
// line is written. Its lines carry "level":"TRACE".
const LevelTrace = slog.Level(-8)

// NewLogger returns the hub's logger: one JSON object per line on w, its
// time in UTC and its level one of TRACE, DEBUG, INFO, WARN and ERROR, lines
// below level dropped, and the message and every attribute, also in groups,
// passed through s before they are written. A string is written scrubbed and
// an error as its scrubbed text. Any other value is scrubbed as its fmt text
// ([]byte as text): when scrubbing changes that text, removing a secret or
// cutting it to scrub.MaxLen, the scrubbed text is written in its place.
// Otherwise every string of its JSON form, map keys included, is scrubbed,
// so that what fmt does not show (a MarshalText result, a value behind a
// pointer) is scrubbed too, and a value holding no secret keeps its JSON
// form. Numbers, booleans and durations are written as they are. The values
// of an http.Header's Authorization and Proxy-Authorization headers are
// never written. Attribute keys are written as they are. A line logged with
// the context of a request the HTTP server handles carries that request's id
// as request_id, unless it already has a request_id attribute. A line whose
// write fails is dropped: the logger never returns the failure. level is
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
	return slog.New(requestIDHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		// The JSON handler hands every attribute to ReplaceAttr, the message
		// and the attributes of With and of groups included, with LogValuers
		// already resolved, so this one place sees every string it writes.
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			switch a.Value.Kind() {
			case slog.KindTime:
				// UTC, so that the logs of hosts in different zones sort
				// together.
				if len(groups) == 0 && a.Key == slog.TimeKey {
					a.Value = slog.TimeValue(a.Value.Time().UTC())
				}
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
					} else if rendered, changed := scrubJSON(s, v); changed {
						a.Value = slog.AnyValue(rendered)
					}
				}
			}
			return a
		},
	})})
}

// requestIDHandler adds the id of the request whose context a line is logged
// with, so that every line written while a request is handled can be traced
// to it. A line that already carries request_id, such as the access-log
// line, keeps its own, so that no line holds the member twice.
type requestIDHandler struct{ slog.Handler }

func (h requestIDHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := server.RequestID(ctx); id != "" && !hasAttr(r, "request_id") {
		r = r.Clone()
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h requestIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return requestIDHandler{h.Handler.WithAttrs(attrs)}
}

func (h requestIDHandler) WithGroup(name string) slog.Handler {
	return requestIDHandler{h.Handler.WithGroup(name)}
}

// hasAttr reports whether r has an attribute named key.
func hasAttr(r slog.Record, key string) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		found = a.Key == key
		return !found
	})
	return found
}

// scrubJSON scrubs every string of v's JSON form, the keys of its objects
// included, and returns that form and whether scrubbing changed it. A value
// that has no JSON form is left to the handler.
func scrubJSON(s *scrub.Scrubber, v any) (any, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // numbers are written back as they were
	var rendered any
	if dec.Decode(&rendered) != nil {
		return nil, false
	}
	return scrubStrings(s, rendered)
}

// scrubStrings scrubs every string of a decoded JSON value.
func scrubStrings(s *scrub.Scrubber, v any) (any, bool) {
	switch x := v.(type) {
	case string:
		scrubbed := s.Scrub(x)
		return scrubbed, scrubbed != x
	case []any:
		changed := false
		for i, e := range x {
			var c bool
			x[i], c = scrubStrings(s, e)
			changed = changed || c
		}
		return x, changed
	case map[string]any:
		out := make(map[string]any, len(x))
		changed := false
		for k, e := range x {
			key, ck := scrubStrings(s, k)
			value, cv := scrubStrings(s, e)
			out[key.(string)] = value
			changed = changed || ck || cv
		}
		return out, changed
	}
	return v, false
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
