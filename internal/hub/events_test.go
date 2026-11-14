package hub_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

func TestStartHook_MayEmitBeforeListening(t *testing.T) {
	t.Parallel()
	api := "api"
	userID := "u_1"
	in := event.Input{Type: "parley.git.fetch", Workspace: &api, ActorType: "pat", ActorID: &userID,
		ResourceType: "workspace", ResourceID: "api"}
	var events *event.Component
	var emitted event.Record
	hubtest.Start(t, func(c *hub.Composition) {
		events = c.Events()
		c.AddStartHook("emit-at-start", func(ctx context.Context, _ *hub.Runtime) error {
			var err error
			emitted, err = c.Events().EmitNow(ctx, in)
			return err
		})
	})

	recs, _, err := events.ReadAfter(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	if len(recs) != 1 || recs[0].Seq != 1 || recs[0].ID != emitted.ID || recs[0].Type != "parley.git.fetch" {
		t.Errorf("ReadAfter(0) = %+v, want the event the start hook emitted, with seq 1", recs)
	}
}

func TestEvents_NotifiesCommitsWhileTheHubServes(t *testing.T) {
	t.Parallel()
	notified := make(chan event.Record, 1)
	hubtest.Start(t, func(c *hub.Composition) {
		c.Events().OnCommit(func(rec event.Record) { notified <- rec })
		c.AddStartHook("emit-at-start", func(ctx context.Context, _ *hub.Runtime) error {
			_, err := c.Events().EmitNow(ctx, event.Input{Type: "parley.workspace.create", ActorType: "system",
				ResourceType: "workspace", ResourceID: "api", Metadata: map[string]any{"scope": "user", "scope_id": "u_1"}})
			return err
		})
	})

	select {
	case rec := <-notified:
		if rec.Seq != 1 || rec.Type != "parley.workspace.create" {
			t.Errorf("notified %+v, want the event of the start hook with seq 1", rec)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no commit notified within 5 s of the start")
	}
}

func TestEvents_DecidesVisibilityWithTheHubsAccessRule(t *testing.T) {
	t.Parallel()
	var events *event.Component
	h := hubtest.Start(t, func(c *hub.Composition) { events = c.Events() })
	dev, mem, out := h.CreateUser(t, "dev"), h.CreateUser(t, "mem"), h.CreateUser(t, "out")
	acme := h.CreateOrg(t, "acme")
	h.AddMember(t, acme, mem)
	h.AddWorkspace(t, access.Record{Slug: "api", OwnerID: dev, OrgID: acme, Kind: access.KindRepository, Status: access.StatusReady})

	ctx := context.Background()
	api := "api"
	onAPI, err := events.EmitNow(ctx, event.Input{Type: "parley.change.open", Workspace: &api, ActorType: "api_key",
		ActorID: &dev, ResourceType: "change", ResourceID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	ofAcme, err := events.EmitNow(ctx, event.Input{Type: "parley.variable.update", ActorType: "admin_token",
		ResourceType: "variable", ResourceID: "region", Metadata: map[string]any{"scope": "org", "scope_id": acme}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		rec  event.Record
		user string
		want bool
	}{
		{"owner, workspace event", onAPI, dev, true},
		{"org member, workspace event", onAPI, mem, true},
		{"no relation, workspace event", onAPI, out, false},
		{"org member, org event", ofAcme, mem, true},
		{"no relation, org event", ofAcme, out, false},
	} {
		got, err := events.Visible(ctx, event.Caller{UserID: tc.user}, tc.rec)
		if err != nil || got != tc.want {
			t.Errorf("%s: Visible = %v, %v, want %v, nil", tc.name, got, err, tc.want)
		}
	}
}

func TestEvents_StopReleasesHeldLogRequests(t *testing.T) {
	t.Parallel()
	h := hubtest.Start(t, nil)

	wrote := make(chan struct{})
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { close(wrote) },
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL("/api/v1/events/log?wait=60"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	type reply struct {
		status int
		body   []byte
		err    error
	}
	answered := make(chan reply, 1)
	go func() {
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- reply{err: err}
			return
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		answered <- reply{res.StatusCode, body, err}
	}()
	select {
	case <-wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("the request was not sent within 5 s")
	}
	// The hub has nothing to answer on an empty log, so the request is held
	// once it reaches the handler.
	time.Sleep(200 * time.Millisecond)
	select {
	case r := <-answered:
		t.Fatalf("answered %d %s, %v before the stop, want the request held", r.status, r.body, r.err)
	default:
	}

	t0 := time.Now()
	h.Stop()
	select {
	case r := <-answered:
		var page struct {
			Items []json.RawMessage `json:"items"`
		}
		if r.err != nil || r.status != http.StatusOK || json.Unmarshal(r.body, &page) != nil || page.Items == nil || len(page.Items) != 0 {
			t.Errorf("answer = %d %s, %v; want 200 with items == []", r.status, r.body, r.err)
		}
		if elapsed := time.Since(t0); elapsed >= time.Second {
			t.Errorf("answered %v after the stop began, want under 1 s", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the held request was not answered within 5 s of the stop")
	}
}

func TestHub_AppliesStreamLimit(t *testing.T) {
	t.Parallel()
	var events *event.Component
	h := hubtest.StartWith(t, hubtest.Config{TOML: "[events]\nmax_sse_connections = 1\nretention = \"2d\"\n",
		Compose: func(c *hub.Composition) { events = c.Events() }})
	if got := events.Retention(); got != 48*time.Hour {
		t.Errorf("Retention() = %v, want the configured 48h", got)
	}

	open := func() *http.Response {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL("/api/v1/events"), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+h.Token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/v1/events: %v", err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}
	if res := open(); res.StatusCode != http.StatusOK {
		t.Fatalf("first stream: status %d, want 200", res.StatusCode)
	}
	res := open()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second stream: status %d, want 503", res.StatusCode)
	}
	if got := res.Header.Get("Retry-After"); got != "5" {
		t.Errorf("second stream: Retry-After = %q, want 5", got)
	}
}

func TestHub_RefusesInvalidEventConfig(t *testing.T) {
	t.Parallel()
	dir := workDir(t, localConfig+"[events]\nretention = \"10m\"\n")
	var stderr syncBuffer
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	code := hub.Run(ctx, hub.Options{WorkDir: dir, Stderr: &stderr})

	if code != 1 {
		t.Errorf("Run returned %d, want 1", code)
	}
	lines := nonEmptyLines(stderr.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "events.retention") {
		t.Errorf("stderr = %q, want one line naming events.retention", stderr.String())
	}
	if strings.Contains(stderr.String(), "listening") {
		t.Errorf("stderr %q has a listening line", stderr.String())
	}
}
