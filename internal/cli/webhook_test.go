package cli

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// The ids of a subscription and of one of its deliveries, and the route of
// that subscription in the workspace demo, as sent.
const (
	hookID       = "0b5f3c1e-7a2d-4c8e-9f10-2a3b4c5d6e7f"
	hookDelivery = "5d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	hookItem     = "/api/v1/workspaces/demo/webhooks/" + hookID
)

func TestWebhook_SendsTheCommandsRequestAndPrintsTheAnswer(t *testing.T) {
	t.Parallel()
	wantWrites(t, []sentWrite{
		{args: []string{"webhook", "create", "demo", "--url", "https://ci.example/h"}, method: http.MethodPost, path: "demo/webhooks",
			body: `{"url": "https://ci.example/h"}`},
		{args: []string{"webhook", "create", "demo", "--url", "https://ci.example/h", "--types", "parley.change.*,parley.git.push", "--no-forks", "--description", "CI"},
			method: http.MethodPost, path: "demo/webhooks",
			body: `{"url": "https://ci.example/h", "types": ["parley.change.*", "parley.git.push"], "include_forks": false, "description": "CI"}`},
		{args: []string{"webhook", "create", "a/b", "--url", "https://ci.example/h"}, method: http.MethodPost, path: "a%2Fb/webhooks",
			body: `{"url": "https://ci.example/h"}`},
		{args: []string{"webhook", "list", "demo"}, method: http.MethodGet, path: "demo/webhooks"},
		{args: []string{"webhook", "list", "demo", "--state", "failing", "--limit", "10", "--cursor", "c"}, method: http.MethodGet, path: "demo/webhooks",
			query: url.Values{"state": {"failing"}, "limit": {"10"}, "cursor": {"c"}}},
		{args: []string{"webhook", "get", "demo", hookID}, method: http.MethodGet, path: "demo/webhooks/" + hookID},
		{args: []string{"webhook", "update", "demo", hookID, "--forks", "--disable"}, method: http.MethodPatch, path: "demo/webhooks/" + hookID,
			body: `{"include_forks": true, "active": false}`},
		{args: []string{"webhook", "update", "demo", hookID, "--url", "https://ci.example/u", "--types", "a,b", "--description", "", "--enable"},
			method: http.MethodPatch, path: "demo/webhooks/" + hookID,
			body: `{"url": "https://ci.example/u", "types": ["a", "b"], "description": "", "active": true}`},
		{args: []string{"webhook", "update", "demo", hookID, "--no-forks"}, method: http.MethodPatch, path: "demo/webhooks/" + hookID,
			body: `{"include_forks": false}`},
		{args: []string{"webhook", "update", "demo", hookID, "--enable=false", "--no-forks"}, method: http.MethodPatch, path: "demo/webhooks/" + hookID,
			body: `{"include_forks": false}`},
		{args: []string{"webhook", "update", "demo", hookID, "--forks", "--no-forks=false", "--disable=false", "--enable"}, method: http.MethodPatch,
			path: "demo/webhooks/" + hookID, body: `{"include_forks": true, "active": true}`},
		{args: []string{"webhook", "create", "demo", "--url", "https://ci.example/h", "--no-forks=false"}, method: http.MethodPost, path: "demo/webhooks",
			body: `{"url": "https://ci.example/h"}`},
		{args: []string{"webhook", "rotate-secret", "demo", hookID}, method: http.MethodPost, path: "demo/webhooks/" + hookID + "/rotate-secret"},
		{args: []string{"webhook", "deliveries", "demo", hookID}, method: http.MethodGet, path: "demo/webhooks/" + hookID + "/deliveries"},
		{args: []string{"webhook", "deliveries", "demo", hookID, "--status", "failed"}, method: http.MethodGet, path: "demo/webhooks/" + hookID + "/deliveries",
			query: url.Values{"status": {"failed"}}},
		{args: []string{"webhook", "deliveries", "demo", hookID, "--status", "failed", "--limit", "5", "--cursor", "c"}, method: http.MethodGet,
			path: "demo/webhooks/" + hookID + "/deliveries", query: url.Values{"status": {"failed"}, "limit": {"5"}, "cursor": {"c"}}},
		{args: []string{"webhook", "ping", "demo", hookID}, method: http.MethodPost, path: "demo/webhooks/" + hookID + "/ping",
			answer: succeededDelivery, status: http.StatusOK},
		{args: []string{"webhook", "redeliver", "demo", hookID, hookDelivery}, method: http.MethodPost,
			path: "demo/webhooks/" + hookID + "/deliveries/" + hookDelivery + "/redeliver", answer: succeededDelivery, status: http.StatusOK},
	})
}

func TestWebhook_PrintsTheHubsErrorAsReceived(t *testing.T) {
	t.Parallel()
	notFound := `{"error": {"code": 404, "message": "no such event webhook", "error_type": "webhook_not_found"}}`
	for _, tc := range []struct {
		args   []string
		method string
		path   string
	}{
		{[]string{"get", "demo", hookID}, http.MethodGet, hookItem},
		{[]string{"update", "demo", hookID, "--description", "x"}, http.MethodPatch, hookItem},
		{[]string{"delete", "demo", hookID}, http.MethodDelete, hookItem},
		{[]string{"rotate-secret", "demo", hookID}, http.MethodPost, hookItem + "/rotate-secret"},
		{[]string{"ping", "demo", hookID}, http.MethodPost, hookItem + "/ping"},
		{[]string{"deliveries", "demo", hookID}, http.MethodGet, hookItem + "/deliveries"},
		{[]string{"redeliver", "demo", hookID, hookDelivery}, http.MethodPost, hookItem + "/deliveries/" + hookDelivery + "/redeliver"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(tc.method, tc.path, http.StatusNotFound, notFound)

			code, stdout, stderr, sent := runOnHub(t, hub, append([]string{"webhook"}, tc.args...)...)

			wantOnlyRequest(t, sent, tc.method, tc.path)
			wantHubFailure(t, code, stdout, stderr, notFound)
		})
	}
}

// signingSecret is a signing secret as the hub generates it, and
// withSecret a subscription as create and rotate-secret answer it.
const (
	signingSecret = "whsec_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	withSecret    = `{"id": "` + hookID + `", "workspace": "demo", "url": "https://ci.example/h", "state": "active", "secret": "` + signingSecret + `"}`
)

func TestWebhookCreateAndRotateSecret_PrintTheSecretAndWarnItIsShownOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args         []string
		method, path string
		status       int
	}{
		{[]string{"create", "demo", "--url", "https://ci.example/h"}, http.MethodPost, "/api/v1/workspaces/demo/webhooks", http.StatusCreated},
		{[]string{"rotate-secret", "demo", hookID}, http.MethodPost, hookItem + "/rotate-secret", http.StatusOK},
	} {
		t.Run(tc.args[0], func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(tc.method, tc.path, tc.status, withSecret)

			code, stdout, stderr, sent := runOnHub(t, hub, append([]string{"webhook"}, tc.args...)...)

			wantOnlyRequest(t, sent, tc.method, tc.path)
			var printed struct {
				Secret string `json:"secret"`
			}
			oneJSONDocument(t, stdout, &printed)
			if code != 0 || printed.Secret != signingSecret {
				t.Errorf("exit status %d, printed secret %q; want 0 and %q", code, printed.Secret, signingSecret)
			}
			if !strings.Contains(stderr, showOnceWarning) || strings.Contains(stderr, signingSecret) {
				t.Errorf("stderr = %q, want the show-once warning without the secret", stderr)
			}
		})
	}
}

// The delivery records a ping or a redelivery answers, one that succeeded
// and one that failed.
const (
	succeededDelivery = `{"id": "` + hookDelivery + `", "seq": 7, "type": "parley.change.open", "status": "succeeded", "attempts": 2, "response_code": 200, "error": null, "duration_ms": 12, "created_at": "2026-11-19T10:00:00Z", "last_attempt_at": "2026-11-19T10:01:00Z", "next_attempt_at": null}`
	failedDelivery    = `{"id": "` + hookDelivery + `", "seq": 7, "type": "parley.change.open", "status": "failed", "attempts": 2, "response_code": 500, "error": null, "duration_ms": 12, "created_at": "2026-11-19T10:00:00Z", "last_attempt_at": "2026-11-19T10:01:00Z", "next_attempt_at": null}`
)

func TestWebhookPingAndRedeliver_ExitByTheOutcomeOfTheAttempt(t *testing.T) {
	t.Parallel()
	commands := []struct {
		args []string
		path string
	}{
		{[]string{"ping", "demo", hookID}, hookItem + "/ping"},
		{[]string{"redeliver", "demo", hookID, hookDelivery}, hookItem + "/deliveries/" + hookDelivery + "/redeliver"},
	}
	for _, c := range commands {
		for _, tc := range []struct {
			name, record string
			code         int
		}{
			{"succeeded", succeededDelivery, 0},
			{"failed", failedDelivery, 1},
		} {
			t.Run(c.args[0]+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				hub := newStubHub(t).answer(http.MethodPost, c.path, http.StatusOK, tc.record)

				code, stdout, stderr, sent := runOnHub(t, hub, append([]string{"webhook"}, c.args...)...)

				wantOnlyRequest(t, sent, http.MethodPost, c.path)
				var got, want map[string]any
				oneJSONDocument(t, stdout, &got)
				_ = json.Unmarshal([]byte(tc.record), &want)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("stdout = %q, want the record %s", stdout, tc.record)
				}
				if code != tc.code {
					t.Errorf("exit status = %d, want %d", code, tc.code)
				}
				if tc.code == 0 {
					if stderr != "" {
						t.Errorf("stderr = %q, want it empty", stderr)
					}
					return
				}
				if envCode, _, errorType := clientEnvelope(t, stderr); envCode != 0 || errorType != "delivery_failed" {
					t.Errorf("envelope code %v, error_type %q; want 0 and delivery_failed", envCode, errorType)
				}
			})
		}
	}
}

func TestWebhookDelete_PrintsTheConfirmation(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t).answer(http.MethodDelete, hookItem, http.StatusNoContent, "")

	code, stdout, stderr, sent := runOnHub(t, hub, "webhook", "delete", "demo", hookID)

	wantOnlyRequest(t, sent, http.MethodDelete, hookItem)
	wantResult(t, code, stdout, stderr, `{"status": "deleted", "id": "`+hookID+`"}`)
}

func TestWebhook_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	wantRefused(t, [][]string{
		{"webhook", "create", "demo"},
		{"webhook", "create", "demo", "--url", "https://ci.example/h", "--forks"},
		{"webhook", "update", "demo", hookID},
		{"webhook", "update", "demo", hookID, "--forks", "--no-forks"},
		{"webhook", "update", "demo", hookID, "--enable", "--disable"},
		{"webhook", "update", "demo", hookID, "--enable=false"},
		{"webhook", "update", "demo", hookID, "--forks=false"},
		{"webhook", "update", "demo", hookID, "--disable=false", "--no-forks=false"},
		{"webhook", "get", "demo"},
		{"webhook", "redeliver", "demo", hookID},
		{"webhook", "ping", "demo", hookID, "extra"},
		{"webhook", "list", "demo", "--color"},
		{"webhook", "list"},
		{"webhook", "delete", "demo"},
		{"webhook", "rotate-secret", "demo", hookID, "extra"},
		{"webhook", "deliveries", "demo"},
		{"webhook", "deliveries", "demo", hookID, "--state", "failed"},
	})
}
