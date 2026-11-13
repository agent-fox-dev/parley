package hub_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/scrub"
)

func TestNewLogger_DropsEveryLineBelowTheConfiguredLevel(t *testing.T) {
	t.Parallel()
	levels := []struct {
		name  string
		level slog.Level
	}{
		{"trace", hub.LevelTrace},
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
	}
	for i, configured := range levels {
		t.Run(configured.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			log := hub.NewLogger(&buf, configured.name, scrub.New())
			for _, l := range levels {
				log.Log(context.Background(), l.level, "m-"+l.name)
			}

			var got, want []string
			for _, l := range levels[i:] {
				want = append(want, "m-"+l.name)
			}
			for _, line := range nonEmptyLines(buf.String()) {
				rec := decodeLine(t, line)
				for _, member := range []string{"time", "level", "msg"} {
					if _, ok := rec[member]; !ok {
						t.Errorf("line %q has no member %s", line, member)
					}
				}
				if rec["msg"] == "m-trace" && rec["level"] != "TRACE" {
					t.Errorf("trace line has level %v, want TRACE", rec["level"])
				}
				got = append(got, rec["msg"].(string))
			}
			if !slices.Equal(got, want) {
				t.Errorf("lines written = %v, want %v", got, want)
			}
		})
	}
}

// token is a named string type, as a package's own credential type would be.
type token string

func TestNewLogger_ScrubsTheMessageAndEveryAttribute(t *testing.T) {
	t.Parallel()
	const pat = "parley_pat_abcd1234_0123456789abcdefghijklmnopqrstuv"
	s := scrub.New()
	s.Add("hunter2")
	var buf bytes.Buffer
	log := hub.NewLogger(&buf, "info", s).With("origin", "https://u:hunter2@h/o.git")

	log.Info("clone failed for https://u:hunter2@h/r.git",
		"err", "bad token "+pat,
		"attempt", 3,
		"cause", errors.New("auth hunter2 refused"),
		slog.Group("req", "auth", "Bearer "+pat),
		"header", http.Header{"Authorization": {"Bearer " + pat}, "X-Trace": {"t1"}},
		"basic", http.Header{"Authorization": {"Basic dXNlcjpwYXNz"}},
		"token", token("hunter2"),
		"remotes", []string{"https://u:hunter2@h/x.git"},
		"body", []byte("password=hunter2"),
		"ids", []string{"a", "b"},
		"plain", struct{ N int }{7},
	)

	lines := nonEmptyLines(buf.String())
	if len(lines) != 1 {
		t.Fatalf("%d lines written, want 1: %q", len(lines), buf.String())
	}
	rec := decodeLine(t, lines[0])
	for _, tc := range []struct {
		key  string
		want any
	}{
		{"msg", "clone failed for https://h/r.git"},
		{"err", "bad token [redacted]"},
		{"attempt", float64(3)},
		{"cause", "auth [redacted] refused"},
		{"origin", "https://h/o.git"},
		{"level", "INFO"},
		{"header", "map[Authorization:[[redacted]] X-Trace:[t1]]"},
		{"basic", "map[Authorization:[[redacted]]]"},
		{"token", "[redacted]"},
		{"remotes", "[https://h/x.git]"},
		{"body", "password=[redacted]"},
		// A value that holds no secret keeps its JSON form.
		{"ids", []any{"a", "b"}},
		{"plain", map[string]any{"N": float64(7)}},
	} {
		if !reflect.DeepEqual(rec[tc.key], tc.want) {
			t.Errorf("%s = %#v, want %#v", tc.key, rec[tc.key], tc.want)
		}
	}
	if group, _ := rec["req"].(map[string]any); group["auth"] != "Bearer [redacted]" {
		t.Errorf("req = %v, want auth Bearer [redacted]", rec["req"])
	}
	if _, ok := rec["time"].(string); !ok {
		t.Errorf("time = %v, want the time of the line", rec["time"])
	}
}
