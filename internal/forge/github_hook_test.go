package forge_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/agent-fox-dev/parley/internal/forge"
)

// ghObject is a JSON object of the members given, for the members of a
// GitHub payload.
type ghObject = map[string]any

// everything holds every member a GitHub row requires, so that a payload
// with it lacks nothing a row could read.
var everything = ghObject{
	"issue":        ghObject{"number": 7, "pull_request": ghObject{"url": "https://x"}},
	"pull_request": ghObject{"number": 7, "merged": true},
	"label":        ghObject{"name": "bug"},
	"ref":          "refs/heads/main",
}

func TestMapGitHub_YieldsOneEventForAListedEventAndAction(t *testing.T) {
	t.Parallel()
	issue := ghObject{"issue": ghObject{"number": 7}}
	labeled := ghObject{"issue": ghObject{"number": 7}, "label": ghObject{"name": "bug"}}
	pull := func(merged any) ghObject {
		p := ghObject{"number": 7}
		if merged != nil {
			p["merged"] = merged
		}
		return ghObject{"pull_request": p}
	}
	cases := []struct {
		event, action string
		extra         ghObject
		want          forge.HookEvent
	}{
		{"issues", "opened", issue, forge.HookEvent{Type: "parley.forge.issue.opened", ResourceType: "issue", ResourceID: "7"}},
		{"issues", "edited", issue, forge.HookEvent{Type: "parley.forge.issue.edited", ResourceType: "issue", ResourceID: "7"}},
		{"issues", "closed", issue, forge.HookEvent{Type: "parley.forge.issue.closed", ResourceType: "issue", ResourceID: "7"}},
		{"issues", "reopened", issue, forge.HookEvent{Type: "parley.forge.issue.reopened", ResourceType: "issue", ResourceID: "7"}},
		{"issues", "labeled", labeled, forge.HookEvent{Type: "parley.forge.issue.labeled", ResourceType: "issue", ResourceID: "7",
			Metadata: map[string]any{"label": "bug"}}},
		{"issues", "unlabeled", labeled, forge.HookEvent{Type: "parley.forge.issue.unlabeled", ResourceType: "issue", ResourceID: "7",
			Metadata: map[string]any{"label": "bug"}}},
		{"issue_comment", "created", issue, forge.HookEvent{Type: "parley.forge.comment.created", ResourceType: "comment", ResourceID: "7",
			Metadata: map[string]any{"pull_request": false}}},
		{"issue_comment", "created", ghObject{"issue": ghObject{"number": 7, "pull_request": ghObject{"url": "https://x"}}},
			forge.HookEvent{Type: "parley.forge.comment.created", ResourceType: "comment", ResourceID: "7",
				Metadata: map[string]any{"pull_request": true}}},
		{"issue_comment", "created", ghObject{"issue": ghObject{"number": 7, "pull_request": "https://x"}},
			forge.HookEvent{Type: "parley.forge.comment.created", ResourceType: "comment", ResourceID: "7",
				Metadata: map[string]any{"pull_request": false}}},
		{"pull_request", "opened", pull(nil), forge.HookEvent{Type: "parley.forge.pull_request.opened", ResourceType: "pull_request", ResourceID: "7"}},
		{"pull_request", "reopened", pull(nil), forge.HookEvent{Type: "parley.forge.pull_request.reopened", ResourceType: "pull_request", ResourceID: "7"}},
		{"pull_request", "synchronize", pull(nil), forge.HookEvent{Type: "parley.forge.pull_request.synchronize", ResourceType: "pull_request", ResourceID: "7"}},
		{"pull_request", "closed", pull(true), forge.HookEvent{Type: "parley.forge.pull_request.merged", ResourceType: "pull_request", ResourceID: "7"}},
		{"pull_request", "closed", pull(false), forge.HookEvent{Type: "parley.forge.pull_request.closed", ResourceType: "pull_request", ResourceID: "7"}},
		{"pull_request", "closed", pull(nil), forge.HookEvent{Type: "parley.forge.pull_request.closed", ResourceType: "pull_request", ResourceID: "7"}},
		{"pull_request", "closed", pull("yes"), forge.HookEvent{Type: "parley.forge.pull_request.closed", ResourceType: "pull_request", ResourceID: "7"}},
		{"push", "", ghObject{"ref": "refs/heads/main"}, forge.HookEvent{Type: "parley.forge.push", ResourceType: "ref", ResourceID: "refs/heads/main"}},
	}
	for _, tc := range cases {
		action := any(tc.action)
		if tc.action == "" {
			action = nil
		}
		evs, err := forge.MapGitHub(tc.event, tc.action, ghPayload(action, "acme/widgets", tc.extra))
		if err != nil || len(evs) != 1 || !reflect.DeepEqual(evs[0], tc.want) {
			t.Errorf("%s %s %s: events %+v, error %v; want [%+v]", tc.event, tc.action, jsonOf(tc.extra), evs, err, tc.want)
		}
	}
}

func TestMapGitHub_ReportsARequiredFieldThatIsMissingOrOfTheWrongType(t *testing.T) {
	t.Parallel()
	label := ghObject{"name": "bug"}
	cases := []struct {
		event, action string
		extra         ghObject
	}{
		{"issues", "opened", nil},
		{"issues", "closed", ghObject{"issue": ghObject{"number": 0}}},
		{"issues", "edited", ghObject{"issue": ghObject{"number": 1.5}}},
		{"issues", "reopened", ghObject{"issue": ghObject{"number": "7"}}},
		{"issues", "labeled", ghObject{"issue": ghObject{"number": 7}}},
		{"issues", "labeled", ghObject{"issue": ghObject{"number": 7}, "label": ghObject{"name": ""}}},
		{"issues", "unlabeled", ghObject{"issue": ghObject{"number": 7}, "label": ghObject{"name": 3}}},
		{"issues", "labeled", ghObject{"label": label}},
		{"issue_comment", "created", ghObject{"issue": ghObject{"pull_request": ghObject{}}}},
		{"pull_request", "opened", ghObject{"issue": ghObject{"number": 7}}},
		{"pull_request", "closed", ghObject{"pull_request": ghObject{"merged": true}}},
		{"push", "", nil},
		{"push", "", ghObject{"ref": ""}},
		{"push", "", ghObject{"ref": 1}},
	}
	for _, tc := range cases {
		evs, err := forge.MapGitHub(tc.event, tc.action, ghPayload(tc.action, "acme/widgets", tc.extra))
		if !errors.Is(err, forge.ErrMissingField) || len(evs) != 0 {
			t.Errorf("%s %s %s: events %+v, error %v; want none and a missing field", tc.event, tc.action, jsonOf(tc.extra), evs, err)
		}
	}
}

func TestMapGitHub_HasNoMappingForAnUnlistedEventOrAction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		event  string
		action any
	}{
		{"ping", nil},
		{"issue_comment", "edited"},
		{"issue_comment", "deleted"},
		{"issues", "assigned"},
		{"issues", "deleted"},
		{"issues", "Opened"},
		{"issues", nil},
		{"Issues", "opened"},
		{"pull_request", "labeled"},
		{"pull_request", "edited"},
		{"pull_request_review", "submitted"},
		{"star", "created"},
		{"Push", nil},
	}
	for _, tc := range cases {
		action, _ := tc.action.(string)
		body := ghPayload(tc.action, "acme/widgets", everything)
		if evs, err := forge.MapGitHub(tc.event, action, body); !errors.Is(err, forge.ErrNoMapping) || len(evs) != 0 {
			t.Errorf("%s %v: events %+v, error %v; want no mapping", tc.event, tc.action, evs, err)
		}
		if red := forge.Reduce(forge.KindGitHub, tc.event, body, widgets); red.Outcome != forge.OutcomeIgnored || len(red.Events) != 0 {
			t.Errorf("Reduce %s %v: outcome %q with %d events; want ignored with none", tc.event, tc.action, red.Outcome, len(red.Events))
		}
	}
}
