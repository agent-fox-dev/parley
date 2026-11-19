package sandbox_test

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestWorkspaceRuleProvider_RecordsTheHoldingWorkspacesRulesOnANewSandbox(t *testing.T) {
	t.Parallel()
	h := startPolicyWorld(t, "")
	h.repository(t, "held", workspace.StatusReady)
	h.fork(t, "held--a", "held", recordStart.Add(24*time.Hour), nil)
	h.repository(t, "empty", workspace.StatusReady)
	off := startCreateHub(t, "allow_workspace_rules = false\n"+agentProfile)
	off.repository(t, "held", workspace.StatusReady)
	off.AddMember(t, off.org, off.dana)
	// The older rule has the greater id: rules are ordered by when they were
	// saved, then by id.
	seed := func(hub *createHub) []sandbox.SandboxRule {
		return []sandbox.SandboxRule{
			hub.saveRule(t, "held", "sbr_"+strings.Repeat("f", 32), `{"host":"older.test"}`, recordStart.Add(-2*time.Hour)),
			hub.saveRule(t, "held", "sbr_"+strings.Repeat("0", 32), `{"host":"newer.test"}`, recordStart.Add(-time.Hour)),
		}
	}
	rules, offRules := seed(h), seed(off)

	for _, tc := range []struct {
		name string
		hub  *createHub
		slug string
		body map[string]any
		want []sandbox.SandboxRule
	}{
		{"on a repository", h, "held", map[string]any{}, rules},
		{"on a fork, the parent's", h, "held--a", map[string]any{}, rules},
		{"on a workspace without rules", h, "empty", map[string]any{}, nil},
		{"with another profile", h, "held", map[string]any{"profile": "plain"}, rules},
		{"with allow_workspace_rules off", off, "held", map[string]any{}, offRules},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := sandboxObject(t, tc.hub.post(t, tc.slug, tc.body))
			id := o["id"].(string)
			var ids, copies, wantCopies []string
			for _, r := range tc.want {
				ids = append(ids, r.ID)
				wantCopies = append(wantCopies, r.ID+" "+string(r.Rule))
			}
			want := nameList(ids...)
			if !reflect.DeepEqual(o["workspace_rules"], want) {
				t.Errorf("workspace_rules = %v; want %v", o["workspace_rules"], want)
			}
			for _, r := range storedRecord(t, tc.hub.Hub, id).WorkspaceRules {
				copies = append(copies, r.ID+" "+string(r.Body))
			}
			if !slices.Equal(copies, wantCopies) {
				t.Errorf("recorded copies %q; want %q", copies, wantCopies)
			}
			if ev := tc.hub.sandboxEvents(t, sandbox.EventCreate, id); len(ev) != 1 || !reflect.DeepEqual(ev[0].metadata["workspace_rules"], want) {
				t.Errorf("create events %+v; want one with workspace_rules %v", ev, want)
			}
		})
	}
}

func TestComposePolicy_ComposesTheBasePolicyTheProtectedRulesAndTheAddedRulesInOrder(t *testing.T) {
	t.Parallel()
	h := startPolicyWorld(t, "")
	ctx := context.Background()
	const ruleA, ruleB, ruleC = `{"host":"a.test","port":443}`, `{"host":"b.test","port":443}`, `{"host":"c.test","port":80}`
	withHub := func(id, slug string) []any { return []any{hubRule(id, slug), credentialRule(id, "model")} }
	for _, tc := range []struct {
		name, slug, profile, base string
		// rules are saved on the workspace before the create; approved
		// are approved for the sandbox once it is ready, and then it is
		// stopped and started again.
		rules, approved []string
		protected       func(id, slug string) []any
		added           func(r []sandbox.SandboxRule, a []sandbox.ApprovedRule) []runtime.Rule
	}{
		{name: "without workspace rules or approvals", slug: "p-none", profile: "full", base: policyContent, protected: withHub,
			added: func([]sandbox.SandboxRule, []sandbox.ApprovedRule) []runtime.Rule { return nil }},
		{name: "with two workspace rules", slug: "p-rules", profile: "full", base: policyContent, rules: []string{ruleA, ruleB}, protected: withHub,
			added: func(r []sandbox.SandboxRule, _ []sandbox.ApprovedRule) []runtime.Rule {
				return []runtime.Rule{copyOf(r[0]), copyOf(r[1])}
			}},
		{name: "with a workspace rule and an approval, at a start", slug: "p-start", profile: "full", base: policyContent,
			rules: []string{ruleA}, approved: []string{ruleC}, protected: withHub,
			added: func(r []sandbox.SandboxRule, a []sandbox.ApprovedRule) []runtime.Rule {
				return []runtime.Rule{copyOf(r[0]), approvedOf(a[0])}
			}},
		{name: "without hub access", slug: "p-nohub", profile: "nohub",
			protected: func(id, _ string) []any { return []any{credentialRule(id, "model")} },
			added:     func([]sandbox.SandboxRule, []sandbox.ApprovedRule) []runtime.Rule { return nil }},
		{name: "with an approval of a workspace rule the sandbox has", slug: "p-same", profile: "agent",
			rules: []string{ruleA}, approved: []string{`{ "port": 443, "host": "a.test" }`},
			protected: func(id, slug string) []any { return []any{hubRule(id, slug)} },
			added: func(r []sandbox.SandboxRule, _ []sandbox.ApprovedRule) []runtime.Rule {
				return []runtime.Rule{copyOf(r[0])}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.repository(t, tc.slug, workspace.StatusReady)
			var saved []sandbox.SandboxRule
			for i, body := range tc.rules {
				saved = append(saved, h.saveRule(t, tc.slug, "", body, recordStart.Add(time.Duration(i-10)*time.Second)))
			}
			id := h.readyIn(t, tc.slug, map[string]any{"profile": tc.profile})
			spec, _ := h.rt.Spec("parley", id)
			given := spec.Policy
			var approved []sandbox.ApprovedRule
			if tc.approved != nil {
				h.stopIn(t, id)
				for i, body := range tc.approved {
					approved = append(approved, h.saveApproved(t, id, body, recordStart.Add(time.Duration(i)*time.Second)))
				}
				if rec := h.startAgain(t, tc.slug, id); rec.State != sandbox.StateReady {
					t.Fatalf("started sandbox = %s %q; want ready", rec.State, rec.StateReason)
				}
				sp, err := h.rt.Client("parley", nil).GetPolicy(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				given = sp.Policy
			}
			rec := storedRecord(t, h.Hub, id)
			protected, err := h.svc.ProtectedRules(ctx, rec)
			if err != nil {
				t.Fatal(err)
			}
			wantAdded := decodedRules(t, tc.added(saved, approved))
			for what, p := range map[string]runtime.Policy{"the runtime's policy": given, "ComposePolicy": h.composed(t, id)} {
				if string(p.Base) != tc.base {
					t.Errorf("%s: base %q; want %q", what, p.Base, tc.base)
				}
				if got, want := decodedRules(t, p.Protected), tc.protected(id, tc.slug); !reflect.DeepEqual(got, want) || !reflect.DeepEqual(p.Protected, protected) {
					t.Errorf("%s: protected %#v; want %#v, as the hub supplies them", what, got, want)
				}
				if got := decodedRules(t, p.Added); !reflect.DeepEqual(got, wantAdded) {
					t.Errorf("%s: added %#v; want %#v", what, got, wantAdded)
				}
				for _, r := range p.Added {
					if r.Name == "parley-hub" || strings.HasPrefix(r.Name, "credential-") {
						t.Errorf("%s: protected rule %s among the added rules", what, r.Name)
					}
				}
			}
		})
	}
}

func TestComposePolicy_KeepsTheWorkspaceRulesRecordedAtCreationForTheSandboxsLife(t *testing.T) {
	t.Parallel()
	h := startPolicyWorld(t, "")
	h.repository(t, "keep", workspace.StatusReady)
	r1 := h.saveRule(t, "keep", "", `{"host":"deleted.test"}`, recordStart.Add(-time.Hour))
	s := h.readyIn(t, "keep", map[string]any{"profile": "agent"})
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := sandbox.DeleteRule(ctx, tx, "keep", r1.ID)
		return err
	})
	r2 := sandbox.SandboxRule{
		ID: sandbox.NewRuleID(), Workspace: "keep", Rule: json.RawMessage(`{"host":"promoted.test"}`), Summary: "promoted",
		FromProposal: &sandbox.RuleOrigin{SandboxID: sandbox.NewID(), ProposalID: "sbp_" + strings.Repeat("1", 32), Workspace: "keep--b"},
		CreatedBy:    sandbox.Creator{ActorType: "api_key", ActorID: h.dana, Username: "dana"}, CreatedAt: recordStart,
	}
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return sandbox.SaveRule(ctx, tx, r2) })

	if got := objectMap(t, storedRecord(t, h.Hub, s))["workspace_rules"]; !reflect.DeepEqual(got, nameList(r1.ID)) {
		t.Errorf("workspace_rules of the earlier sandbox = %v; want [%s]", got, r1.ID)
	}
	if got, want := decodedRules(t, h.composed(t, s).Added), decodedRules(t, []runtime.Rule{copyOf(r1)}); !reflect.DeepEqual(got, want) {
		t.Errorf("added rules of the earlier sandbox %#v; want %#v", got, want)
	}
	later := h.readyIn(t, "keep", map[string]any{"profile": "agent"})
	if got := objectMap(t, storedRecord(t, h.Hub, later))["workspace_rules"]; !reflect.DeepEqual(got, nameList(r2.ID)) {
		t.Errorf("workspace_rules of a later sandbox = %v; want [%s]", got, r2.ID)
	}
}

// A row steers the runtime for the time it runs, so none runs in parallel.
func TestComposePolicy_FailsAProfileWhoseBasePolicyRefersToTheHubCredential(t *testing.T) {
	t.Parallel()
	h := startPolicyWorld(t, "")
	h.repository(t, "base", workspace.StatusReady)
	for _, tc := range []struct {
		profile string
		refers  bool
	}{
		{"leaky", true},
		{"full", false},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			// The runtime reports the providers the base document refers
			// to, which are named after the sandbox: the hook learns the
			// id when the hub registers the sandbox's hub provider.
			h.hooks.set(func(_ context.Context, op, name string) error {
				if id, ok := strings.CutSuffix(name, "-hub"); ok && op == runtime.OpEnsureProvider {
					providers := []string{id + "-model"}
					if tc.refers {
						providers = append(providers, name)
					}
					h.rt.SetPolicyValidation([]byte(policyContent), runtime.PolicyValidation{Providers: providers})
				}
				return nil
			})
			t.Cleanup(func() {
				h.hooks.set(nil)
				h.rt.SetPolicyValidation([]byte(policyContent), runtime.PolicyValidation{})
			})
			_, rec := h.provisioned(t, "base", "", map[string]any{"profile": tc.profile})
			if !tc.refers {
				if rec.State != sandbox.StateReady {
					t.Fatalf("sandbox = %s %q; want ready", rec.State, rec.StateReason)
				}
				return
			}
			if text := h.provisionFailure(t, rec); !strings.Contains(text, "leaky") || !strings.Contains(text, "hub credential") {
				t.Errorf("error %q; want it to name the profile leaky and the hub credential", text)
			}
			for _, c := range h.rt.Calls() {
				if c.Op == runtime.OpCreateSandbox && c.Target == rec.ID {
					t.Fatal("the runtime received a create call")
				}
			}
		})
	}
}

// A row steers the runtime for the time it runs, so none runs in parallel.
func TestComposePolicy_NamesTheProfileOrTheRuleTheRuntimeRejects(t *testing.T) {
	t.Parallel()
	h := startPolicyWorld(t, "")
	h.repository(t, "names", workspace.StatusReady)
	var r []sandbox.SandboxRule
	for i, host := range []string{"one.test", "two.test", "three.test"} {
		r = append(r, h.saveRule(t, "names", "", `{"host":"`+host+`"}`, recordStart.Add(time.Duration(i-10)*time.Second)))
	}
	always := func(runtime.Policy) bool { return true }
	withBody := func(body string) func(runtime.Policy) bool {
		return func(p runtime.Policy) bool {
			return slices.ContainsFunc(p.Added, func(a runtime.Rule) bool { return string(a.Body) == body })
		}
	}
	const first, second = `{"host":"first-approved.test"}`, `{"host":"second-approved.test"}`
	for _, tc := range []struct {
		name, reason string
		reject       func(runtime.Policy) bool
		// approved are approved for a sandbox created ready, which is
		// then stopped and started again under the rejection; has and
		// hasNot may name them by their index as "approved-<i>".
		approved    []string
		has, hasNot []string
	}{
		{name: "the second rule", reason: "host not allowed", reject: withBody(string(r[1].Rule)),
			has: []string{r[1].ID}, hasNot: []string{r[0].ID, r[2].ID}},
		{name: "the base policy", reason: "base not allowed", reject: always, has: []string{`"agent"`}, hasNot: []string{r[0].ID, r[1].ID, r[2].ID}},
		{name: "with a long reason", reason: strings.Repeat("y", 5000), reject: always},
		{name: "the second rule approved for the sandbox", reason: "host not allowed", reject: withBody(second), approved: []string{first, second},
			has: []string{"approved-1"}, hasNot: []string{`"agent"`, "approved-0", r[0].ID, r[1].ID, r[2].ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { h.rt.RejectPolicies(nil, "") })
			var rec sandbox.Record
			names := map[string]string{}
			if tc.approved == nil {
				h.rt.RejectPolicies(tc.reject, tc.reason)
				_, rec = h.provisioned(t, "names", "", map[string]any{"profile": "agent"})
			} else {
				id := h.readyIn(t, "names", map[string]any{"profile": "agent"})
				h.stopIn(t, id)
				for i, body := range tc.approved {
					names[fmt.Sprintf("approved-%d", i)] = h.saveApproved(t, id, body, recordStart.Add(time.Duration(i)*time.Second)).ProposalID
				}
				h.rt.RejectPolicies(tc.reject, tc.reason)
				rec = h.startAgain(t, "names", id)
			}
			text := h.provisionFailure(t, rec)
			if len(text) > 1024 {
				t.Errorf("error of %d bytes; want at most 1024", len(text))
			}
			for _, s := range tc.has {
				if !strings.Contains(text, cmp.Or(names[s], s)) {
					t.Errorf("error %q; want it to hold %s", text, cmp.Or(names[s], s))
				}
			}
			for _, s := range tc.hasNot {
				if strings.Contains(text, cmp.Or(names[s], s)) {
					t.Errorf("error %q; want it not to hold %s", text, cmp.Or(names[s], s))
				}
			}
		})
	}
}

func TestComposePolicy_ComposesARestoredSandboxFromTheWorkspaceRulesAndNoneOfItsSourcesApprovals(t *testing.T) {
	t.Parallel()
	h := startPolicyWorld(t, "")
	h.svc.RegisterRestorer(&stubRestorer{hooks: h.hooks})
	src := h.seedIn(t, "demo--t1", json.RawMessage(`{"ok":true,"refs":[{"branch":"t1","sha":"`+h.base+`","dirty":false}]}`))
	own := h.saveApproved(t, src, `{"host":"source-only.test"}`, recordStart)
	r9 := h.saveRule(t, "demo", "", `{"host":"promoted.test"}`, recordStart)
	h.saveApproved(t, src, string(r9.Rule), recordStart.Add(time.Second))

	o, rec := h.provisioned(t, "demo--t1", "", map[string]any{"restore_from": src})
	if rec.State != sandbox.StateReady {
		t.Fatalf("sandbox = %s %q; want ready", rec.State, rec.StateReason)
	}
	if !reflect.DeepEqual(o["workspace_rules"], nameList(r9.ID)) {
		t.Errorf("workspace_rules = %v; want [%s]", o["workspace_rules"], r9.ID)
	}
	spec, _ := h.rt.Spec("parley", rec.ID)
	if got, want := decodedRules(t, spec.Policy.Added), decodedRules(t, []runtime.Rule{copyOf(r9)}); !reflect.DeepEqual(got, want) {
		t.Errorf("added rules %#v; want %#v", got, want)
	}
	for _, a := range spec.Policy.Added {
		if string(a.Body) == string(own.Rule) {
			t.Errorf("the policy holds the rule approved for the source alone: %s", a.Body)
		}
	}
}
