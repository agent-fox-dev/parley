package acceptance

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// TestCarried_OneJourneyPerFeature takes one journey through each feature
// the product carries, driven by the parley CLI. Each journey is the
// feature's representative path only: its other cases are the feature's
// own tests.
func TestCarried_OneJourneyPerFeature(t *testing.T) {
	t.Parallel()
	t.Run("workspace", carriedWorkspace)
	t.Run("carry-patch", carriedCarryPatch)
	t.Run("secrets", carriedSecretsAndVariables)
	t.Run("forge", carriedForge)
	t.Run("sessions", carriedSessionAuditAndMetrics)
}

// carriedWorkspace takes a workspace through its life: created and ready,
// synced after its origin moved, archived, reactivated, archived again and
// deleted.
func carriedWorkspace(t *testing.T) {
	t.Parallel()
	hub := harness.Start(t)
	key := hub.User("alice").Key
	origin := hub.Origin()

	created := hub.CLI(key, "workspace", "create", "--slug", "life", "--git-url", origin.URL(), "--wait").Must(t).JSON()
	if created["status"] != "ready" {
		t.Fatalf("parley workspace create --wait printed the status %v, want ready", created["status"])
	}

	head := origin.CommitFiles("main", "Add news", map[string]string{"news.txt": "news\n"})
	synced := hub.CLI(key, "workspace", "sync", "life").Must(t).JSON()
	if b := syncedBranch(synced, "main"); b["result"] != "fast_forward" || b["local_sha"] != head {
		t.Errorf("the sync's main is %v, want fast_forward to %s", b, head)
	}

	if s := hub.CLI(key, "workspace", "archive", "life").Must(t).JSON()["status"]; s != "archived" {
		t.Errorf("parley workspace archive printed the status %v, want archived", s)
	}
	if s := hub.CLI(key, "workspace", "reactivate", "life", "--wait").Must(t).JSON()["status"]; s != "ready" {
		t.Errorf("parley workspace reactivate --wait printed the status %v, want ready", s)
	}
	hub.CLI(key, "workspace", "archive", "life").Must(t)
	if s := hub.CLI(key, "workspace", "delete", "life", "--confirm").Must(t).JSON()["status"]; s != "deleted" {
		t.Errorf("parley workspace delete printed the status %v, want deleted", s)
	}

	got := hub.CLI(key, "workspace", "get", "life")
	if got.Code != 1 || !strings.Contains(got.Stderr, `"not_found"`) {
		t.Errorf("parley workspace get of the deleted workspace exited %d with %q, want 1 and not_found", got.Code, got.Stderr)
	}
}

// syncedBranch is the entry of the sync answer's branch list named name, nil
// when there is none.
func syncedBranch(synced map[string]any, name string) map[string]any {
	for _, b := range objectsOf(synced, "branches") {
		if b["name"] == name {
			return b
		}
	}
	return nil
}

// carriedCarryPatch rebuilds a carry-patch workspace after its upstream
// moved: the integration branch becomes upstream's head plus the patch.
func carriedCarryPatch(t *testing.T) {
	t.Parallel()
	hub := harness.Start(t)
	key := hub.User("alice").Key
	upstream := hub.Origin()
	origin := hub.Downstream(upstream)
	patch := origin.PatchBranch("fix-1", map[string]string{"fix-1.txt": "the fix\n"})

	created := hub.CLI(key, "workspace", "create", "--slug", "carry", "--git-url", origin.URL(),
		"--workspace-mode", "carry_patch", "--upstream-url", upstream.URL(), "--wait").Must(t).JSON()
	upstreamHead := upstream.CommitFiles("main", "Upstream moves on", map[string]string{"upstream.txt": "later\n"})

	hub.CLI(key, "patch", "add", "carry", "--branch", "fix-1").Must(t)
	rebuild := hub.CLI(key, "rebuild", "submit", "carry", "--wait").Must(t).JSON()
	if rebuild["status"] != "completed" {
		t.Fatalf("parley rebuild submit --wait printed the status %v, want completed", rebuild["status"])
	}

	clone := hub.Clone(harness.Str(created["hub_url"]), key)
	if parent := hub.Rev(clone, "origin/deploy~1"); parent != upstreamHead {
		t.Errorf("deploy~1 is %s, want upstream's head %s", parent, upstreamHead)
	}
	if subject := hub.Git(clone, "", "log", "-1", "--format=%s", "origin/deploy").Must(t).Stdout; strings.TrimSpace(subject) != "Patch fix-1" {
		t.Errorf("deploy's head is %q, want the patch's commit Patch fix-1 (%s)", strings.TrimSpace(subject), patch)
	}
	if content := hub.Git(clone, "", "show", "origin/deploy:fix-1.txt").Must(t).Stdout; content != "the fix\n" {
		t.Errorf("deploy holds fix-1.txt as %q, want the patch's file", content)
	}
}

// carriedSecretsAndVariables stores a workspace secret and a workspace
// variable: no output ever shows the secret's value, and the variable
// resolves with its origin.
func carriedSecretsAndVariables(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	const value = "s3cr3t-value"
	var printed []string
	cli := func(args ...string) harness.Result {
		t.Helper()
		r := hub.CLI(demo.Owner.Key, args...)
		printed = append(printed, r.Stdout, r.Stderr)
		return r.Must(t)
	}

	cli("secrets", "create", "API_TOKEN="+value, "--workspace", demo.Slug)
	listed := cli("secrets", "list", "--workspace", demo.Slug)
	if !slices.Contains(members(listed.JSON(), "items", "key"), "API_TOKEN") {
		t.Errorf("parley secrets list printed %s, want API_TOKEN among the keys", listed.Stdout)
	}
	cli("vars", "create", "REGION=eu", "--workspace", demo.Slug)
	resolved := cli("vars", "resolve", demo.Slug).JSON()

	var region map[string]any
	for _, v := range objectsOf(resolved, "items") {
		if v["key"] == "REGION" {
			region = v
		}
	}
	if region["value"] != "eu" || region["origin"] != "workspace" {
		t.Errorf("parley vars resolve gave REGION as %v, want the value eu from the workspace", region)
	}
	for _, out := range printed {
		if strings.Contains(out, value) {
			t.Errorf("a command printed the secret's value: %q", out)
		}
	}
}

// carriedForge points a workspace at the fake forge and lists the issues
// it holds.
func carriedForge(t *testing.T) {
	t.Parallel()
	forge := harness.NewForge(t)
	forge.SetIssues("acme", "widgets",
		harness.Issue{Number: 7, Title: "Widgets wobble"},
		harness.Issue{Number: 9, Title: "Widgets squeak"})
	hub := harness.Start(t, harness.Options{Env: forge.Env()})
	alice := hub.User("alice")
	demo := hub.Register(alice, "demo", hub.Origin(harness.WithRepoPath("acme/widgets.git")), nil)

	hub.CLI(alice.Key, "workspace", "update", demo.Slug, "--setting", "forge_api_url="+forge.URL).Must(t)
	issues := hub.CLI(alice.Key, "issue", "list", demo.Slug).Must(t).JSON()
	got := numbersAt(objectsOf(issues, "items"), "number")
	slices.Sort(got)
	if !slices.Equal(got, []int{7, 9}) {
		t.Errorf("parley issue list printed the issues %v, want the forge's 7 and 9", got)
	}
}

// carriedSessionAuditAndMetrics runs an agent session: its usage, a claim
// made under it, the audit trail that finds the claim by the session, the
// metric of active sessions, and its completion.
func carriedSessionAuditAndMetrics(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key

	session := harness.Str(hub.CLI(key, "session", "start", demo.Slug, "--model", "m").Must(t).JSON()["id"])
	if session == "" {
		t.Fatal("parley session start printed no id")
	}
	hub.CLI(key, "session", "usage", session, "--model", "m", "--input", "10", "--output", "5").Must(t)
	hub.CLI(key, "claim", "create", demo.Slug, "--paths", "src/**", "--intent", "tidy up", "--session", session).Must(t)

	audit := hub.CLI(key, "audit", "query", "--session", session).Must(t).JSON()
	if types := members(audit, "items", "type"); !slices.Contains(types, "parley.claim.create") {
		t.Errorf("parley audit query --session printed the types %v, want parley.claim.create among them", types)
	}

	if body := string(hub.Call(http.MethodGet, "/metrics", "", nil).Want(t, http.StatusOK).Raw); !strings.Contains(body, "parley_agent_sessions_active") {
		t.Error("GET /metrics does not expose parley_agent_sessions_active")
	}
	// The gauge is set by the maintenance pass a start runs, then hourly:
	// a restart shows the session, which stays active across it.
	hub.Stop()
	hub.Start()
	series := `parley_agent_sessions_active{workspace="` + demo.Slug + `"} 1`
	harness.Eventually(t, "GET /metrics to count the active session of "+demo.Slug, func() bool {
		return strings.Contains(string(hub.Call(http.MethodGet, "/metrics", "", nil).Raw), series)
	})

	if s := hub.CLI(key, "session", "complete", session).Must(t).JSON()["status"]; s != "completed" {
		t.Errorf("parley session complete printed the status %v, want completed", s)
	}
}
