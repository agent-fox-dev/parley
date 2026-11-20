package acceptance

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// Every scenario here runs against a real OpenShell gateway, named by
// harness.Gateway's variable, or is skipped. Each starts its hub with the
// profiles it names and deletes every sandbox it created when it ends.

// gatewayHub starts a hub connected to the gateway, with the [sandbox]
// settings given and the profiles named, and the cast around demo.
func gatewayHub(t *testing.T, settings string, profiles ...string) (*harness.Hub, *harness.Cast) {
	t.Helper()
	hub := harness.Start(t, harness.SandboxOptions(t, settings, profiles...))
	return hub, hub.Cast()
}

// storeSecrets stores the workspace secrets of demo, by name, as its owner.
func storeSecrets(t *testing.T, hub *harness.Hub, cast *harness.Cast, secrets map[string]string) {
	t.Helper()
	for name, value := range secrets {
		hub.CLI(cast.OwnerKey, "secrets", "create", name+"="+value, "--workspace", cast.Demo.Slug).Must(t)
	}
}

// commitIn runs script with sh -c in s's working directory and fails the
// test unless it exits 0.
func commitIn(t *testing.T, s *harness.Sandbox, script string) {
	t.Helper()
	if e := s.Exec("sh", "-c", script); e.ExitCode != 0 {
		t.Fatalf("%q in sandbox %s exited %d:\n%s%s", script, s.ID, e.ExitCode, e.Stdout, e.Stderr)
	}
}

// gitAs is git with an author, which a sandbox's image need not configure.
const gitAs = "git -c user.name=Agent -c user.email=agent@example.test"

// curl asks for target from inside s and returns the exit code.
func curl(s *harness.Sandbox, target string) int {
	return s.Exec("curl", "-fsS", "--max-time", "10", target).ExitCode
}

// proposalFor is the open proposal among items whose rule names host, nil
// when there is none: the host is only inside the rule and its summary.
func proposalFor(items []map[string]any, host string) map[string]any {
	for _, p := range items {
		if p["state"] == "open" && names(p, host) {
			return p
		}
	}
	return nil
}

// names reports whether the rule or the summary of a proposal or a rule
// names host.
func names(obj map[string]any, host string) bool {
	rule, _ := json.Marshal(obj["rule"])
	return strings.Contains(string(rule), host) || strings.Contains(harness.Str(obj["summary"]), host)
}

// openProposal asks for target from inside s, which no rule lets reach it,
// and returns the open proposal the denial raised, once the hub lists it.
func openProposal(t *testing.T, hub *harness.Hub, s *harness.Sandbox, target, host string) map[string]any {
	t.Helper()
	if code := curl(s, target); code == 0 {
		t.Fatalf("sandbox %s reached %s with no rule allowing it", s.ID, target)
	}
	var p map[string]any
	harness.Eventually(t, "an open proposal for "+host+" in sandbox "+s.ID, func() bool {
		p = proposalFor(hub.Get(s.Credential, s.Path("/proposals")).Items(), host)
		return p != nil
	})
	return p
}

// denied reports whether the event log holds parley.sandbox.deny for the
// sandbox s and host.
func denied(t *testing.T, hub *harness.Hub, s *harness.Sandbox, host string) bool {
	t.Helper()
	for _, e := range eventLog(t, hub, s.Credential, "workspace="+s.Workspace+"&types=parley.sandbox.deny", 0) {
		if e["resource_id"] == s.ID && harness.Field(e, "metadata.host") == host {
			return true
		}
	}
	return false
}

func TestSandbox_ForkWithSandboxChecksOutAndPushOpensAChange(t *testing.T) {
	harness.Require(t, harness.Gateway)
	t.Parallel()
	hub, cast := gatewayHub(t, "", "default")

	r := hub.Call(http.MethodPost, "/api/v1/workspaces/demo/forks", cast.OwnerKey,
		map[string]any{"sandbox": map[string]any{"profile": "default"}})
	fork := harness.Str(harness.Field(r.Body, "workspace.slug"))
	var s *harness.Sandbox
	if obj, ok := r.Body["sandbox"].(map[string]any); ok {
		s = hub.AdoptSandbox(cast.OwnerKey, fork, obj)
	}
	r.Want(t, http.StatusCreated)
	if kind := harness.Field(r.Body, "workspace.kind"); kind != "fork" || s == nil || s.ID == "" {
		t.Fatalf("the fork-create answer holds a workspace of kind %v and the sandbox %v, want a fork and a sandbox:\n%s", kind, s, r.Raw)
	}
	s.WaitReady()

	head := harness.Str(hub.Get(cast.OwnerKey, "/api/v1/workspaces/"+fork+"/scm/refs/main").Body["sha"])
	e := hub.CLI(cast.OwnerKey, "sandbox", "exec", fork, s.ID, "--", "git", "rev-parse", "HEAD")
	if e.Code != 0 || strings.TrimSpace(e.Stdout) != head {
		t.Errorf("git rev-parse HEAD in the sandbox exited %d printing %q, want the fork's main %s:\n%s", e.Code, e.Stdout, head, e.Stderr)
	}
	// The hub requires an intent to open a change.
	push := hub.CLI(cast.OwnerKey, "sandbox", "exec", fork, s.ID, "--", "sh", "-c",
		"echo hi > s.txt && git add s.txt && "+gitAs+" commit -q -m s && "+
			"git push -o parley.change -o 'parley.intent=add s.txt' origin HEAD:refs/heads/work")
	if push.Code != 0 {
		t.Fatalf("the commit and push in the sandbox exited %d:\n%s%s", push.Code, push.Stdout, push.Stderr)
	}
	open := hub.Get(cast.OwnerKey, "/api/v1/workspaces/demo/changes?state=open").Items()
	if len(open) != 1 || harness.Field(open[0], "source.workspace_slug") != fork {
		t.Errorf("demo's open changes are %v, want one whose source workspace is %s", open, fork)
	}
}

func TestSandbox_WorkloadSeesPlaceholdersOnly(t *testing.T) {
	harness.Require(t, harness.Gateway)
	t.Parallel()
	hub, cast := gatewayHub(t, "", "cred", "exposing")
	values := map[string]string{"MODEL_KEY": "mk-real-value", "DEPLOY_KEY": "dk-real-value"}
	storeSecrets(t, hub, cast, values)

	aliceFork := hub.Fork(cast.OwnerKey, "demo", nil, nil)
	s1 := hub.Sandbox(cast.OwnerKey, aliceFork.Slug, map[string]any{"profile": "cred"})
	env := s1.Exec("env").Stdout
	var key string
	for _, line := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(line, "PARLEY_API_KEY="); ok {
			key = v
		}
	}
	// The runtime, not the hub, puts the hub credential's placeholder there.
	if key == "" {
		t.Errorf("the sandbox's environment holds no PARLEY_API_KEY, want the hub credential's placeholder:\n%s", env)
	} else if st := hub.Call(http.MethodGet, "/api/v1/user", key, nil).Status; st != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/user with the sandbox's PARLEY_API_KEY answered %d, want 401: it is a real credential", st)
	}
	for name, value := range values {
		if strings.Contains(env, value) {
			t.Errorf("the environment of a sandbox exposing no secret holds the value of %s", name)
		}
	}

	// bob may only read demo, so the parent's secret is not his to expose.
	bobFork := hub.Fork(cast.MemberKey, "demo", nil, nil)
	s2 := hub.Sandbox(cast.MemberKey, bobFork.Slug, map[string]any{"profile": "exposing"})
	if strings.Contains(s2.Exec("env").Stdout, values["DEPLOY_KEY"]) {
		t.Errorf("the environment of the read-only member's sandbox holds the value of the parent's DEPLOY_KEY")
	}
	if got := s2.Get(); strings.Contains(string(got.Raw), "DEPLOY_KEY") {
		t.Errorf("the read-only member's sandbox names an exposed DEPLOY_KEY: %s", got.Raw)
	}
}

func TestSandbox_ExposedSecretIsReadableInsideAndNamedNotShownOutside(t *testing.T) {
	harness.Require(t, harness.Gateway)
	t.Parallel()
	hub, cast := gatewayHub(t, "", "exposing")
	const value = "dk-real-value"
	storeSecrets(t, hub, cast, map[string]string{"DEPLOY_KEY": value})

	fork := hub.Fork(cast.OwnerKey, "demo", nil, nil)
	s := hub.Sandbox(cast.OwnerKey, fork.Slug, map[string]any{
		"profile": "exposing", "command": []string{"sh", "-c", `echo "$DEPLOY_KEY"; exec sleep 86400`},
	})
	if got := s.Exec("printenv", "DEPLOY_KEY").Stdout; got != value+"\n" {
		t.Errorf("printenv DEPLOY_KEY in the sandbox printed %q, want the secret's value", got)
	}

	created, _ := json.Marshal(eventsOf(eventLog(t, hub, cast.OwnerKey, "workspace="+fork.Slug+"&types=parley.sandbox.create", 0), s.ID))
	for what, doc := range map[string]string{
		"GET /api/v1/sandbox-profiles":    string(hub.Get(cast.OwnerKey, "/api/v1/sandbox-profiles").Raw),
		"the sandbox object":              string(s.Get().Raw),
		"the parley.sandbox.create event": string(created),
	} {
		if !strings.Contains(doc, "DEPLOY_KEY") || strings.Contains(doc, value) {
			t.Errorf("%s names DEPLOY_KEY: %t, holds its value: %t, want named and not shown:\n%s",
				what, strings.Contains(doc, "DEPLOY_KEY"), strings.Contains(doc, value), doc)
		}
	}

	var logs string
	harness.Eventually(t, "the sandbox's stored log to hold the redacted line", func() bool {
		logs = hub.CLI(cast.OwnerKey, "sandbox", "logs", fork.Slug, s.ID).Stdout
		return strings.Contains(logs, "[redacted]")
	})
	if strings.Contains(logs, value) {
		t.Errorf("parley sandbox logs printed the secret's value:\n%s", logs)
	}
}

// eventsOf are the events among all whose resource is the sandbox id.
func eventsOf(all []map[string]any, id string) []map[string]any {
	var of []map[string]any
	for _, e := range all {
		if e["resource_id"] == id {
			of = append(of, e)
		}
	}
	return of
}

func TestSandbox_RefusalsCredentialMissingTokenBoundInsufficientScope(t *testing.T) {
	harness.Require(t, harness.Gateway)
	t.Parallel()
	hub, cast := gatewayHub(t, "", "default", "needs")
	forks := func() int { return len(hub.Get(cast.OwnerKey, "/api/v1/workspaces/demo/forks").Items()) }

	n0 := forks()
	r := hub.Call(http.MethodPost, "/api/v1/workspaces/demo/forks", cast.OwnerKey,
		map[string]any{"sandbox": map[string]any{"profile": "needs"}})
	if obj, ok := r.Body["sandbox"].(map[string]any); ok {
		hub.AdoptSandbox(cast.OwnerKey, harness.Str(harness.Field(r.Body, "workspace.slug")), obj)
	}
	if msg := harness.Str(harness.Field(r.Body, "error.message")); r.Status != http.StatusUnprocessableEntity ||
		r.ErrorType() != "credential_missing" || !strings.Contains(msg, "ABSENT_KEY") {
		t.Errorf("a fork with a sandbox needing an absent secret answered %d %s, want 422 credential_missing naming ABSENT_KEY", r.Status, r.Raw)
	}
	if n := forks(); n != n0 {
		t.Errorf("demo has %d forks after the refused create, want %d: the fork was left behind", n, n0)
	}

	target, host := harness.Page(t)
	f := hub.Fork(cast.OwnerKey, "demo", harness.AgentScopes, nil)
	s := hub.Sandbox(cast.OwnerKey, f.Slug, map[string]any{"profile": "default"})
	scopes, _ := harness.Field(s.Body, "hub_access.scopes").([]any)
	if !slices.Contains(scopes, any("git:write")) || !slices.Contains(scopes, any("changes:write")) {
		t.Fatalf("the sandbox's hub credential holds %v, want git:write and changes:write", scopes)
	}
	p := harness.Str(openProposal(t, hub, s, target, host)["id"])

	a := hub.Call(http.MethodPost, s.Path("/proposals/"+p+"/approve"), f.Token, map[string]any{})
	if a.Status != http.StatusForbidden || a.ErrorType() != "token_bound" {
		t.Errorf("the approval with the fork's bound token answered %d %s, want 403 token_bound", a.Status, a.Raw)
	}
	if proposalFor(hub.Get(cast.OwnerKey, s.Path("/proposals")).Items(), host) == nil {
		t.Errorf("proposal %s is no longer open after the refused approval", p)
	}

	pat := hub.PAT(cast.Alice, []string{"sandboxes:read", "sandboxes:write"})
	x := hub.Call(http.MethodPost, s.Path("/exec"), pat, map[string]any{"command": []string{"touch", "/tmp/ran"}})
	if x.Status != http.StatusForbidden || x.ErrorType() != "insufficient_scope" {
		t.Errorf("an exec with a PAT lacking changes:write answered %d %s, want 403 insufficient_scope", x.Status, x.Raw)
	}
	if code := s.Exec("test", "-e", "/tmp/ran").ExitCode; code == 0 {
		t.Errorf("/tmp/ran exists in the sandbox: the refused exec ran")
	}
}

func TestSandbox_DeniedHostIsReportedAndOpensAfterApproval(t *testing.T) {
	harness.Require(t, harness.Gateway)
	t.Parallel()
	hub, cast := gatewayHub(t, "", "default")
	target, host := harness.Page(t)
	fork := hub.Fork(cast.OwnerKey, "demo", nil, nil)
	s := hub.Sandbox(cast.OwnerKey, fork.Slug, map[string]any{"profile": "default"})

	if code := curl(s, target); code == 0 {
		t.Fatalf("sandbox %s reached %s with no rule allowing it", s.ID, target)
	}
	harness.EventuallyWithin(t, 20*time.Second, "parley.sandbox.deny for "+host, func() bool { return denied(t, hub, s, host) })
	var p map[string]any
	harness.Eventually(t, "parley sandbox proposals to list an open proposal for "+host, func() bool {
		listed := hub.CLI(cast.OwnerKey, "sandbox", "proposals", fork.Slug, s.ID).Must(t)
		items, _ := listed.JSON()["items"].([]any)
		var objs []map[string]any
		for _, it := range items {
			m, _ := it.(map[string]any)
			objs = append(objs, m)
		}
		p = proposalFor(objs, host)
		return p != nil
	})
	hub.CLI(cast.OwnerKey, "sandbox", "approve", fork.Slug, s.ID, harness.Str(p["id"]), "--acknowledge-findings").Must(t)
	harness.Eventually(t, "the sandbox to reach "+target+" after the approval", func() bool { return curl(s, target) == 0 })
}

func TestSandbox_WorkspaceRuleAppliesToNewSandboxesOnly(t *testing.T) {
	harness.Require(t, harness.Gateway)
	t.Parallel()
	hub, cast := gatewayHub(t, "", "default")
	key := cast.OwnerKey
	target, host := harness.Page(t)
	sandboxOn := func(ws string) *harness.Sandbox {
		return hub.Sandbox(key, ws, map[string]any{"profile": "default"})
	}
	s1 := sandboxOn(hub.Fork(key, "demo", nil, nil).Slug)
	s0 := sandboxOn(hub.Fork(key, "demo", nil, nil).Slug)
	p := harness.Str(openProposal(t, hub, s1, target, host)["id"])

	hub.Call(http.MethodPost, s1.Path("/proposals/"+p+"/approve"), key,
		map[string]any{"scope": "workspace", "acknowledge_findings": true}).Want(t, http.StatusOK)
	var rule map[string]any
	rules, _ := hub.CLI(key, "sandbox", "rules", "demo").Must(t).JSON()["items"].([]any)
	for _, it := range rules {
		if m, _ := it.(map[string]any); names(m, host) {
			if rule != nil {
				t.Errorf("demo holds more than one rule for %s: %v", host, rules)
			}
			rule = m
		}
	}
	if rule == nil {
		t.Fatalf("demo's sandbox rules are %v, want one for %s", rules, host)
	}

	for what, s := range map[string]*harness.Sandbox{
		"a new sandbox on demo":       sandboxOn("demo"),
		"a new sandbox on a new fork": sandboxOn(hub.Fork(key, "demo", nil, nil).Slug),
	} {
		if code := curl(s, target); code != 0 {
			t.Errorf("%s did not reach %s under the workspace rule (curl exited %d)", what, target, code)
		}
		if items := hub.Get(key, s.Path("/proposals")).Items(); len(items) != 0 {
			t.Errorf("%s has the proposals %v, want none", what, items)
		}
	}
	if code := curl(s0, target); code == 0 {
		t.Errorf("a sandbox running before the approval reached %s: the rule applied to it", target)
	}

	hub.CLI(key, "sandbox", "rule-delete", "demo", harness.Str(rule["id"])).Must(t)
	s4 := sandboxOn("demo")
	if code := curl(s4, target); code == 0 {
		t.Errorf("a sandbox created after the rule was deleted reached %s", target)
	}
	harness.Eventually(t, "parley.sandbox.deny for the sandbox created after the delete", func() bool { return denied(t, hub, s4, host) })
}

func TestSandbox_CapacityRefusalNamesTheLimitHit(t *testing.T) {
	harness.Require(t, harness.Gateway)
	t.Parallel()
	limits := []string{"max_per_workspace", "max_sandboxes", "max_per_org", "max_per_user"}
	for _, limit := range limits {
		t.Run(limit, func(t *testing.T) {
			t.Parallel()
			hub, cast := gatewayHub(t, limit+" = 1", "default")
			key := cast.OwnerKey
			// The second request goes where only this limit is reached:
			// demo's family for the workspace limit, another workspace of
			// acme for the hub's and the organization's, and one in alice's
			// own organization for the user's.
			second := "demo"
			switch limit {
			case "max_sandboxes", "max_per_org":
				second = hub.Register(cast.Alice, "other", hub.Origin(), map[string]any{"org_id": "acme"}).Slug
			case "max_per_user":
				second = hub.Workspace(cast.Alice, "solo").Slug
			}
			first := hub.Sandbox(key, "demo", map[string]any{"profile": "default"})

			_, r := hub.TrySandbox(key, second, map[string]any{"profile": "default"})
			msg := harness.Str(harness.Field(r.Body, "error.message"))
			if r.Status != http.StatusTooManyRequests || r.ErrorType() != "sandbox_capacity" || !strings.Contains(msg, limit) {
				t.Errorf("the second sandbox answered %d %s, want 429 sandbox_capacity naming %s", r.Status, r.Raw, limit)
			}
			for _, other := range limits {
				if other != limit && strings.Contains(msg, other) {
					t.Errorf("the refusal %q names %s, want %s only", msg, other, limit)
				}
			}
			if all := hub.Get(hub.AdminToken, "/api/v1/sandboxes").Items(); len(all) != 1 {
				t.Errorf("the hub holds %d sandboxes after the refusal, want 1: %v", len(all), all)
			}

			hub.CLI(key, "sandbox", "delete", "demo", first.ID).Must(t)
			first.WaitState(30*time.Second, "deleted")
			if s, r := hub.TrySandbox(key, second, map[string]any{"profile": "default"}); s == nil {
				t.Errorf("the same request after the delete answered %d %s, want 201", r.Status, r.Raw)
			}
		})
	}
}

func TestSandbox_CheckpointsRestoreAndHubRestart(t *testing.T) {
	harness.Require(t, harness.Gateway)
	t.Parallel()
	// The hub's lifecycle pass runs every minute, so an ending under the
	// profile quick (idle timeout 1m, ttl 3m) is seen up to a minute late.
	endings := []struct {
		name, profile string
		end           func(t *testing.T, hub *harness.Hub, key string, s *harness.Sandbox)
		state, reason string
		limit         time.Duration
	}{
		{"stop", "default", func(t *testing.T, hub *harness.Hub, key string, s *harness.Sandbox) {
			hub.CLI(key, "sandbox", "stop", s.Workspace, s.ID).Must(t)
		}, "stopped", "", time.Minute},
		{"idle", "quick", nil, "stopped", "idle", 4 * time.Minute},
		// An exec every 20 s keeps the sandbox from going idle first.
		{"expiry", "quick", func(_ *testing.T, hub *harness.Hub, key string, s *harness.Sandbox) {
			go func() {
				for {
					r := hub.Call(http.MethodPost, s.Path("/exec"), key, map[string]any{"command": []string{"true"}})
					if r.Status != http.StatusOK {
						return
					}
					time.Sleep(20 * time.Second)
				}
			}()
		}, "deleted", "expired", 6 * time.Minute},
	}
	for _, e := range endings {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			hub, cast := gatewayHub(t, "", "default", "quick")
			key := cast.OwnerKey
			fork := hub.Fork(key, "demo", nil, nil)
			s := hub.Sandbox(key, fork.Slug, map[string]any{"profile": e.profile})
			uncommitted := "uncommitted in " + s.ID + "\n"
			commitIn(t, s, "echo committed > c.txt && git add c.txt && "+gitAs+" commit -q -m 'Add c.txt' && "+
				"printf '%s' '"+uncommitted+"' > u.txt")

			if e.end != nil {
				e.end(t, hub, key, s)
			}
			ended := s.WaitState(e.limit, e.state)
			if reason := harness.Str(ended["state_reason"]); e.reason != "" && reason != e.reason {
				t.Errorf("the sandbox ended %s with the reason %q, want %q", e.state, reason, e.reason)
			}

			refs := hub.Git("", "", "ls-remote", harness.AuthURL(fork.Remote, key), "refs/parley/sandboxes/"+s.ID+"/*").Must(t).Lines()
			if len(refs) == 0 {
				t.Errorf("the fork has no checkpoint ref under refs/parley/sandboxes/%s/", s.ID)
			}
			n := hub.Sandbox(key, fork.Slug, map[string]any{"profile": "default", "restore_from": s.ID})
			if log := n.Exec("git", "log", "--oneline", "--", "c.txt").Stdout; log == "" {
				t.Errorf("the restored sandbox's history has no commit of c.txt")
			}
			if got := n.Exec("cat", "u.txt").Stdout; got != uncommitted {
				t.Errorf("the restored sandbox's u.txt holds %q, want %q", got, uncommitted)
			}
		})
	}

	t.Run("restart", func(t *testing.T) {
		t.Parallel()
		hub, cast := gatewayHub(t, "", "default")
		fork := hub.Fork(cast.OwnerKey, "demo", nil, nil)
		r := hub.Sandbox(cast.OwnerKey, fork.Slug, map[string]any{
			"profile": "default", "command": []string{"sh", "-c", "while true; do date +%s >> /tmp/ticks; sleep 1; done"},
		})
		ticks := func() []string { return r.Exec("cat", "/tmp/ticks").Lines() }
		var before []string
		harness.Eventually(t, "the main process to append ticks", func() bool { before = ticks(); return len(before) >= 3 })

		hub.Stop()
		time.Sleep(5 * time.Second) // the sandbox runs on without its hub
		hub.Start()
		harness.EventuallyWithin(t, 60*time.Second, "the sandbox to be shown ready after the restart", func() bool {
			return hub.Call(http.MethodGet, r.Path(""), cast.OwnerKey, nil).Body["state"] == "ready"
		})

		after := ticks()
		if len(after) <= len(before) {
			t.Errorf("the main process appended no tick across the restart: %d before, %d after", len(before), len(after))
		}
		gap := 0
		for i := 1; i < len(after); i++ {
			a, errA := strconv.Atoi(after[i-1])
			b, errB := strconv.Atoi(after[i])
			if errA != nil || errB != nil {
				t.Fatalf("the ticks are not whole seconds: %q", after)
			}
			gap = max(gap, b-a)
		}
		if gap > 5 {
			t.Errorf("the main process's ticks have a gap of %d s, want at most 5: the hub's restart stopped it", gap)
		}
	})
}
