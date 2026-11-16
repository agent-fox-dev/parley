package land_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/land"
)

// overridePolicy registers the landing policy's gate on the world again
// and sets api's policy: a file under gated/ needs one approval and the
// check test, one under deploy/secrets/ is blocked, and any other file
// needs nothing, so that a change is pending, blocked or ready by the
// files it sets.
func (w *world) overridePolicy(t *testing.T) {
	t.Helper()
	w.land.RegisterGate(w.policyGate)
	policy := map[string]any{"default": map[string]any{"approvals": 0}, "rules": []any{
		map[string]any{"name": "secrets", "paths": []string{"deploy/secrets/**"}, "require": map[string]any{"block": true}},
		map[string]any{"name": "gated", "paths": []string{"gated/**"}, "require": map[string]any{"approvals": 1, "checks": []string{"test"}}},
	}}
	if status, raw := w.Call(t, http.MethodPut, "/api/v1/workspaces/api/policy", w.ownerKey, policy); status != http.StatusOK {
		t.Fatalf("PUT api's policy = %d %s, want 200", status, raw)
	}
}

// openIn opens, through the change component, a change of api by the
// caller by from a new branch of base in the workspace slug, whose
// repository is at dir, that sets the files, and returns it.
func (w *world) openIn(t *testing.T, slug, dir, branch string, files map[string]string, by change.Caller) change.Change {
	t.Helper()
	commit(t, dir, branch, w.base, branch, files)
	return w.opened(t, change.OpenInput{Workspace: "api", SourceWorkspace: slug, SourceBranch: branch, Intent: new("why"),
		Kind: change.KindRef, Caller: by})
}

// withOverride is a land body with the override reason, and the members
// of more.
func withOverride(reason string, more map[string]any) map[string]any {
	body := map[string]any{"override": map[string]any{"reason": reason}}
	for k, v := range more {
		body[k] = v
	}
	return body
}

func TestLand_OverrideLetsTheOwnerLandPastPendingApprovalsAndChecks(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.overridePolicy(t)
	n := 0
	for _, caller := range []struct{ name, credential string }{
		{"the owner's API key", w.ownerKey},
		{"the owner's unbound PAT", w.docsPAT(t, nil)},
		{"the admin token", w.Token},
	} {
		for _, shape := range []struct {
			name  string
			stack bool
		}{{"a single change", false}, {"a stack of two pending changes", true}} {
			t.Run(caller.name+", "+shape.name, func(t *testing.T) {
				n++
				prefix := "o" + strconv.Itoa(n) + "-"
				var covered []change.Change
				var more map[string]any
				if shape.stack {
					covered = w.stack(t, prefix, false, map[string]string{"gated/" + prefix + "1": "1\n"},
						map[string]string{"gated/" + prefix + "2": "2\n"})
					more = map[string]any{"stack": true}
				} else {
					covered = []change.Change{w.openSetting(t, prefix, map[string]string{"gated/" + prefix: "x\n"})}
				}
				top := covered[len(covered)-1]

				status, a := w.landCall(t, caller.credential, top.Number, "", withOverride("hotfix", more))

				want := map[string]any{"reason": "hotfix"}
				if status != http.StatusOK || a["status"] != land.StatusLanded || !reflect.DeepEqual(a["override"], want) {
					t.Fatalf("land = %d %v, want 200 landed with override %v", status, a, want)
				}
				lands := w.landEvents(t, land.EventLand, a["id"].(string))
				if len(lands) != len(covered) {
					t.Fatalf("%d parley.change.land events, want %d", len(lands), len(covered))
				}
				for _, e := range lands {
					if got := metadata(t, e)["override"]; !reflect.DeepEqual(got, want) {
						t.Errorf("event %d carries override %v, want %v", e.Seq, got, want)
					}
				}
			})
		}
	}
}

func TestLand_OverrideNeverLandsABlockedChange(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.overridePolicy(t)
	t.Run("a change touching a blocked path", func(t *testing.T) {
		ch := w.openSetting(t, "secret", map[string]string{"deploy/secrets/k": "k\n"})
		main := head(t, w.api, "main")

		status, answer := w.landCall(t, w.ownerKey, ch.Number, "", withOverride("hotfix", nil))

		errObj, _ := answer["error"].(map[string]any)
		details, _ := errObj["details"].(map[string]any)
		blocked, _ := details["blocked"].(map[string]any)
		override, recorded := details["override"]
		if status != http.StatusConflict || errorType(answer) != "policy_blocked" || details["status"] != land.StatusBlocked ||
			!recorded || override != nil || blocked["change"] != float64(ch.Number) {
			t.Errorf("land = %d %v, want 409 policy_blocked with a blocked attempt naming %s and no override", status, answer, ch.Ref)
		}
		if got := head(t, w.api, "main"); got != main {
			t.Errorf("main moved from %s to %s", main, got)
		}
	})
	t.Run("a draft change", func(t *testing.T) {
		ch := w.openDraft(t, "draft")
		main := head(t, w.api, "main")

		status, answer := w.landCall(t, w.ownerKey, ch.Number, "", withOverride("hotfix", nil))

		if status != http.StatusConflict || answer["status"] == land.StatusLanded {
			t.Errorf("land = %d %v, want 409 and the draft not landed", status, answer)
		}
		if got := head(t, w.api, "main"); got != main {
			t.Errorf("main moved from %s to %s", main, got)
		}
		if st := w.readChange(t, ch.Number).State; st != change.StateDraft {
			t.Errorf("%s is %s, want draft", ch.Ref, st)
		}
	})
}

func TestLand_RefusesAnOverrideFromACallerWhoMayNotOverride(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.overridePolicy(t)
	authorID, authorKey := w.member(t, "member-a")
	_, memberKey := w.member(t, "member-b")
	authorDir, ownerFork := w.fork(t, "api--a", authorID), w.fork(t, "api--f", w.owner)
	author := change.Caller{UserID: authorID, Username: "member-a", Credential: change.Credential{Type: change.CredentialAPIKey, ID: "ka"}}
	write := []string{"changes:read", "changes:write"}
	boundToAPI := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: write, Workspace: "api"})
	boundToFork := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: write, Workspace: "api--f"})
	n := 0
	for _, c := range []struct {
		name, credential string
		// slug and dir are the workspace the change is opened from and its
		// repository, by the caller by.
		slug, dir string
		by        change.Caller
		// settings are api's settings besides changes.auto_restack.
		settings  string
		errorType string
	}{
		{name: "the owner's PAT bound to the workspace", credential: boundToAPI, slug: "api", dir: w.api, by: w.caller(),
			errorType: "override_not_allowed"},
		{name: "the owner's PAT bound to a fork, on a change from it", credential: boundToFork, slug: "api--f", dir: ownerFork,
			by: w.caller(), errorType: "override_not_allowed"},
		// A member of the org reads the workspace and does not write it;
		// only its owner does, and who may land refuses them first.
		{name: "a member of the org with an API key", credential: memberKey, slug: "api", dir: w.api, by: w.caller(),
			errorType: "forbidden"},
		{name: "the change's author landing by land.by_author", credential: authorKey, slug: "api--a", dir: authorDir, by: author,
			settings: `"land.by_author":true`, errorType: "override_not_allowed"},
		{name: "the owner's API key with land.allow_override false", credential: w.ownerKey, slug: "api", dir: w.api, by: w.caller(),
			settings: `"land.allow_override":false`, errorType: "override_not_allowed"},
		{name: "the admin token with land.allow_override false", credential: w.Token, slug: "api", dir: w.api, by: w.caller(),
			settings: `"land.allow_override":false`, errorType: "override_not_allowed"},
	} {
		for _, policy := range []struct {
			name string
			dir  string
		}{{"pending", "gated/"}, {"ready", "free/"}} {
			n++
			branch := "r" + strconv.Itoa(n) + "-" + policy.name
			t.Run(c.name+", "+policy.name, func(t *testing.T) {
				settings := `{"changes.auto_restack":false`
				if c.settings != "" {
					settings += "," + c.settings
				}
				w.configure(t, settings+"}")
				ch := w.openIn(t, c.slug, c.dir, branch, map[string]string{policy.dir + branch: "x\n"}, c.by)
				attempts, main := w.attempts(t), head(t, w.api, "main")

				for _, r := range []struct {
					reason, errorType string
					status            int
				}{{"", "invalid_request", http.StatusBadRequest}, {"x", c.errorType, http.StatusForbidden}} {
					status, raw := w.post(t, c.credential, "api", ch.Number, "", withOverride(r.reason, nil))

					var answer map[string]any
					_ = json.Unmarshal(raw, &answer)
					if status != r.status || errorType(answer) != r.errorType {
						t.Errorf("land with override reason %q = %d %s, want %d %s", r.reason, status, raw, r.status, r.errorType)
					}
				}
				if n := w.attempts(t); n != attempts {
					t.Errorf("%d land attempts are recorded, want %d", n, attempts)
				}
				if got := head(t, w.api, "main"); got != main {
					t.Errorf("main moved from %s to %s", main, got)
				}
			})
		}
	}
}

func TestLand_IgnoresAnOverrideWhenEveryChangeIsReady(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.land.RegisterGate(w.policyGate)
	ch := w.open(t, "ready")

	status, a := w.landCall(t, w.ownerKey, ch.Number, "", withOverride("x", nil))

	if override, ok := a["override"]; status != http.StatusOK || a["status"] != land.StatusLanded || !ok || override != nil {
		t.Fatalf("land = %d %v, want 200 landed with override null", status, a)
	}
	lands := w.landEvents(t, land.EventLand, a["id"].(string))
	if len(lands) != 1 {
		t.Fatalf("%d parley.change.land events, want 1", len(lands))
	}
	if override, ok := metadata(t, lands[0])["override"]; !ok || override != nil {
		t.Errorf("the event carries override %v (present %t), want null", override, ok)
	}
}
