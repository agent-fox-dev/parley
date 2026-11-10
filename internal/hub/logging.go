package hub

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"github.com/agent-fox-dev/parley/internal/scrub"
)

// LevelTrace is the level below debug; with it every line is written.
const LevelTrace = slog.Level(-8)

// levelOf maps a configured level name (trace, debug, info, warn, error, in
// any letter case) to its slog level; an unknown name is info.
func levelOf(name string) slog.Level {
	switch strings.ToLower(name) {
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

// NewLogger builds the hub's logger: one JSON object per line on w with
// time, level and msg, lines below level dropped, and the message and every
// string attribute passed through the scrubber before they are written.
func NewLogger(w io.Writer, level string, s *scrub.Scrubber) *slog.Logger {
	json := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: levelOf(level),
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.LevelKey {
				if l, ok := a.Value.Any().(slog.Level); ok && l <= LevelTrace {
					return slog.String(slog.LevelKey, "TRACE")
				}
			}
			return a
		},
	})
	return slog.New(&scrubHandler{next: json, scrubber: s})
}

// scrubHandler scrubs a record's message and string attributes and hands
// the record on.
type scrubHandler struct {
	next     slog.Handler
	scrubber *scrub.Scrubber
}

func (h *scrubHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *scrubHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, h.scrubber.Scrub(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.attr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *scrubHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = h.attr(a)
	}
	return &scrubHandler{next: h.next.WithAttrs(scrubbed), scrubber: h.scrubber}
}

func (h *scrubHandler) WithGroup(name string) slog.Handler {
	return &scrubHandler{next: h.next.WithGroup(name), scrubber: h.scrubber}
}

// attr scrubs a string attribute, an error and every attribute of a group.
func (h *scrubHandler) attr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.scrubber.Scrub(a.Value.String()))
	case slog.KindGroup:
		group := a.Value.Group()
		out := make([]any, len(group))
		for i, g := range group {
			out[i] = h.attr(g)
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, h.scrubber.Scrub(err.Error()))
		}
	}
	return a
}
