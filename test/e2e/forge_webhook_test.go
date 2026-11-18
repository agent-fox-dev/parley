package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
)

// webhookHub starts the hub of the workspace demo, whose origin repository
// is acme/widgets and whose forge_api_url is the fake forge's GitHub API,
// and returns it with the fake, to count what the hub asks the forge, and
// a parley that runs the CLI as the workspace's owner.
func webhookHub(t *testing.T) (h *workspaceHub, fake *forgetest.Fake, parley func(args ...string) ran) {
	t.Helper()
	fake = forgetest.New(t)
	h, _ = forgeHub(t, fake, "demo", "acme/widgets.git", "tok-webhook", fake.URL+"/github")
	parley = func(args ...string) ran {
		var stdout, stderr bytes.Buffer
		code := cli.Run(context.Background(), append([]string{"--endpoint-url", h.URL(""), "--api-key", h.key}, args...), &stdout, &stderr)
		return ran{code, stdout.String(), stderr.String()}
	}
	return h, fake, parley
}

// deliver posts body to the hub's ingress of demo as a forge does, with
// header and no credential, and returns the status and the decoded answer.
func deliver(t *testing.T, h *workspaceHub, body []byte, header map[string]string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.URL("/hooks/forge/demo"), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range header {
		req.Header.Set(name, value)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /hooks/forge/demo: %v", err)
	}
	defer resp.Body.Close()
	var answer map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("POST /hooks/forge/demo = %d with a body that is not JSON: %v", resp.StatusCode, err)
	}
	return resp.StatusCode, answer
}

// eventTypes are the types of events, in their order.
func eventTypes(events []map[string]any) []string {
	types := []string{}
	for _, ev := range events {
		types = append(types, ev["type"].(string))
	}
	return types
}

func TestForgeWebhook_AGitHubDeliveryIsRelayedOnceAndReadBackFromTheShell(t *testing.T) {
	parallel(t)
	h, fake, parley := webhookHub(t)

	set := parley("forge", "webhook", "set", "demo")
	if set.code != 0 {
		t.Fatalf("forge webhook set exited %d, stderr %q; want 0", set.code, set.stderr)
	}
	answer := set.json(t)
	secret, _ := answer["secret"].(string)
	url, _ := answer["url"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(secret) || !strings.HasSuffix(url, "/hooks/forge/demo") {
		t.Fatalf("forge webhook set printed %v, want a secret of 64 hex characters and a url ending in /hooks/forge/demo", answer)
	}
	if !strings.Contains(set.stderr, "will not be shown again") {
		t.Errorf("forge webhook set wrote %q on stderr, want a note that the secret will not be shown again", set.stderr)
	}

	body := []byte(`{"action": "labeled", "issue": {"number": 7, "title": "Crash", "html_url": "https://github.com/acme/widgets/issues/7",
		"labels": [{"name": "bug"}]}, "label": {"name": "bug"}, "repository": {"full_name": "acme/widgets"}, "sender": {"login": "kim"}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	header := map[string]string{
		"X-Hub-Signature-256": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
		"X-GitHub-Delivery":   "d-1",
		"X-GitHub-Event":      "issues",
	}
	status1, first := deliver(t, h, body, header)
	status2, again := deliver(t, h, body, header)
	if status1 != http.StatusAccepted || first["outcome"] != "relayed" || status2 != http.StatusAccepted || again["outcome"] != "duplicate" {
		t.Fatalf("the two deliveries = %d %v and %d %v, want 202 relayed and 202 duplicate", status1, first, status2, again)
	}

	listed := parley("forge", "deliveries", "demo")
	var page struct {
		Items []struct {
			Outcome string   `json:"outcome"`
			Target  string   `json:"target"`
			Relayed []string `json:"relayed"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(listed.stdout), &page); err != nil || listed.code != 0 {
		t.Fatalf("forge deliveries exited %d with %q (%v), stderr %q; want 0 and a page", listed.code, listed.stdout, err, listed.stderr)
	}
	if len(page.Items) != 1 || page.Items[0].Outcome != "relayed" || page.Items[0].Target != "origin" ||
		!reflect.DeepEqual(page.Items[0].Relayed, []string{"parley.forge.issue.labeled"}) {
		t.Errorf("forge deliveries printed %+v, want one relayed delivery of the origin that relayed parley.forge.issue.labeled", page.Items)
	}

	events := h.forgeEvents(t, "demo")
	if got, want := eventTypes(events), []string{"parley.forge.webhook.set", "parley.forge.issue.labeled"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the forge events of demo are %v, want %v", got, want)
	}
	labeled := events[1]
	if meta, _ := labeled["metadata"].(map[string]any); labeled["actor_type"] != "forge" || meta["delivery_id"] != "d-1" {
		t.Errorf("parley.forge.issue.labeled has actor_type %v and metadata %v, want forge and delivery_id d-1", labeled["actor_type"], meta)
	}
	if n := fake.Count(); n != 0 {
		t.Errorf("the forge API received %d requests, want none", n)
	}
}

func TestForgeWebhook_AGitLabLabelUpdateIsRelayedInOrderUntilTheWebhookIsUnset(t *testing.T) {
	parallel(t)
	const token = "gitlab-token-0123456789"
	h, _, parley := webhookHub(t)

	set := parley("forge", "webhook", "set", "demo", "--secret", token)
	if _, shown := set.json(t)["secret"]; set.code != 0 || shown {
		t.Fatalf("forge webhook set --secret exited %d and printed %q; want 0 and no secret", set.code, set.stdout)
	}

	body := []byte(`{"object_kind": "issue", "user": {"username": "kim"}, "project": {"path_with_namespace": "acme/widgets"},
		"object_attributes": {"iid": 3, "action": "update", "title": "Renamed", "url": "https://gitlab.example.com/acme/widgets/-/issues/3"},
		"labels": [{"title": "alpha"}, {"title": "beta"}],
		"changes": {"title": {"previous": "Old", "current": "Renamed"},
			"labels": {"previous": [{"title": "old"}], "current": [{"title": "beta"}, {"title": "alpha"}]}}}`)
	header := map[string]string{"X-Gitlab-Token": token, "X-Gitlab-Event": "Issue Hook", "X-Gitlab-Event-UUID": "u-1"}
	if status, answer := deliver(t, h, body, header); status != http.StatusAccepted || answer["outcome"] != "relayed" {
		t.Fatalf("the delivery = %d %v, want 202 relayed", status, answer)
	}

	var issueEvents [][2]any
	for _, ev := range h.forgeEvents(t, "demo") {
		typ := ev["type"].(string)
		if !strings.HasPrefix(typ, "parley.forge.issue.") {
			continue
		}
		meta, _ := ev["metadata"].(map[string]any)
		if meta["delivery_id"] != "u-1" {
			t.Errorf("%s has delivery_id %v, want u-1", typ, meta["delivery_id"])
		}
		issueEvents = append(issueEvents, [2]any{typ, meta["label"]})
	}
	want := [][2]any{
		{"parley.forge.issue.edited", nil},
		{"parley.forge.issue.unlabeled", "old"},
		{"parley.forge.issue.labeled", "alpha"},
		{"parley.forge.issue.labeled", "beta"},
	}
	if !reflect.DeepEqual(issueEvents, want) {
		t.Errorf("the issue events of demo are %v, want %v", issueEvents, want)
	}

	unset := parley("forge", "webhook", "unset", "demo")
	if got, want := unset.json(t), map[string]any{"status": "unset", "workspace": "demo"}; unset.code != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("forge webhook unset exited %d and printed %v, want 0 and %v", unset.code, got, want)
	}
	header["X-Gitlab-Event-UUID"] = "u-2"
	status, answer := deliver(t, h, body, header)
	if refusal, _ := answer["error"].(map[string]any); status != http.StatusNotFound || refusal["error_type"] != "not_found" {
		t.Errorf("the delivery after the unset = %d %v, want 404 not_found", status, answer)
	}

	code, listed := h.Call(t, http.MethodGet, "/api/v1/workspaces/demo/forge/deliveries", h.key, nil)
	var page struct {
		Items []struct {
			Forge   string   `json:"forge"`
			Relayed []string `json:"relayed"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listed, &page); err != nil || code != http.StatusOK {
		t.Fatalf("GET …/forge/deliveries = %d %s (%v), want 200 and a page", code, listed, err)
	}
	if len(page.Items) != 1 || page.Items[0].Forge != "gitlab" || len(page.Items[0].Relayed) != 4 {
		t.Errorf("the deliveries of demo are %+v, want the one GitLab delivery that relayed four events", page.Items)
	}
}
