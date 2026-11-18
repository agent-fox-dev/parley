package forge_test

import (
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/forge"
)

// widgets are the repositories of a carry-patch workspace whose origin is
// acme/widgets and whose upstream is up/widgets.
var widgets = forge.Repos{Origin: "acme/widgets", Upstream: "up/widgets"}

// ghPayload is a GitHub payload with the action and repository.full_name
// given, and the other members of extra; a nil value leaves its member out.
func ghPayload(action, fullName any, extra map[string]any) []byte {
	p := map[string]any{}
	if action != nil {
		p["action"] = action
	}
	if fullName != nil {
		p["repository"] = map[string]any{"full_name": fullName}
	}
	for k, v := range extra {
		p[k] = v
	}
	return []byte(jsonOf(p))
}

// glPayload is a GitLab payload with object_attributes.action and
// project.path_with_namespace given, and the other members of extra, whose
// object_attributes are added to the action; a nil value leaves its member
// out.
func glPayload(action, path any, extra map[string]any) []byte {
	attrs := map[string]any{}
	if action != nil {
		attrs["action"] = action
	}
	p := map[string]any{}
	for k, v := range extra {
		p[k] = v
	}
	if more, ok := extra["object_attributes"].(map[string]any); ok {
		for k, v := range more {
			attrs[k] = v
		}
	}
	p["object_attributes"] = attrs
	if path != nil {
		p["project"] = map[string]any{"path_with_namespace": path}
	}
	return []byte(jsonOf(p))
}

func targetName(t *forge.Target) string {
	if t == nil {
		return "null"
	}
	return string(*t)
}

func TestReduce_AnswersABodyThatIsNotOneJSONObjectAsMalformed(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"", "[]", `"x"`, "42", "null", "{", "{} {}", "{} x", "payload=%7B%7D"} {
		red := forge.Reduce(forge.KindGitHub, "issues", []byte(body), forge.Repos{Origin: "acme/widgets"})
		if red.Outcome != forge.OutcomeMalformed || red.Action != "" || red.Target != nil || len(red.Events) != 0 {
			t.Errorf("Reduce(%q) = outcome %q, action %q, target %s, %d events; want malformed, \"\", null, none",
				body, red.Outcome, red.Action, targetName(red.Target), len(red.Events))
		}
	}
}

func TestReduce_ReadsTheActionAndTheTargetOfAnyObject(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 100)
	cases := []struct {
		name   string
		kind   forge.ForgeKind
		event  string
		body   []byte
		repos  forge.Repos
		action string
		target string
	}{
		{"github origin", forge.KindGitHub, "issues", ghPayload("labeled", "acme/widgets", nil), widgets, "labeled", "origin"},
		{"github origin in another case", forge.KindGitHub, "issues", ghPayload("opened", "ACME/Widgets", nil), widgets, "opened", "origin"},
		{"github upstream", forge.KindGitHub, "issues", ghPayload("opened", "up/widgets", nil), widgets, "opened", "upstream"},
		{"github other repository", forge.KindGitHub, "issues", ghPayload("opened", "other/x", nil), widgets, "opened", "null"},
		{"github full_name missing", forge.KindGitHub, "issues", ghPayload("opened", nil, nil), widgets, "opened", "null"},
		{"github full_name not a string", forge.KindGitHub, "issues", ghPayload("opened", 7, nil), widgets, "opened", "null"},
		{"github repository not an object", forge.KindGitHub, "issues",
			[]byte(`{"action":"opened","repository":"acme/widgets"}`), widgets, "opened", "null"},
		{"gitlab upstream in another case", forge.KindGitLab, "Issue Hook", glPayload("open", "Up/Widgets", nil), widgets, "open", "upstream"},
		{"gitlab group path", forge.KindGitLab, "Issue Hook", glPayload("open", "group/sub/widgets", nil),
			forge.Repos{Origin: "group/sub/widgets"}, "open", "origin"},
		{"gitlab reads no top-level action", forge.KindGitLab, "Issue Hook",
			[]byte(`{"action":"open","project":{"path_with_namespace":"acme/widgets"}}`), widgets, "", "origin"},
		{"no upstream", forge.KindGitHub, "issues", ghPayload("opened", "up/widgets", nil),
			forge.Repos{Origin: "acme/widgets"}, "opened", "null"},
		{"origin equal to upstream", forge.KindGitHub, "issues", ghPayload("opened", "acme/widgets", nil),
			forge.Repos{Origin: "acme/widgets", Upstream: "acme/widgets"}, "opened", "origin"},
		{"action absent", forge.KindGitHub, "issues", ghPayload(nil, "acme/widgets", nil), widgets, "", "origin"},
		{"action not a string", forge.KindGitHub, "issues", ghPayload(5, "acme/widgets", nil), widgets, "", "origin"},
		{"long action", forge.KindGitHub, "issues", ghPayload(long, "acme/widgets", nil), widgets, long[:64], "origin"},
		{"long action cut before a character", forge.KindGitHub, "issues",
			ghPayload(strings.Repeat("a", 63)+"é", "acme/widgets", nil), widgets, strings.Repeat("a", 63), "origin"},
	}
	for _, tc := range cases {
		red := forge.Reduce(tc.kind, tc.event, tc.body, tc.repos)
		if red.Action != tc.action || targetName(red.Target) != tc.target {
			t.Errorf("%s: action %q, target %s; want %q, %s", tc.name, red.Action, targetName(red.Target), tc.action, tc.target)
		}
	}

	// An event the hub relays nothing of still has its target.
	red := forge.Reduce(forge.KindGitHub, "ping", ghPayload(nil, "acme/widgets", map[string]any{"zen": "Keep it simple."}), widgets)
	if red.Outcome != forge.OutcomeIgnored || targetName(red.Target) != "origin" || len(red.Events) != 0 {
		t.Errorf("ping: outcome %q, target %s, %d events; want ignored, origin, none", red.Outcome, targetName(red.Target), len(red.Events))
	}
}

func TestReduce_DecidesTheOutcomeByTheFirstMatchingRow(t *testing.T) {
	t.Parallel()
	issue := func(number any) map[string]any { return map[string]any{"issue": map[string]any{"number": number}} }
	cases := []struct {
		name    string
		event   string
		body    []byte
		outcome forge.Outcome
		events  []forge.HookEvent
	}{
		{"no mapping", "ping", ghPayload(nil, nil, nil), forge.OutcomeIgnored, nil},
		{"no mapping for the action", "issues", ghPayload("assigned", nil, issue(7)), forge.OutcomeIgnored, nil},
		{"required number missing", "issues", ghPayload("opened", "other/x", nil), forge.OutcomeMalformed, nil},
		{"required number a string", "issues", ghPayload("opened", "acme/widgets", issue("7")), forge.OutcomeMalformed, nil},
		{"required number 0", "issues", ghPayload("opened", "acme/widgets", issue(0)), forge.OutcomeMalformed, nil},
		{"required number not an integer", "issues", ghPayload("opened", "acme/widgets", issue(1.5)), forge.OutcomeMalformed, nil},
		{"required string empty", "push", ghPayload(nil, "acme/widgets", map[string]any{"ref": ""}), forge.OutcomeMalformed, nil},
		{"required string not a string", "push", ghPayload(nil, "acme/widgets", map[string]any{"ref": 7}), forge.OutcomeMalformed, nil},
		{"events without a repository", "issues", ghPayload("opened", nil, issue(7)), forge.OutcomeMalformed, nil},
		{"events of another repository", "issues", ghPayload("opened", "other/x", issue(7)), forge.OutcomeRepositoryMismatch, nil},
		{"relayed", "issues", ghPayload("opened", "acme/widgets", issue(7)), forge.OutcomeRelayed,
			[]forge.HookEvent{{Type: "parley.forge.issue.opened", ResourceType: "issue", ResourceID: "7"}}},
		{"relayed for the upstream", "push", ghPayload(nil, "up/widgets", map[string]any{"ref": "refs/heads/main"}), forge.OutcomeRelayed,
			[]forge.HookEvent{{Type: "parley.forge.push", ResourceType: "ref", ResourceID: "refs/heads/main"}}},
	}
	for _, tc := range cases {
		red := forge.Reduce(forge.KindGitHub, tc.event, tc.body, widgets)
		if red.Outcome != tc.outcome || len(red.Events) != len(tc.events) {
			t.Errorf("%s: outcome %q with %d events; want %q with %d", tc.name, red.Outcome, len(red.Events), tc.outcome, len(tc.events))
			continue
		}
		for i, e := range red.Events {
			if e.Type != tc.events[i].Type || e.ResourceType != tc.events[i].ResourceType || e.ResourceID != tc.events[i].ResourceID {
				t.Errorf("%s: event %d = %+v, want %+v", tc.name, i, e, tc.events[i])
			}
		}
	}

	// A merge request update without a new revision relays nothing, which
	// is ignored even of no repository.
	update := map[string]any{"object_attributes": map[string]any{"iid": 9}}
	red := forge.Reduce(forge.KindGitLab, "Merge Request Hook", glPayload("update", nil, update), widgets)
	if red.Outcome != forge.OutcomeIgnored || len(red.Events) != 0 {
		t.Errorf("no events: outcome %q with %d events; want ignored with none", red.Outcome, len(red.Events))
	}
}
