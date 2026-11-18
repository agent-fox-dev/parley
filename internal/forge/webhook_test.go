package forge_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// webhookPath is the webhook route of a workspace.
const webhookPath = "/forge/webhook"

// generatedSecret is the form of a secret the hub generates.
var generatedSecret = regexp.MustCompile(`^[0-9a-f]{64}$`)

// webhookAnswer is the answer of a PUT of the webhook, with its members
// as they were sent.
type webhookAnswer struct {
	Configured bool    `json:"configured"`
	URL        *string `json:"url"`
	Secret     *string `json:"secret"`

	members map[string]json.RawMessage
}

// putWebhook sends PUT on the webhook of slug with credential and body,
// and returns the answer, decoded when it is 200.
func (w *world) putWebhook(t *testing.T, slug, credential string, body any) (int, []byte, webhookAnswer) {
	t.Helper()
	status, raw, _ := w.send(t, http.MethodPut, slug, webhookPath, credential, body)
	var a webhookAnswer
	if status == http.StatusOK {
		if err := json.Unmarshal(raw, &a); err != nil || json.Unmarshal(raw, &a.members) != nil {
			t.Fatalf("PUT %s%s = 200 %s, which is no webhook answer", slug, webhookPath, raw)
		}
	}
	return status, raw, a
}

// webhookSecret is the stored webhook secret of slug, "" for none.
func (w *world) webhookSecret(t *testing.T, slug string) string {
	t.Helper()
	value, _, err := w.secrets.Get(context.Background(), secret.ScopeWorkspace, slug, forge.SecretWebhook)
	if err != nil {
		t.Fatalf("read the webhook secret of %s: %v", slug, err)
	}
	return value
}

// secretCount is how many workspace secrets slug holds.
func (w *world) secretCount(t *testing.T, slug string) int {
	t.Helper()
	n, err := w.secrets.Count(context.Background(), secret.ScopeWorkspace, slug)
	if err != nil {
		t.Fatalf("count the secrets of %s: %v", slug, err)
	}
	return n
}

// fillSecrets stores n workspace secrets of slug that are not the webhook
// secret.
func (w *world) fillSecrets(t *testing.T, slug string, n int) {
	t.Helper()
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		for i := range n {
			if err := w.secrets.Put(ctx, tx, secret.ScopeWorkspace, slug, fmt.Sprintf("OTHER_%d", i), "value"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("store %d secrets of %s: %v", n, slug, err)
	}
}

// logged are the events of the hub's log whose type begins with prefix,
// of slug only unless slug is "".
func (w *world) logged(t *testing.T, prefix, slug string) []event.Record {
	t.Helper()
	records, _, err := w.events.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("read the event log: %v", err)
	}
	var out []event.Record
	for _, r := range records {
		if strings.HasPrefix(r.Type, prefix) && (slug == "" || r.Workspace != nil && *r.Workspace == slug) {
			out = append(out, r)
		}
	}
	return out
}

// wantWebhookSet fails the test unless the log holds exactly one webhook
// set event of slug, by the owner's API key, whose metadata says whether
// the secret was generated.
func (w *world) wantWebhookSet(t *testing.T, slug string, generated bool) {
	t.Helper()
	got := w.logged(t, "parley.forge.webhook.", slug)
	if len(got) != 1 {
		t.Fatalf("the log holds %d webhook events of %s, want 1: %+v", len(got), slug, got)
	}
	w.wantWebhookEvent(t, got[0], slug, "set", fmt.Sprintf(`{"generated":%t}`, generated))
}

// wantWebhookEvent fails the test unless e is the webhook event action of
// slug, by the owner's API key, with the metadata given.
func (w *world) wantWebhookEvent(t *testing.T, e event.Record, slug, action, metadata string) {
	t.Helper()
	if e.Type != "parley.forge.webhook."+action || e.ResourceType != "webhook" || e.ResourceID != slug || e.Action != action ||
		e.ActorType != event.ActorAPIKey || e.ActorID == nil || *e.ActorID != w.owner || !sameJSON(e.Metadata, metadata) {
		t.Errorf("the webhook event of %s = %+v (metadata %s), want parley.forge.webhook.%s of webhook %s, action %s, by the owner's key, metadata %s",
			slug, e, e.Metadata, action, slug, action, metadata)
	}
}

func TestWebhookSet_GeneratesAndStoresASecret(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	cases := []struct {
		name string
		body any
	}{
		{"no body", nil},
		{"an empty object", `{}`},
		{"a null secret", `{"secret":null}`},
		{"a null secret and an unknown member", `{"secret":null,"x":1}`},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slug := fmt.Sprintf("gen%d", i)
			w.addWorkspace(t, workspace.Workspace{Slug: slug})
			status, body, a := w.putWebhook(t, slug, w.ownerKey, c.body)
			if status != http.StatusOK || !a.Configured || a.Secret == nil || !generatedSecret.MatchString(*a.Secret) ||
				len(a.members) != 3 || string(a.members["url"]) != "null" {
				t.Fatalf("PUT %s = %d %s, want 200 configured, url null and a secret of 64 hex characters", slug, status, body)
			}
			if got := w.webhookSecret(t, slug); got != *a.Secret {
				t.Errorf("the stored secret = %q, want the one answered %q", got, *a.Secret)
			}
			w.wantWebhookSet(t, slug, true)
		})
	}

	t.Run("a second PUT replaces the first secret", func(t *testing.T) {
		w.addWorkspace(t, workspace.Workspace{Slug: "rotate"})
		_, _, first := w.putWebhook(t, "rotate", w.ownerKey, nil)
		status, body, second := w.putWebhook(t, "rotate", w.ownerKey, nil)
		if status != http.StatusOK || first.Secret == nil || second.Secret == nil || *first.Secret == *second.Secret {
			t.Fatalf("the second PUT = %d %s, want 200 with a secret other than the first", status, body)
		}
		if got := w.webhookSecret(t, "rotate"); got != *second.Secret {
			t.Errorf("the stored secret = %q, want the second %q", got, *second.Secret)
		}
		if n := w.secretCount(t, "rotate"); n != 1 {
			t.Errorf("the workspace holds %d secrets, want 1", n)
		}
	})

	if n := len(w.logged(t, "parley.secret.", "")); n != 0 {
		t.Errorf("the log holds %d secret events, want none", n)
	}
	for _, e := range w.logged(t, "", "") {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		for _, slug := range []string{"gen0", "gen1", "gen2", "gen3", "rotate"} {
			if s := w.webhookSecret(t, slug); strings.Contains(string(line), s) {
				t.Errorf("the event %s holds the secret of %s", line, slug)
			}
		}
	}
}

func TestWebhookSet_StoresAGivenSecret(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	cases := []struct{ name, secret string }{
		{"16 characters", "0123456789abcdef"},
		{"255 characters", strings.Repeat("Ab3~", 63) + "xyz"},
		{"the ends of the range", "!~!~!~!~!~!~!~!~"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slug := fmt.Sprintf("given%d", i)
			w.addWorkspace(t, workspace.Workspace{Slug: slug})
			status, body, a := w.putWebhook(t, slug, w.ownerKey, map[string]any{"secret": c.secret})
			if _, has := a.members["secret"]; status != http.StatusOK || !a.Configured || has {
				t.Fatalf("PUT %s = %d %s, want 200 configured without a secret", slug, status, body)
			}
			if got := w.webhookSecret(t, slug); got != c.secret {
				t.Errorf("the stored secret = %q, want %q", got, c.secret)
			}
			w.wantWebhookSet(t, slug, false)
		})
	}

	t.Run("a secret stored under another casing of its key", func(t *testing.T) {
		w.addWorkspace(t, workspace.Workspace{Slug: "cased"})
		w.putSecret(t, "cased", "forge_webhook_secret", "the-old-value-0123")
		const value = "the-new-value-0123"
		if status, body, _ := w.putWebhook(t, "cased", w.ownerKey, map[string]any{"secret": value}); status != http.StatusOK {
			t.Fatalf("PUT cased = %d %s, want 200", status, body)
		}
		items, err := w.secrets.ListPage(context.Background(), secret.ScopeWorkspace, "cased", "", 200)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || !strings.EqualFold(items[0].Key, forge.SecretWebhook) {
			t.Errorf("the workspace's secrets = %+v, want one, the webhook secret", items)
		}
		if got := w.webhookSecret(t, "cased"); got != value {
			t.Errorf("the stored secret = %q, want %q", got, value)
		}
	})
}

func TestWebhookSet_RefusesABodyOutsideTheRule(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo"})
	const before = "the-value-before-0123"
	w.putSecret(t, "demo", forge.SecretWebhook, before)
	events := len(w.logged(t, "", ""))

	cases := []struct {
		name string
		body string
		// secret is the value submitted, which the answer must not hold;
		// "" for none.
		secret string
	}{
		{"a secret of 15 characters", `{"secret":"Fifteen-Chars-1"}`, "Fifteen-Chars-1"},
		{"a secret of 256 characters", `{"secret":"` + strings.Repeat("Q", 256) + `"}`, strings.Repeat("Q", 256)},
		{"a secret with a space", `{"secret":"Has-A Space-0123456"}`, "Has-A Space-0123456"},
		{"a secret with a tab", `{"secret":"Has-A\tTab-0123456"}`, "Has-A\tTab-0123456"},
		{"a secret with a character past the range", `{"secret":"Has-An-é-0123456"}`, "Has-An-é-0123456"},
		{"an empty secret", `{"secret":""}`, ""},
		{"a number", `{"secret":42}`, ""},
		{"a boolean", `{"secret":true}`, ""},
		{"an array", `{"secret":[]}`, ""},
		{"a body that is an array", `[]`, ""},
		{"a body that is a string", `"x"`, ""},
		{"a body that is cut short", `{`, ""},
		{"a body with a second value", `{}{}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body, _ := w.putWebhook(t, "demo", w.ownerKey, c.body)
			if e := errorOf(t, body); status != http.StatusBadRequest || e.Type != "invalid_request" {
				t.Errorf("PUT demo with %s = %d %s, want 400 invalid_request", c.body, status, body)
			}
			if c.secret != "" && (strings.Contains(errorOf(t, body).Message, c.secret) || strings.Contains(string(body), c.secret)) {
				t.Errorf("the answer %s holds the submitted value", body)
			}
			if got := w.webhookSecret(t, "demo"); got != before {
				t.Errorf("the stored secret = %q, want %q", got, before)
			}
			if n := len(w.logged(t, "", "")); n != events {
				t.Errorf("the log holds %d events, want %d", n, events)
			}
		})
	}
}

func TestWebhookSet_ReportsTheWebhookURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		external string
		want     *string
	}{
		{"https://hub.example.com", new("https://hub.example.com/hooks/forge/demo")},
		{"https://hub.example.com/", new("https://hub.example.com/hooks/forge/demo")},
		{"https://h.example/parley/", new("https://h.example/parley/hooks/forge/demo")},
		{"", nil},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("external_url %q", c.external), func(t *testing.T) {
			t.Parallel()
			w := newWorldWith(t, hubtest.Config{Server: externalURL(c.external)}, nil)
			w.addWorkspace(t, workspace.Workspace{Slug: "demo"})
			status, body, a := w.putWebhook(t, "demo", w.ownerKey, nil)
			if _, has := a.members["url"]; status != http.StatusOK || !has {
				t.Fatalf("PUT demo = %d %s, want 200 with a url", status, body)
			}
			if (a.URL == nil) != (c.want == nil) || a.URL != nil && *a.URL != *c.want {
				t.Errorf("the url = %s, want %v", a.members["url"], jsonOf(c.want))
			}
			if a.Secret == nil || w.webhookSecret(t, "demo") != *a.Secret {
				t.Errorf("the generated secret %v is not the stored one", a.Secret)
			}
		})
	}
}

// jsonOf is v as JSON text.
func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestWebhookSet_RefusesByScopeAccessForkBodyAndArchivedInOrder(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo"})
	w.addWorkspace(t, workspace.Workspace{Slug: "archived", Status: workspace.StatusArchived})
	w.addWorkspace(t, workspace.Workspace{Slug: "demo--t1", ParentSlug: new("demo")})
	readPAT := w.pat(t, w.owner, "", forge.ScopeRead)
	valid, short := map[string]any{"secret": "0123456789abcdef"}, map[string]any{"secret": "short"}

	cases := []struct {
		name, credential, slug string
		body                   any
		status                 int
		errorType              string
	}{
		{"a PAT with forge:read only", readPAT, "demo", valid, http.StatusForbidden, "insufficient_scope"},
		{"a PAT with forge:read only on an unknown slug", readPAT, "nosuch", valid, http.StatusForbidden, "insufficient_scope"},
		{"a user without access", w.outKey, "demo", valid, http.StatusNotFound, "not_found"},
		{"a member who can only read", w.memberKey, "demo", valid, http.StatusForbidden, "forbidden"},
		{"a member who can only read an archived workspace", w.memberKey, "archived", valid, http.StatusForbidden, "forbidden"},
		{"a task fork", w.ownerKey, "demo--t1", valid, http.StatusBadRequest, "fork_has_no_origin"},
		{"a task fork before an invalid body", w.ownerKey, "demo--t1", short, http.StatusBadRequest, "fork_has_no_origin"},
		{"an archived workspace", w.ownerKey, "archived", valid, http.StatusConflict, "workspace_archived"},
		{"an invalid body before an archived workspace", w.ownerKey, "archived", short, http.StatusBadRequest, "invalid_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body, _ := w.putWebhook(t, c.slug, c.credential, c.body)
			if e := errorOf(t, body); status != c.status || e.Type != c.errorType {
				t.Errorf("PUT %s = %d %s, want %d %s", c.slug, status, body, c.status, c.errorType)
			}
			for _, slug := range []string{"demo", "archived", "demo--t1"} {
				if n := w.secretCount(t, slug); n != 0 {
					t.Errorf("%s holds %d secrets, want none", slug, n)
				}
			}
			if n := len(w.logged(t, "parley.forge.webhook.", "")); n != 0 {
				t.Errorf("the log holds %d webhook events, want none", n)
			}
		})
	}
}

func TestWebhookSet_RefusesWithoutAKeyOrRoomForTheSecret(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo"})
	w.addWorkspace(t, workspace.Workspace{Slug: "full"})
	w.addWorkspace(t, workspace.Workspace{Slug: "room"})
	w.fillSecrets(t, "full", secret.MaxEntries)
	w.fillSecrets(t, "room", secret.MaxEntries-1)
	w.putSecret(t, "room", forge.SecretWebhook, "the-value-before-0123")
	valid := map[string]any{"secret": "0123456789abcdef"}

	status, body, _ := w.putWebhook(t, "full", w.ownerKey, valid)
	if e := errorOf(t, body); status != http.StatusTooManyRequests || e.Type != "secret_limit_reached" ||
		e.Message != "maximum of 100 entries per scope exceeded" {
		t.Errorf("PUT full = %d %s, want 429 secret_limit_reached", status, body)
	}
	if n := w.secretCount(t, "full"); n != secret.MaxEntries {
		t.Errorf("full holds %d secrets, want %d", n, secret.MaxEntries)
	}
	if status, body, _ := w.putWebhook(t, "room", w.ownerKey, valid); status != http.StatusOK {
		t.Errorf("PUT room = %d %s, want 200: the webhook secret is replaced, not added", status, body)
	}
	if n := w.secretCount(t, "room"); n != secret.MaxEntries {
		t.Errorf("room holds %d secrets, want %d", n, secret.MaxEntries)
	}

	w.RestartEnv(t, map[string]string{secret.EnvCurrentKey: ""})
	for _, slug := range []string{"demo", "full"} {
		before := w.secretCount(t, slug)
		status, body, _ := w.putWebhook(t, slug, w.ownerKey, valid)
		if e := errorOf(t, body); status != http.StatusServiceUnavailable || e.Type != "secrets_unavailable" {
			t.Errorf("PUT %s without a key = %d %s, want 503 secrets_unavailable", slug, status, body)
		}
		if n := w.secretCount(t, slug); n != before {
			t.Errorf("%s holds %d secrets, want %d", slug, n, before)
		}
	}
	for _, slug := range []string{"demo", "full"} {
		if n := len(w.logged(t, "parley.forge.webhook.", slug)); n != 0 {
			t.Errorf("the log holds %d webhook events of %s, want none", n, slug)
		}
	}
}

func TestWebhookUnset_RemovesTheSecretInAnyStateAndRecordsWhetherOneExisted(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	cases := []struct {
		name     string
		archived bool
		// put sets the secret over the API first; key, when it is not "",
		// stores it under that key.
		put     bool
		key     string
		deletes int
		existed bool
	}{
		{"a secret set over the API", false, true, "", 1, true},
		{"a secret stored under another casing of its key", false, false, "forge_webhook_secret", 1, true},
		{"an archived workspace", true, false, forge.SecretWebhook, 1, true},
		{"no secret, twice", false, false, "", 2, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slug := fmt.Sprintf("unset%d", i)
			ws := workspace.Workspace{Slug: slug}
			if c.archived {
				ws.Status = workspace.StatusArchived
			}
			w.addWorkspace(t, ws)
			w.fillSecrets(t, slug, 1)
			if c.put {
				if status, body, _ := w.putWebhook(t, slug, w.ownerKey, nil); status != http.StatusOK {
					t.Fatalf("PUT %s = %d %s, want 200", slug, status, body)
				}
			}
			if c.key != "" {
				w.putSecret(t, slug, c.key, "the-value-before-0123")
			}
			for range c.deletes {
				if status, body, _ := w.send(t, http.MethodDelete, slug, webhookPath, w.ownerKey, nil); status != http.StatusNoContent || len(body) != 0 {
					t.Errorf("DELETE %s = %d %q, want 204 with no body", slug, status, body)
				}
			}
			if got, n := w.webhookSecret(t, slug), w.secretCount(t, slug); got != "" || n != 1 {
				t.Errorf("%s holds the webhook secret %q and %d secrets, want no webhook secret and its other secret", slug, got, n)
			}
			got := w.logged(t, "parley.forge.webhook.unset", slug)
			if len(got) != c.deletes {
				t.Fatalf("the log holds %d unset events of %s, want %d", len(got), slug, c.deletes)
			}
			for _, e := range got {
				w.wantWebhookEvent(t, e, slug, "unset", fmt.Sprintf(`{"existed":%t}`, c.existed))
			}
		})
	}
	if n := len(w.logged(t, "parley.secret.", "")); n != 0 {
		t.Errorf("the log holds %d secret events, want none", n)
	}
}

func TestWebhookUnset_RefusesByScopeAccessAndFork(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo"})
	w.addWorkspace(t, workspace.Workspace{Slug: "demo--t1", ParentSlug: new("demo")})
	const before = "the-value-before-0123"
	for _, slug := range []string{"demo", "demo--t1"} {
		w.putSecret(t, slug, forge.SecretWebhook, before)
	}
	readPAT := w.pat(t, w.owner, "", forge.ScopeRead)

	cases := []struct {
		name, credential, slug string
		status                 int
		errorType              string
	}{
		{"a PAT with forge:read only", readPAT, "demo", http.StatusForbidden, "insufficient_scope"},
		{"a user without access", w.outKey, "demo", http.StatusNotFound, "not_found"},
		{"a member who can only read", w.memberKey, "demo", http.StatusForbidden, "forbidden"},
		{"a task fork", w.ownerKey, "demo--t1", http.StatusBadRequest, "fork_has_no_origin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body, _ := w.send(t, http.MethodDelete, c.slug, webhookPath, c.credential, nil)
			if e := errorOf(t, body); status != c.status || e.Type != c.errorType {
				t.Errorf("DELETE %s = %d %s, want %d %s", c.slug, status, body, c.status, c.errorType)
			}
			for _, slug := range []string{"demo", "demo--t1"} {
				if got := w.webhookSecret(t, slug); got != before {
					t.Errorf("the webhook secret of %s = %q, want %q", slug, got, before)
				}
			}
			if n := len(w.logged(t, "parley.forge.webhook.", "")); n != 0 {
				t.Errorf("the log holds %d webhook events, want none", n)
			}
		})
	}
}

func TestWebhook_SendsNoRequestToTheForge(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	requests := []struct {
		method string
		body   any
		status int
	}{
		{http.MethodPut, nil, http.StatusOK},
		{http.MethodPut, map[string]any{"secret": "0123456789abcdef"}, http.StatusOK},
		{http.MethodDelete, nil, http.StatusNoContent},
	}
	for _, r := range requests {
		if status, body, sent := w.send(t, r.method, "demo", webhookPath, w.ownerKey, r.body); status != r.status || sent != 0 {
			t.Errorf("%s demo = %d %s with %d requests to the forge, want %d and none", r.method, status, body, sent, r.status)
		}
	}
}

// errLogBroken is the failure of every event of brokenLog.
var errLogBroken = errors.New("the event log cannot be written")

// brokenLog is an event log that records no event.
type brokenLog struct{}

func (brokenLog) Emit(context.Context, *sql.Tx, event.Input) (event.Record, error) {
	return event.Record{}, errLogBroken
}

func TestWebhook_KeepsTheSecretAsItWasWhenItsEventCannotBeRecorded(t *testing.T) {
	t.Parallel()
	w := newWorldWith(t, hubtest.Config{}, func(_ *world, d *forge.Deps) { d.Log = brokenLog{} })
	const held, given = "the-secret-held-0123", "the-secret-given-0123"
	cases := []struct {
		name, method string
		// held is the webhook secret stored before the request, "" for none.
		held string
		body any
	}{
		{"a PUT on a workspace without a secret", http.MethodPut, "", map[string]any{"secret": given}},
		{"a PUT on a workspace holding a secret", http.MethodPut, held, map[string]any{"secret": given}},
		{"a DELETE on a workspace holding a secret", http.MethodDelete, held, nil},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slug := fmt.Sprintf("nolog%d", i)
			w.addWorkspace(t, workspace.Workspace{Slug: slug})
			if c.held != "" {
				w.putSecret(t, slug, forge.SecretWebhook, c.held)
			}
			status, body, _ := w.send(t, c.method, slug, webhookPath, w.ownerKey, c.body)
			if e := errorOf(t, body); status != http.StatusInternalServerError || e.Type != "internal" {
				t.Errorf("%s %s = %d %s, want 500 internal", c.method, slug, status, body)
			}
			for _, leak := range []string{errLogBroken.Error(), held, given} {
				if strings.Contains(string(body), leak) {
					t.Errorf("the answer %s holds %q", body, leak)
				}
			}
			if got := w.webhookSecret(t, slug); got != c.held {
				t.Errorf("the webhook secret of %s = %q, want %q", slug, got, c.held)
			}
			if n := len(w.logged(t, "parley.forge.webhook.", slug)); n != 0 {
				t.Errorf("the log holds %d webhook events of %s, want none", n, slug)
			}
		})
	}
}
