package sandbox_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

func TestScrub(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		values sandbox.Values
		text   string
		want   string
	}{
		{"an exposed secret of 8 bytes", sandbox.Values{Exposed: []string{"abcdefgh"}}, "x abcdefgh y abcdefgh", "x [redacted] y [redacted]"},
		{"an exposed secret of 7 bytes", sandbox.Values{Exposed: []string{"abcdefg"}}, "x abcdefg y", "x abcdefg y"},
		{"a credential's secret of any length", sandbox.Values{Credentials: []string{"k1"}}, "key k1!", "key [redacted]!"},
		{"the hub credential", sandbox.Values{HubCredential: "pat_abc123"}, "Bearer pat_abc123", "Bearer [redacted]"},
		{"a value inside a longer one", sandbox.Values{Credentials: []string{"token"}, Exposed: []string{"token-longer-1"}},
			"a token-longer-1 b token", "a [redacted] b [redacted]"},
		{"no value", sandbox.Values{}, "plain", "plain"},
		{"no value in 1024 bytes", sandbox.Values{}, strings.Repeat("p", 1024), strings.Repeat("p", 1024)},
		{"2000 bytes of a two-byte character", sandbox.Values{}, strings.Repeat("é", 1000), strings.Repeat("é", 512)},
		{"a cut inside a character", sandbox.Values{}, "a" + strings.Repeat("é", 1000), "a" + strings.Repeat("é", 511)},
		{"a value across the cut", sandbox.Values{Exposed: []string{"SECRET1234"}},
			strings.Repeat("x", 1020) + "SECRET1234 tail", strings.Repeat("x", 1020) + "[red"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sandbox.Scrub(tc.values, tc.text)
			if got != tc.want {
				t.Errorf("Scrub = %q; want %q", got, tc.want)
			}
			if len(got) > 1024 || !utf8.ValidString(got) {
				t.Errorf("Scrub returned %d bytes, valid UTF-8 %t; want at most 1024 bytes of valid UTF-8", len(got), utf8.ValidString(got))
			}
		})
	}
}

// The runtime fails every command of the sandbox with the text of a row; the
// checkpoint's error, its event's, the line the hub logs and the message of
// the checkpoint route's answer carry the text scrubbed of the sandbox's
// credential secret and its hub credential.
func TestScrub_TheRuntimeErrorOfACheckpoint(t *testing.T) {
	t.Parallel()
	const secretValue, hubValue = "sk-secret-value-1234", "hub-credential-5678"
	logs := &hubtest.LogBuffer{}
	h := startCheckpointHub(t, func(d *sandbox.Deps) { d.Logger = slog.New(slog.NewJSONHandler(logs, nil)) })
	id, dir := h.workSandbox(t, nil)
	h.cloneWork(t, dir)
	h.svc.SetValues(id, sandbox.Values{Credentials: []string{secretValue}, HubCredential: hubValue})
	for _, tc := range []struct {
		name, text string
		// redacted is true when a value stands within the text's first
		// 1,024 bytes.
		redacted bool
	}{
		{"a credential's secret", "auth failed for " + secretValue, true},
		{"the hub credential", "bearer " + hubValue + " refused", true},
		{"both", secretValue + " and " + hubValue, true},
		{"5,000 bytes ending in the secret", strings.Repeat("x", 5000-len(secretValue)) + secretValue, false},
		{"no secret", "the gateway is restarting", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.rt.FailOperation(runtime.OpExec, runtime.ErrUnavailable, tc.text)
			t.Cleanup(func() { h.rt.FailOperation(runtime.OpExec, nil, "") })
			logged := len(logs.String())
			cp, err := h.svc.Checkpoint(context.Background(), storedRecord(t, h.Hub, id), sandbox.SystemActor)
			if err == nil || cp == nil || cp.Error == nil {
				t.Fatalf("Checkpoint = %+v, %v; want a failure", cp, err)
			}
			evs := h.sandboxEvents(t, sandbox.EventCheckpoint, id)
			evText, _ := evs[len(evs)-1].metadata["error"].(string)
			var line struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(logs.String()[logged:])), &line); err != nil {
				t.Fatalf("the hub logged %q: %v", logs.String()[logged:], err)
			}
			// The route asks for one more checkpoint, which fails the same.
			a := h.call(t, http.MethodPost, "/workspaces/demo--t1/sandboxes/"+id+"/checkpoint", h.key, nil)
			if a.status != http.StatusBadGateway {
				t.Errorf("POST checkpoint = %d %s; want 502", a.status, a.body)
			}
			for _, sink := range []struct{ name, text string }{
				{"checkpoint.error", *cp.Error}, {"the event's error", evText}, {"the logged error", line.Error}, {"the returned error", err.Error()},
				{"the route's answer", a.err.Message},
			} {
				if strings.Contains(sink.text, secretValue) || strings.Contains(sink.text, hubValue) || len(sink.text) > 1024 {
					t.Errorf("%s = %q (%d bytes); want neither value and at most 1,024 bytes", sink.name, sink.text, len(sink.text))
				}
				if tc.redacted && !strings.Contains(sink.text, "[redacted]") {
					t.Errorf("%s = %q; want [redacted] where a value stood", sink.name, sink.text)
				}
				if !tc.redacted && len(tc.text) < 1024 && !strings.Contains(sink.text, tc.text) {
					t.Errorf("%s = %q; want the text %q unchanged", sink.name, sink.text, tc.text)
				}
			}
		})
	}
}
