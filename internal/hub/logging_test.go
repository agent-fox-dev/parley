package hub_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/server"
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

func TestNewLogger_WritesOneJSONLinePerRecordInUTCWithTheLevelsName(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := hub.NewLogger(&buf, "trace", scrub.New())
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.FixedZone("", 2*60*60))
	levels := []slog.Level{hub.LevelTrace, slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}
	for i, level := range levels {
		rec := slog.NewRecord(at, level, "line", 0)
		if i == len(levels)-1 {
			rec = slog.NewRecord(at, level, "two\nlines", 0)
			rec.AddAttrs(slog.String("v", "a\nb"))
		}
		if err := log.Handler().Handle(context.Background(), rec); err != nil {
			t.Fatalf("handle %v: %v", level, err)
		}
	}

	out := buf.String()
	if n := strings.Count(out, "\n"); n != len(levels) {
		t.Fatalf("output has %d newlines, want %d: %q", n, len(levels), out)
	}
	var got []string
	var last map[string]any
	for line := range strings.Lines(out) {
		last = decodeLine(t, line)
		stamp, _ := last["time"].(string)
		when, err := time.Parse(time.RFC3339, stamp)
		if !strings.HasSuffix(stamp, "Z") || err != nil || !when.Equal(at) {
			t.Errorf("time = %q, want %s in UTC ending in Z", stamp, at.UTC().Format(time.RFC3339))
		}
		level, _ := last["level"].(string)
		got = append(got, level)
	}
	if want := []string{"TRACE", "DEBUG", "INFO", "WARN", "ERROR"}; !slices.Equal(got, want) {
		t.Errorf("levels = %q, want %q", got, want)
	}
	if last["msg"] != "two\nlines" || last["v"] != "a\nb" {
		t.Errorf("last line has msg %q and v %q, want the line breaks kept inside the strings", last["msg"], last["v"])
	}
}

func TestNewLogger_AddsTheRequestIDOnlyToLinesLoggedWithTheRequestsContext(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := hub.NewLogger(&buf, "info", scrub.New())
	deeper := func(ctx context.Context) { log.InfoContext(ctx, "deeper") }
	srv := server.New(server.Options{Logger: log})
	srv.Register(server.Route{Method: http.MethodGet, Pattern: "/probe", Public: true, Handler: func(w http.ResponseWriter, r *http.Request) error {
		log.InfoContext(r.Context(), "in handler")
		deeper(r.Context())
		w.WriteHeader(http.StatusNoContent)
		return nil
	}})
	h := srv.Handler()

	for range 2 {
		buf.Reset()
		res := httptest.NewRecorder()
		h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/v1/probe", nil))
		id := res.Header().Get("X-Request-ID")
		if id == "" {
			t.Fatal("the response has no X-Request-ID")
		}
		seen := map[string]bool{}
		for _, line := range nonEmptyLines(buf.String()) {
			rec := decodeLine(t, line)
			msg, _ := rec["msg"].(string)
			seen[msg] = true
			if rec["request_id"] != id {
				t.Errorf("line %q has request_id %v, want %s", msg, rec["request_id"], id)
			}
			if n := strings.Count(line, `"request_id"`); n != 1 {
				t.Errorf("line %q holds request_id %d times, want once", msg, n)
			}
		}
		for _, msg := range []string{"in handler", "deeper", "request"} {
			if !seen[msg] {
				t.Errorf("no line %q was written; log = %q", msg, buf.String())
			}
		}
	}

	buf.Reset()
	log.InfoContext(context.Background(), "background")
	log.Info("no context")
	for _, line := range nonEmptyLines(buf.String()) {
		if rec := decodeLine(t, line); rec["request_id"] != nil {
			t.Errorf("line %q has request_id %v, want none", rec["msg"], rec["request_id"])
		}
	}
}

// stringer is a value written as the text of its String method.
type stringer string

func (s stringer) String() string { return "tok " + string(s) }

// textual is a value written as the text of its MarshalText method, which
// its fmt form does not show.
type textual struct{ n int }

func (v textual) MarshalText() ([]byte, error) { return []byte("tok hunter" + strconv.Itoa(v.n)), nil }

// newPAT returns a personal access token in the hub's format.
func newPAT(t *testing.T) string {
	t.Helper()
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8+1+32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	b[8] = '_'
	return "parley_pat_" + string(b)
}

func TestNewLogger_ScrubsEveryStringInAValueOfAnyType(t *testing.T) {
	t.Parallel()
	pat := newPAT(t)
	type credential struct{ Token string }
	type holder struct{ Cred *credential }
	type payload struct{ Body []byte }
	type wrapped struct{ Payload *payload }
	for _, tc := range []struct {
		name  string
		value any
		want  string // a text the line holds in place of the secret
	}{
		{"an error", errors.New("auth hunter2 failed"), `"v":"auth [redacted] failed"`},
		{"a wrapped error with a URL", fmt.Errorf("wrap: %w", errors.New("https://u:hunter2@h/r.git")), "https://h/r.git"},
		{"a group", slog.GroupValue(slog.String("inner", "hunter2"), slog.Int("n", 7)), scrub.Marker},
		{"a list", []string{"a", "hunter2"}, scrub.Marker},
		{"a map", map[string]string{"k": "hunter2"}, scrub.Marker},
		{"a byte slice", []byte("hunter2"), scrub.Marker},
		{"a value with a String method", stringer("hunter2"), scrub.Marker},
		{"a value written as text", textual{2}, scrub.Marker},
		{"a token in a struct field", credential{pat}, scrub.Marker},
		{"a token behind a pointer field", holder{&credential{pat}}, scrub.Marker},
		{"a byte slice in a struct field", payload{[]byte("password=hunter2")}, "password=" + scrub.Marker},
		{"a byte slice beside a secret string", struct {
			Body []byte
			Note string
		}{[]byte("hunter2"), "hunter2"}, scrub.Marker},
		{"byte slices in a map", map[string][]byte{"k": []byte("hunter2")}, scrub.Marker},
		{"a byte slice in a list of structs", []payload{{[]byte("a")}, {[]byte("password=hunter2")}}, "password=" + scrub.Marker},
		{"a byte slice behind a pointer field", wrapped{&payload{[]byte("password=hunter2")}}, "password=" + scrub.Marker},
		// Both keys scrub to the same text, and both values are kept.
		{"keys that scrub to the same text", map[textual]string{{2}: "first", {3}: "second"},
			`{"tok [redacted]":"first","tok [redacted]#2":"second"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := scrub.New()
			s.Add("hunter2")
			s.Add("hunter3")
			var buf bytes.Buffer
			hub.NewLogger(&buf, "info", s).Info("m", "v", tc.value, "count", 3, "ok", true, "wait", 2*time.Second)

			line := buf.String()
			for _, secret := range []string{"hunter2", "hunter3", pat} {
				// fmt writes a nested byte slice as its byte values.
				decimal := strings.Trim(fmt.Sprint([]byte(secret)), "[]")
				if strings.Contains(line, secret) || strings.Contains(line, decimal) {
					t.Errorf("line %s holds the secret %s", line, secret)
				}
			}
			if !strings.Contains(line, tc.want) {
				t.Errorf("line %s does not hold %s", line, tc.want)
			}
			rec := decodeLine(t, line)
			if rec["count"] != float64(3) || rec["ok"] != true || rec["wait"] != float64(2*time.Second) {
				t.Errorf("count, ok, wait = %v, %v, %v, want 3, true, %d", rec["count"], rec["ok"], rec["wait"], 2*time.Second)
			}
			if _, ok := rec["v"]; !ok {
				t.Errorf("line %s has no member v", line)
			}
		})
	}
}

// failingStderr passes writes to buf until the listening line has been
// written, and fails every write after it.
type failingStderr struct {
	buf    *syncBuffer
	failed atomic.Int64
	broken atomic.Bool
}

func (w *failingStderr) Write(p []byte) (int, error) {
	if w.broken.Load() {
		w.failed.Add(1)
		return 0, errors.New("stderr is gone")
	}
	if bytes.Contains(p, []byte(`"msg":"listening"`)) {
		w.broken.Store(true)
	}
	return w.buf.Write(p)
}

func TestRun_GoesOnWhenALogLineCannotBeWritten(t *testing.T) {
	t.Parallel()
	stderr := &failingStderr{buf: newSyncBuffer()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := make(chan int, 1)
	go func() {
		code <- hub.Run(ctx, hub.Options{WorkDir: workDir(t, localConfig+"[logging]\nlevel = \"trace\"\n"), Stdout: io.Discard, Stderr: stderr})
	}()
	addr, _ := stderr.buf.waitForMsg(t, "listening")["addr"].(string)

	var version struct {
		Version string `json:"version"`
	}
	if status := getJSON(t, "http://"+addr+"/version", &version); status != http.StatusOK || version.Version == "" {
		t.Errorf("GET /version = %d with version %q, want 200 with a version", status, version.Version)
	}
	var envelope map[string]any
	if status := getJSON(t, "http://"+addr+"/api/v1/no-such-route", &envelope); status != http.StatusNotFound {
		t.Errorf("GET /api/v1/no-such-route = %d, want 404", status)
	}
	if stderr.failed.Load() == 0 {
		t.Error("no log line was written after the listening line, so no failing write was tried")
	}
	cancel()
	select {
	case c := <-code:
		if c != 0 {
			t.Errorf("the hub exited %d after its stop, want 0", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hub did not stop within 10 s")
	}
}

func TestRun_KeepsTheProcesssDefaultLoggerOffItsStderr(t *testing.T) {
	// Not parallel: it writes through the process's default logger, which
	// another test's hub would otherwise be the one to change.
	var stderr bytes.Buffer
	if code := hub.Run(t.Context(), hub.Options{WorkDir: workDir(t, localConfig+"[audit]\nmax_runs = 0\n"), Stderr: &stderr}); code != 1 {
		t.Fatalf("Run returned %d, want 1", code)
	}
	slog.Info("a line of another logger in the same process")
	if lines := strings.Split(strings.TrimSpace(stderr.String()), "\n"); len(lines) != 1 || !strings.Contains(lines[0], "cannot start") {
		t.Errorf("stderr = %q, want only the line of the refused start", stderr.String())
	}
}
