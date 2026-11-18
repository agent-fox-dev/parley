package forge_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// hookSecret is the webhook secret of demo in the ingress tests.
const hookSecret = "the-webhook-secret-0123"

// maxDelivery is the largest body the ingress takes, in bytes.
const maxDelivery = 1 << 20

// ingressClient sends the deliveries. It keeps no connection open, so that
// a hub restarted on the same port never gets a request over a connection
// of the stopped run.
var ingressClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true, ExpectContinueTimeout: time.Minute}}

// newIngressWorld is a delivery world whose workspace demo holds the
// webhook secret hookSecret.
func newIngressWorld(t *testing.T) *world {
	t.Helper()
	w := newDeliveryWorld(t, deliveryTime, nil)
	w.putSecret(t, "demo", forge.SecretWebhook, hookSecret)
	return w
}

// deliver posts body to the ingress of slug with the headers of header and
// nothing else, no Authorization in particular, and returns the answer. A
// body of a type net/http does not know the length of is sent chunked.
func (w *world) deliver(t *testing.T, slug string, header http.Header, body io.Reader) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, w.URL("/hooks/forge/"+slug), body)
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range header {
		req.Header[name] = values
	}
	res, err := ingressClient.Do(req)
	if err != nil {
		t.Fatalf("POST /hooks/forge/%s: %v", slug, err)
	}
	defer res.Body.Close()
	answer, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("POST /hooks/forge/%s: read the answer: %v", slug, err)
	}
	return res.StatusCode, answer
}

// signature is the X-Hub-Signature-256 of body under secretValue.
func signature(secretValue string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secretValue))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ghHeaders are the headers of a GitHub delivery id of the event, signed
// under secretValue.
func ghHeaders(id, eventName, secretValue string, body []byte) http.Header {
	return http.Header{
		"X-Github-Delivery":   {id},
		"X-Github-Event":      {eventName},
		"X-Hub-Signature-256": {signature(secretValue, body)},
	}
}

// glHeaders are the headers of a GitLab delivery with the event UUID id,
// of the event, with token.
func glHeaders(id, eventName, token string) http.Header {
	return http.Header{
		"X-Gitlab-Event-Uuid": {id},
		"X-Gitlab-Event":      {eventName},
		"X-Gitlab-Token":      {token},
	}
}

// glIssueOpen is a GitLab Issue Hook open payload of issue 12 of the
// repository.
func glIssueOpen(repo string) []byte {
	return glPayload("open", repo, map[string]any{"object_attributes": map[string]any{"iid": 12}})
}

// withHeader is h with the header name set to values, or left out without
// values.
func withHeader(h http.Header, name string, values ...string) http.Header {
	out := h.Clone()
	if len(values) == 0 {
		out.Del(name)
		return out
	}
	out[http.CanonicalHeaderKey(name)] = values
	return out
}

// deliveryCount is how many deliveries the hub has recorded, in every
// workspace.
func (w *world) deliveryCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.Store.Main().QueryRow(`SELECT COUNT(*) FROM forge_deliveries`).Scan(&n); err != nil {
		t.Fatalf("count the deliveries: %v", err)
	}
	return n
}

// wantOutcome fails the test unless the answer is 202 with exactly the
// outcome given.
func wantOutcome(t *testing.T, what string, status int, body []byte, outcome forge.Outcome) {
	t.Helper()
	if status != http.StatusAccepted || !sameJSON(body, `{"outcome":"`+string(outcome)+`"}`) {
		t.Errorf("%s = %d %s, want 202 {\"outcome\":%q}", what, status, body, outcome)
	}
}

// wantRefusal fails the test unless the answer is the error envelope of
// status and errorType.
func wantRefusal(t *testing.T, what string, status int, body []byte, wantStatus int, errorType string) {
	t.Helper()
	var e struct {
		Error hubError `json:"error"`
	}
	if status != wantStatus || json.Unmarshal(body, &e) != nil || e.Error.Code != wantStatus || e.Error.Type != errorType {
		t.Errorf("%s = %d %s, want %d %s", what, status, body, wantStatus, errorType)
	}
}

// readLog records the reads of the ingress's workspace and secret
// readers, in order.
type readLog struct {
	mu    sync.Mutex
	reads []string
}

func (l *readLog) add(read string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads = append(l.reads, read)
}

// take returns the reads recorded since the last take.
func (l *readLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	reads := l.reads
	l.reads = nil
	return reads
}

// loggedSecrets is a secret reader that records every read in log.
type loggedSecrets struct {
	workspace.SecretReader
	log *readLog
}

func (s loggedSecrets) Get(ctx context.Context, scope secret.Scope, scopeID, name string) (string, bool, error) {
	s.log.add(fmt.Sprintf("secret %s/%s/%s", scope, scopeID, name))
	return s.SecretReader.Get(ctx, scope, scopeID, name)
}

func TestIngress_AnswersTheSameNotFoundForAWorkspaceWithoutAWebhook(t *testing.T) {
	t.Parallel()
	reads := &readLog{}
	w := newDeliveryWorld(t, deliveryTime, func(_ *world, d *forge.Deps) {
		workspaces := d.Workspaces
		d.Workspaces = func(ctx context.Context, slug string) (workspace.Workspace, bool, error) {
			reads.add("workspace " + slug)
			return workspaces(ctx, slug)
		}
		d.Secrets = loggedSecrets{SecretReader: d.Secrets, log: reads}
	})
	w.putSecret(t, "demo", forge.SecretWebhook, hookSecret)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo--t1", ParentSlug: new("demo")})
	w.addWorkspace(t, workspace.Workspace{Slug: "nosecret", GitURL: new("https://github.com/acme/widgets.git")})
	w.addWorkspace(t, workspace.Workspace{Slug: "emptysecret", GitURL: new("https://github.com/acme/widgets.git")})
	w.putSecret(t, "emptysecret", forge.SecretWebhook, "")
	w.addWorkspace(t, workspace.Workspace{Slug: "unset", GitURL: new("https://github.com/acme/widgets.git")})
	w.putSecret(t, "unset", forge.SecretWebhook, hookSecret)
	if status, body, _ := w.send(t, http.MethodDelete, "unset", webhookPath, w.ownerKey, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE the webhook of unset = %d %s, want 204", status, body)
	}

	body := ghIssue("opened", nil)
	var first []byte
	reads.take()
	for i, slug := range []string{"nosuch", "demo--t1", "nosecret", "emptysecret", "unset"} {
		status, answer := w.deliver(t, slug, ghHeaders(fmt.Sprintf("n%d", i), "issues", hookSecret, body), bytes.NewReader(body))
		wantRefusal(t, "POST "+slug, status, answer, http.StatusNotFound, "not_found")
		// Each refusal does the same reads, so that its time does not tell
		// an unknown slug from a workspace.
		want := []string{"workspace " + slug, "secret workspace/" + slug + "/" + forge.SecretWebhook}
		if got := reads.take(); !slices.Equal(got, want) {
			t.Errorf("POST %s read %q, want %q", slug, got, want)
		}
		if e := errorOf(t, answer); e.Message != "not found" {
			t.Errorf("POST %s: message %q, want \"not found\"", slug, e.Message)
		}
		if first == nil {
			first = answer
		} else if !bytes.Equal(answer, first) {
			t.Errorf("POST %s answered %s, not the same bytes as %s", slug, answer, first)
		}
	}
	if n := w.deliveryCount(t); n != 0 {
		t.Errorf("the hub recorded %d deliveries, want none", n)
	}
	// The same delivery reaches a workspace that holds the secret.
	status, answer := w.deliver(t, "demo", ghHeaders("d1", "issues", hookSecret, body), bytes.NewReader(body))
	wantOutcome(t, "POST demo", status, answer, forge.OutcomeRelayed)
}

func TestIngress_RefusesAMissingOrWrongCredentialWithOneMessage(t *testing.T) {
	t.Parallel()
	w := newIngressWorld(t)
	body := ghIssue("opened", nil)
	gh := ghHeaders("d1", "issues", hookSecret, body)
	gl := glHeaders("d1", "Issue Hook", hookSecret)
	cases := []struct {
		name   string
		header http.Header
	}{
		{"no scheme header", withHeader(gh, "X-Hub-Signature-256")},
		{"only the SHA-1 signature header", withHeader(withHeader(gh, "X-Hub-Signature-256"), "X-Hub-Signature", "sha1="+strings.Repeat("0", 40))},
		{"a wrong GitLab token", withHeader(gl, "X-Gitlab-Token", "not-the-secret-at-all")},
		{"an empty GitLab token", withHeader(gl, "X-Gitlab-Token", "")},
		{"the GitLab token twice", withHeader(gl, "X-Gitlab-Token", hookSecret, hookSecret)},
		{"a signature under another secret", withHeader(gh, "X-Hub-Signature-256", signature("another-secret-0123456", body))},
		{"a signature of another body", withHeader(gh, "X-Hub-Signature-256", signature(hookSecret, ghIssue("closed", nil)))},
		{"a signature in upper-case hex", withHeader(gh, "X-Hub-Signature-256", "sha256="+strings.ToUpper(strings.TrimPrefix(signature(hookSecret, body), "sha256=")))},
		{"a signature without its prefix", withHeader(gh, "X-Hub-Signature-256", strings.TrimPrefix(signature(hookSecret, body), "sha256="))},
		{"the right signature twice", withHeader(gh, "X-Hub-Signature-256", signature(hookSecret, body), signature(hookSecret, body))},
		{"a wrong signature beside the right GitLab token", withHeader(withHeader(gh, "X-Hub-Signature-256", signature("another-secret-0123456", body)), "X-Gitlab-Token", hookSecret)},
	}
	var first []byte
	for _, c := range cases {
		status, answer := w.deliver(t, "demo", c.header, bytes.NewReader(body))
		wantRefusal(t, c.name, status, answer, http.StatusUnauthorized, "unauthenticated")
		if e := errorOf(t, answer); e.Message != "delivery signature missing or invalid" {
			t.Errorf("%s: message %q", c.name, e.Message)
		}
		if first == nil {
			first = answer
		} else if !bytes.Equal(answer, first) {
			t.Errorf("%s answered %s, not the same bytes as %s", c.name, answer, first)
		}
	}
	if n := w.deliveryCount(t); n != 0 {
		t.Errorf("the hub recorded %d deliveries, want none", n)
	}
	if n := w.forgeEvents(t); n != 0 {
		t.Errorf("the log holds %d forge events, want none", n)
	}
}

func TestIngress_RecordsAnAuthenticatedDeliveryOfEitherForgeAndAnswersItsOutcome(t *testing.T) {
	t.Parallel()
	w := newIngressWorld(t)
	gh := func(id, eventName string, body []byte, extra ...string) (int, []byte) {
		header := ghHeaders(id, eventName, hookSecret, body)
		for i := 0; i+1 < len(extra); i += 2 {
			header.Set(extra[i], extra[i+1])
		}
		return w.deliver(t, "demo", header, bytes.NewReader(body))
	}
	opened := ghIssue("opened", nil)
	status, answer := gh("d1", "issues", opened)
	wantOutcome(t, "the GitHub delivery d1", status, answer, forge.OutcomeRelayed)
	status, answer = gh("d2", "issues", opened, "X-Gitlab-Token", "wrong")
	wantOutcome(t, "the GitHub delivery d2 with a wrong GitLab token", status, answer, forge.OutcomeRelayed)
	status, answer = gh("d3", "ping", []byte(`{"zen":"Keep it logically awesome."}`))
	wantOutcome(t, "the ping d3", status, answer, forge.OutcomeIgnored)
	status, answer = gh("d4", "issues", ghPayload("opened", "other/repo", map[string]any{"issue": map[string]any{"number": 7}}))
	wantOutcome(t, "the delivery d4 of another repository", status, answer, forge.OutcomeRepositoryMismatch)

	body := glIssueOpen("acme/widgets")
	status, answer = w.deliver(t, "demo", withHeader(glHeaders("u1", "Issue Hook", hookSecret), "Idempotency-Key", "k1"), bytes.NewReader(body))
	wantOutcome(t, "the GitLab delivery k1", status, answer, forge.OutcomeRelayed)
	status, answer = w.deliver(t, "demo", glHeaders("u2", "Issue Hook", hookSecret), bytes.NewReader(body))
	wantOutcome(t, "the GitLab delivery u2", status, answer, forge.OutcomeRelayed)

	for _, want := range []struct {
		id        string
		kind      forge.ForgeKind
		eventName string
	}{{"d1", forge.KindGitHub, "issues"}, {"d2", forge.KindGitHub, "issues"}, {"d3", forge.KindGitHub, "ping"},
		{"k1", forge.KindGitLab, "Issue Hook"}, {"u2", forge.KindGitLab, "Issue Hook"}} {
		if d := w.delivery(t, "demo", want.id); d.Forge != want.kind || d.Event != want.eventName {
			t.Errorf("delivery %s has forge %s and event %q, want %s and %q", want.id, d.Forge, d.Event, want.kind, want.eventName)
		}
	}
	if _, found, err := forge.GetDelivery(context.Background(), w.Store.Main(), "demo", "u1"); err != nil || found {
		t.Errorf("a delivery is recorded under the event UUID u1 (found %v, %v), want it under the Idempotency-Key", found, err)
	}
	if n := w.deliveryCount(t); n != 6 {
		t.Errorf("the hub recorded %d deliveries, want 6", n)
	}
	events := w.eventsOf(t, "parley.forge.issue.", "demo")
	if len(events) != 4 {
		t.Errorf("demo holds %d issue events, want 4", len(events))
	}
	for _, e := range events {
		if strings.Contains(jsonOf(e), hookSecret) {
			t.Errorf("the stored event %s holds the webhook secret", jsonOf(e))
		}
	}
	if strings.Contains(w.Log.String(), hookSecret) {
		t.Error("the hub's log holds the webhook secret")
	}
}

// padded is a JSON object of exactly n bytes.
func padded(n int) []byte {
	const head, tail = `{"zen":"`, `"}`
	return []byte(head + strings.Repeat("a", n-len(head)-len(tail)) + tail)
}

func TestIngress_RefusesABodyOverTheLimit(t *testing.T) {
	t.Parallel()
	w := newIngressWorld(t)
	over, limit := padded(maxDelivery+1), padded(maxDelivery)
	// A refusal before the body is read would race the client still
	// sending it, which then fails on a closed connection; with
	// 100-continue the client sends the body only once the hub reads it.
	expect := func(h http.Header) http.Header { return withHeader(h, "Expect", "100-continue") }
	cases := []struct {
		name      string
		header    http.Header
		body      io.Reader
		status    int
		errorType string
	}{
		{"GitHub, signature valid", expect(ghHeaders("g1", "ping", hookSecret, over)), bytes.NewReader(over), http.StatusRequestEntityTooLarge, "payload_too_large"},
		{"GitHub, signature wrong", expect(ghHeaders("g2", "ping", "another-secret-0123456", over)), bytes.NewReader(over), http.StatusRequestEntityTooLarge, "payload_too_large"},
		{"GitHub, chunked", expect(ghHeaders("g3", "ping", hookSecret, over)), io.MultiReader(bytes.NewReader(over)), http.StatusRequestEntityTooLarge, "payload_too_large"},
		{"GitLab, token right", expect(glHeaders("l1", "Push Hook", hookSecret)), bytes.NewReader(over), http.StatusRequestEntityTooLarge, "payload_too_large"},
		{"GitLab, token wrong", expect(glHeaders("l2", "Push Hook", "not-the-secret-at-all")), bytes.NewReader(over), http.StatusUnauthorized, "unauthenticated"},
	}
	for _, c := range cases {
		status, answer := w.deliver(t, "demo", c.header, c.body)
		wantRefusal(t, c.name, status, answer, c.status, c.errorType)
	}
	if n := w.deliveryCount(t); n != 0 {
		t.Errorf("the hub recorded %d deliveries, want none", n)
	}
	status, answer := w.deliver(t, "demo", ghHeaders("g4", "ping", hookSecret, limit), bytes.NewReader(limit))
	wantOutcome(t, "a body of exactly the limit", status, answer, forge.OutcomeIgnored)
}

func TestIngress_RefusesADeliveryWithoutAValidIdOrEventName(t *testing.T) {
	t.Parallel()
	w := newIngressWorld(t)
	body := ghIssue("opened", nil)
	gh := ghHeaders("d1", "issues", hookSecret, body)
	gl := glHeaders("u1", "Issue Hook", hookSecret)
	cases := []struct {
		name   string
		header http.Header
		status int
		// direct sends the delivery to the handler itself: net/http refuses
		// a control character in a header value before any handler runs,
		// so the handler's own rule is only reached that way.
		direct bool
	}{
		{"GitHub without X-GitHub-Delivery", withHeader(gh, "X-GitHub-Delivery"), http.StatusBadRequest, false},
		{"GitHub without X-GitHub-Event", withHeader(gh, "X-GitHub-Event"), http.StatusBadRequest, false},
		{"GitHub with an empty X-GitHub-Event", withHeader(gh, "X-GitHub-Event", ""), http.StatusBadRequest, false},
		{"GitLab without an id header", withHeader(gl, "X-Gitlab-Event-UUID"), http.StatusBadRequest, false},
		{"GitLab without X-Gitlab-Event", withHeader(gl, "X-Gitlab-Event"), http.StatusBadRequest, false},
		{"an id of 256 characters", withHeader(gh, "X-GitHub-Delivery", strings.Repeat("a", 256)), http.StatusBadRequest, false},
		{"an empty id", withHeader(gh, "X-GitHub-Delivery", ""), http.StatusBadRequest, false},
		{"an id with the byte 0x7F", withHeader(gh, "X-GitHub-Delivery", "a\x7fb"), http.StatusBadRequest, true},
		{"an id with é", withHeader(gh, "X-GitHub-Delivery", "café"), http.StatusBadRequest, false},
		{"an id of 255 characters", withHeader(gh, "X-GitHub-Delivery", strings.Repeat("a", 255)), http.StatusAccepted, false},
		{"an id with a space", withHeader(gh, "X-GitHub-Delivery", "a b"), http.StatusAccepted, false},
	}
	for _, c := range cases {
		var status int
		var answer []byte
		if c.direct {
			req := httptest.NewRequest(http.MethodPost, "/hooks/forge/demo", bytes.NewReader(body))
			req.SetPathValue("slug", "demo")
			req.Header = c.header
			rec := httptest.NewRecorder()
			w.forge.Ingress().ServeHTTP(rec, req)
			status, answer = rec.Code, rec.Body.Bytes()
		} else {
			glBody := c.header.Get("X-Gitlab-Token") != ""
			b := body
			if glBody {
				b = glIssueOpen("acme/widgets")
			}
			status, answer = w.deliver(t, "demo", c.header, bytes.NewReader(b))
		}
		if c.status == http.StatusAccepted {
			wantOutcome(t, c.name, status, answer, forge.OutcomeRelayed)
		} else {
			wantRefusal(t, c.name, status, answer, http.StatusBadRequest, "invalid_request")
		}
	}
	if n := w.deliveryCount(t); n != 2 {
		t.Errorf("the hub recorded %d deliveries, want the 2 accepted", n)
	}
}

func TestIngress_RefusesAMalformedDeliveryButKeepsItAndAnswersARepeatAsADuplicate(t *testing.T) {
	t.Parallel()
	w := newIngressWorld(t)
	form := []byte("payload=" + url.QueryEscape(string(ghIssue("opened", nil))))
	header := withHeader(ghHeaders("m1", "issues", hookSecret, form), "Content-Type", "application/x-www-form-urlencoded")
	status, answer := w.deliver(t, "demo", header, bytes.NewReader(form))
	wantRefusal(t, "the form-encoded delivery m1", status, answer, http.StatusBadRequest, "invalid_request")
	if d := w.delivery(t, "demo", "m1"); d.Outcome != forge.OutcomeMalformed || len(d.Relayed) != 0 {
		t.Errorf("delivery m1 has outcome %s and relayed %v, want malformed and none", d.Outcome, d.Relayed)
	}
	status, answer = w.deliver(t, "demo", header, bytes.NewReader(form))
	if status != http.StatusAccepted || strings.TrimSpace(string(answer)) != `{"outcome":"duplicate"}` {
		t.Errorf("m1 again = %d %s, want 202 {\"outcome\":\"duplicate\"}", status, answer)
	}

	body := ghIssue("opened", nil)
	status, answer = w.deliver(t, "demo", ghHeaders("r1", "issues", hookSecret, body), bytes.NewReader(body))
	wantOutcome(t, "r1", status, answer, forge.OutcomeRelayed)
	status, answer = w.deliver(t, "demo", ghHeaders("r1", "issues", hookSecret, body), bytes.NewReader(body))
	if status != http.StatusAccepted || strings.TrimSpace(string(answer)) != `{"outcome":"duplicate"}` {
		t.Errorf("r1 again = %d %s, want 202 {\"outcome\":\"duplicate\"}", status, answer)
	}
	if n := w.deliveryCount(t); n != 2 {
		t.Errorf("the hub recorded %d deliveries, want 2", n)
	}
	if n := len(w.eventsOf(t, "parley.forge.", "demo")); n != 1 {
		t.Errorf("demo holds %d forge events, want the one of r1", n)
	}
}

func TestIngress_RecordsNothingForAWorkspaceDeletedWhileItsDeliveryWasChecked(t *testing.T) {
	t.Parallel()
	// The ingress reads gone's record before the delete commits; by the
	// time the delivery is recorded the workspace is no longer stored.
	w := newDeliveryWorld(t, deliveryTime, func(_ *world, d *forge.Deps) {
		read := d.Workspaces
		d.Workspaces = func(ctx context.Context, slug string) (workspace.Workspace, bool, error) {
			if slug == "gone" {
				return workspace.Workspace{Slug: "gone", Kind: "repository", Status: workspace.StatusReady,
					Mode: workspace.ModeStandard, GitURL: new("https://github.com/acme/widgets.git")}, true, nil
			}
			return read(ctx, slug)
		}
	})
	w.putSecret(t, "gone", forge.SecretWebhook, hookSecret)
	body := ghIssue("opened", nil)
	status, answer := w.deliver(t, "gone", ghHeaders("d1", "issues", hookSecret, body), bytes.NewReader(body))
	wantRefusal(t, "a delivery to a workspace deleted meanwhile", status, answer, http.StatusNotFound, "not_found")
	if n, events := w.deliveryCount(t), w.forgeEvents(t); n != 0 || events != 0 {
		t.Errorf("the hub holds %d deliveries and %d forge events, want none", n, events)
	}
}

func TestIngress_AnswersAHubFailureAsInternalAndKeepsNothing(t *testing.T) {
	t.Parallel()
	key := func() string {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(k)
	}
	first, other := key(), key()
	log := &failingLog{nth: 2}
	w := newWorldWith(t, hubtest.Config{Env: map[string]string{secret.EnvCurrentKey: first}},
		func(_ *world, d *forge.Deps) { log.next = d.Log; d.Log = log })
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: new("https://github.com/acme/widgets.git")})
	w.putSecret(t, "demo", forge.SecretWebhook, hookSecret)

	// The secret was sealed under a key the hub no longer has.
	w.RestartEnv(t, map[string]string{secret.EnvCurrentKey: other})
	opened := ghIssue("opened", nil)
	status, answer := w.deliver(t, "demo", ghHeaders("d1", "issues", hookSecret, opened), bytes.NewReader(opened))
	wantRefusal(t, "a delivery to a workspace whose secret cannot be opened", status, answer, http.StatusInternalServerError, "internal")
	if n := w.deliveryCount(t); n != 0 {
		t.Errorf("the hub recorded %d deliveries, want none", n)
	}
	w.RestartEnv(t, map[string]string{secret.EnvCurrentKey: first})
	status, answer = w.deliver(t, "demo", ghHeaders("d1", "issues", hookSecret, opened), bytes.NewReader(opened))
	wantOutcome(t, "d1 once the key is back", status, answer, forge.OutcomeRelayed)

	// The second of the three events of the update cannot be recorded.
	log.calls.Store(0)
	log.armed.Store(true)
	update := glLabelUpdate("acme/widgets")
	status, answer = w.deliver(t, "demo", glHeaders("u1", "Issue Hook", hookSecret), bytes.NewReader(update))
	wantRefusal(t, "a delivery whose event cannot be recorded", status, answer, http.StatusInternalServerError, "internal")
	for _, leak := range []string{"ZZMARKER", hookSecret} {
		if strings.Contains(string(answer), leak) || strings.Contains(w.Log.String(), leak) {
			t.Errorf("the answer %s or the hub's log holds %q", answer, leak)
		}
	}
	if n := w.deliveryCount(t); n != 1 {
		t.Errorf("the hub holds %d deliveries, want only d1", n)
	}
	if got := types(w.eventsOf(t, "parley.forge.", "demo")); len(got) != 1 {
		t.Errorf("the events of demo are %v, want only the one of d1", got)
	}
	log.armed.Store(false)
	status, answer = w.deliver(t, "demo", glHeaders("u1", "Issue Hook", hookSecret), bytes.NewReader(update))
	wantOutcome(t, "u1 again", status, answer, forge.OutcomeRelayed)
	if got := types(w.eventsOf(t, "parley.forge.issue.", "demo")); len(got) != 4 {
		t.Errorf("the events of demo are %v, want d1's and the three of u1", got)
	}
}

// newRateLimitedWorld is a hub that admits one REST API request per
// credential and minute, with the user owner, who owns the workspace demo
// and the archived workspace old, both of acme/widgets, and holds the API
// key ownerKey. The world's own setup sends more than one request as the
// admin, so this one is set up in the store.
func newRateLimitedWorld(t *testing.T) *world {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	w := &world{}
	w.Hub = hubtest.StartWith(t, hubtest.Config{
		TOML: "[limits]\nrequests_per_minute = 1\n",
		Env:  map[string]string{secret.EnvCurrentKey: base64.StdEncoding.EncodeToString(k)},
		Compose: func(c *hub.Composition) {
			w.secrets, w.forge, w.events = c.Secrets(), c.Forge(), c.Events()
		},
	})
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		u, org, err := identity.CreateUserAndPersonalOrg(ctx, tx, identity.NewUser{
			Username: "owner", Email: "owner@example.com", Provider: "github", ProviderID: "owner", CreatedAt: deliveryTime,
		}, identity.RoleUser)
		w.owner, w.org = u.ID, org.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w.ownerKey = w.IssueKey(t, w.owner)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: new("https://github.com/acme/widgets.git")})
	w.addWorkspace(t, workspace.Workspace{Slug: "old", GitURL: new("https://github.com/acme/widgets.git"), Status: workspace.StatusArchived})
	return w
}

func TestIngress_TakesADeliveryWithoutACredentialOrRateLimitAgainstTheSecretOfTheMoment(t *testing.T) {
	t.Parallel()
	w := newRateLimitedWorld(t)
	w.putSecret(t, "demo", forge.SecretWebhook, hookSecret)
	body := ghIssue("opened", nil)
	for _, id := range []string{"d1", "d2", "d3"} {
		status, answer := w.deliver(t, "demo", ghHeaders(id, "issues", hookSecret, body), bytes.NewReader(body))
		wantOutcome(t, "delivery "+id, status, answer, forge.OutcomeRelayed)
	}
	status, answer := w.deliver(t, "demo", withHeader(ghHeaders("d4", "issues", hookSecret, body), "Authorization", "Bearer nonsense"), bytes.NewReader(body))
	wantOutcome(t, "a delivery with an invalid bearer token", status, answer, forge.OutcomeRelayed)

	status, answer = w.Call(t, http.MethodPut, "/api/v1/workspaces/demo"+webhookPath, w.ownerKey, nil)
	var rotated struct{ Secret string }
	if status != http.StatusOK || json.Unmarshal(answer, &rotated) != nil || rotated.Secret == "" {
		t.Fatalf("PUT the webhook of demo = %d %s, want 200 with a generated secret", status, answer)
	}
	if status, answer := w.Call(t, http.MethodGet, "/api/v1/workspaces/demo/forge", w.ownerKey, nil); status != http.StatusTooManyRequests {
		t.Fatalf("a second API request of the owner = %d %s, want 429: the rate limit is not in force", status, answer)
	}
	status, answer = w.deliver(t, "demo", ghHeaders("d5", "issues", hookSecret, body), bytes.NewReader(body))
	wantRefusal(t, "a delivery signed with the old secret", status, answer, http.StatusUnauthorized, "unauthenticated")
	status, answer = w.deliver(t, "demo", ghHeaders("d6", "issues", rotated.Secret, body), bytes.NewReader(body))
	wantOutcome(t, "a delivery signed with the new secret", status, answer, forge.OutcomeRelayed)

	w.putSecret(t, "demo", forge.SecretWebhook, "abc")
	gl := glIssueOpen("acme/widgets")
	status, answer = w.deliver(t, "demo", glHeaders("u1", "Issue Hook", "abc"), bytes.NewReader(gl))
	wantOutcome(t, "a GitLab delivery with a secret of 3 characters", status, answer, forge.OutcomeRelayed)

	w.putSecret(t, "old", forge.SecretWebhook, hookSecret)
	status, answer = w.deliver(t, "old", ghHeaders("a1", "issues", hookSecret, body), bytes.NewReader(body))
	wantOutcome(t, "a delivery to the archived workspace", status, answer, forge.OutcomeRelayed)
	if n := len(w.eventsOf(t, "parley.forge.issue.opened", "old")); n != 1 {
		t.Errorf("the archived workspace holds %d opened events, want 1", n)
	}
}
