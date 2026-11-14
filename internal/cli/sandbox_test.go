package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// profilesAnswer is a profile list as a hub with sandboxes answers it.
const profilesAnswer = `{"items":[{"name":"agent","ttl":"4h","ttl_seconds":14400,"env_keys":["A"]}],"next_cursor":null}`

func TestSandboxProfiles_PrintsTheHubsProfileList(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t).answer(http.MethodGet, "/api/v1/sandbox-profiles", http.StatusOK, profilesAnswer)

	code, stdout, stderr := runCLI(t, onHub(hub.URL, "sandbox", "profiles")...)

	if code != 0 {
		t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
	}
	if want := indented(t, profilesAnswer); stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	var list struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil || len(list.Items) == 0 || list.Items[0].Name != "agent" {
		t.Errorf("stdout = %q, want a list whose first item is agent", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want it empty", stderr)
	}
	sent := hub.recorded()
	if len(sent) != 1 {
		t.Fatalf("the hub got %d requests, want 1", len(sent))
	}
	if got := sent[0]; got.method != http.MethodGet || got.rawPath != "/api/v1/sandbox-profiles" || got.query != "" || got.body != "" {
		t.Errorf("sent %s %s ?%q with body %q, want GET /api/v1/sandbox-profiles without a query or body", got.method, got.rawPath, got.query, got.body)
	}
	if auth := sent[0].header.Get("Authorization"); auth != "Bearer k" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer k")
	}
}

func TestSandboxProfiles_PrintsTheRefusalOfAHubWithoutSandboxesOnStderr(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t).answer(http.MethodGet, "/api/v1/sandbox-profiles", http.StatusNotFound,
		`{"error":{"code":404,"message":"sandboxes are not enabled on this hub","error_type":"sandboxes_disabled"}}`)

	code, stdout, stderr := runCLI(t, onHub(hub.URL, "sandbox", "profiles")...)

	if code != 1 {
		t.Errorf("exit status = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want it empty", stdout)
	}
	var env struct {
		Error struct {
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &env); err != nil || env.Error.ErrorType != "sandboxes_disabled" {
		t.Errorf("stderr = %q, want the envelope with error_type sandboxes_disabled", stderr)
	}
	if n := len(hub.recorded()); n != 1 {
		t.Errorf("the hub got %d requests, want 1", n)
	}
}

func TestSandbox_WithoutACommandPrintsItsHelpWithoutSettings(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := runCLI(t, "sandbox")

	if code != 0 {
		t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "profiles") {
		t.Errorf("stdout = %q, want the help listing profiles", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want it empty", stderr)
	}
}
