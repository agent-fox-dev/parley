package forge_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/forge"
)

// glObject is a JSON object of the members given, for the members of a
// GitLab payload.
type glObject = map[string]any

// attrs is a payload whose object_attributes are the members given.
func attrs(members glObject) glObject { return glObject{"object_attributes": members} }

// labelChanges is an Issue Hook's changes.labels with the titles given.
func labelChanges(previous, current []string) glObject {
	list := func(titles []string) []any {
		out := make([]any, 0, len(titles))
		for _, t := range titles {
			out = append(out, glObject{"title": t})
		}
		return out
	}
	return glObject{"labels": glObject{"previous": list(previous), "current": list(current)}}
}

// numberedTitles are the titles of n labels, the prefix and a number
// padded to the width of n-1, so that their byte order is their order.
func numberedTitles(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%0*d", prefix, len(fmt.Sprint(n-1)), i)
	}
	return out
}

// described names each event by the last segment of its type, its
// resource id and its label, if any.
func described(evs []forge.HookEvent) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, fmt.Sprintf("%s(%s %v)", e.Type[strings.LastIndex(e.Type, ".")+1:], e.ResourceID, e.Metadata["label"]))
	}
	return out
}

func TestMapGitLab_YieldsOneEventForAListedHookAndAction(t *testing.T) {
	t.Parallel()
	iid := func(n any) glObject { return attrs(glObject{"iid": n}) }
	note := func(kind, on string, n any) glObject {
		return glObject{"object_attributes": glObject{"noteable_type": kind}, on: glObject{"iid": n}}
	}
	ev := func(typ, resourceType, id string, metadata map[string]any) forge.HookEvent {
		return forge.HookEvent{Type: typ, ResourceType: resourceType, ResourceID: id, Metadata: metadata}
	}
	cases := []struct {
		event, action string
		extra         glObject
		want          forge.HookEvent
	}{
		{"Issue Hook", "open", iid(12), ev("parley.forge.issue.opened", "issue", "12", nil)},
		{"Issue Hook", "close", iid(12), ev("parley.forge.issue.closed", "issue", "12", nil)},
		{"Issue Hook", "reopen", iid(12), ev("parley.forge.issue.reopened", "issue", "12", nil)},
		{"Note Hook", "", note("Issue", "issue", 5), ev("parley.forge.comment.created", "comment", "5",
			map[string]any{"pull_request": false})},
		{"Note Hook", "create", note("MergeRequest", "merge_request", 9), ev("parley.forge.comment.created", "comment", "9",
			map[string]any{"pull_request": true})},
		{"Merge Request Hook", "open", iid(9), ev("parley.forge.pull_request.opened", "pull_request", "9", nil)},
		{"Merge Request Hook", "close", iid(9), ev("parley.forge.pull_request.closed", "pull_request", "9", nil)},
		{"Merge Request Hook", "reopen", iid(9), ev("parley.forge.pull_request.reopened", "pull_request", "9", nil)},
		{"Merge Request Hook", "merge", iid(9), ev("parley.forge.pull_request.merged", "pull_request", "9", nil)},
		{"Merge Request Hook", "update", attrs(glObject{"iid": 9, "oldrev": "abc"}),
			ev("parley.forge.pull_request.synchronize", "pull_request", "9", nil)},
		{"Push Hook", "", glObject{"ref": "refs/heads/main"}, ev("parley.forge.push", "ref", "refs/heads/main", nil)},
		{"Tag Push Hook", "", glObject{"ref": "refs/tags/v1"}, ev("parley.forge.push", "ref", "refs/tags/v1", nil)},
	}
	for _, tc := range cases {
		evs, err := forge.MapGitLab(tc.event, tc.action, glPayload(tc.action, "acme/widgets", tc.extra))
		if err != nil || len(evs) != 1 || !reflect.DeepEqual(evs[0], tc.want) {
			t.Errorf("%s %s %s: events %+v, error %v; want [%+v]", tc.event, tc.action, jsonOf(tc.extra), evs, err, tc.want)
		}
	}
}

func TestMapGitLab_RelaysAnIssueUpdateAsEditedThenUnlabeledThenLabeled(t *testing.T) {
	t.Parallel()
	update := func(changes glObject) glObject {
		return glObject{"object_attributes": glObject{"iid": 12}, "changes": changes}
	}
	edited := forge.HookEvent{Type: "parley.forge.issue.edited", ResourceType: "issue", ResourceID: "12"}
	labels := func(kind string, titles ...string) []forge.HookEvent {
		out := make([]forge.HookEvent, 0, len(titles))
		for _, title := range titles {
			out = append(out, forge.HookEvent{Type: "parley.forge.issue." + kind, ResourceType: "issue", ResourceID: "12",
				Metadata: map[string]any{"label": title}})
		}
		return out
	}
	with := func(changes, more glObject) glObject {
		for k, v := range more {
			changes[k] = v
		}
		return changes
	}
	cases := []struct {
		name    string
		changes glObject
		want    []forge.HookEvent
	}{
		{"title and labels", with(labelChanges([]string{"b", "a"}, []string{"a", "d", "c"}), glObject{"title": glObject{"previous": "x", "current": "y"}}),
			append(append([]forge.HookEvent{edited}, labels("unlabeled", "b")...), labels("labeled", "c", "d")...)},
		{"description only", glObject{"description": glObject{"previous": "x", "current": "y"}}, []forge.HookEvent{edited}},
		{"titles ordered by their bytes", labelChanges([]string{"b", "B", "é", "a"}, []string{"z", "Z"}),
			append(labels("unlabeled", "B", "a", "b", "é"), labels("labeled", "Z", "z")...)},
		{"80 removed and 80 added", labelChanges(numberedTitles("p", 80), numberedTitles("c", 80)),
			append(labels("unlabeled", numberedTitles("p", 80)...), labels("labeled", numberedTitles("c", 80)[:20]...)...)},
		{"title and 150 added", with(labelChanges(nil, numberedTitles("c", 150)), glObject{"title": glObject{}}),
			append([]forge.HookEvent{edited}, labels("labeled", numberedTitles("c", 150)[:100]...)...)},
	}
	for _, tc := range cases {
		evs, err := forge.MapGitLab("Issue Hook", "update", glPayload("update", "acme/widgets", update(tc.changes)))
		if err != nil || !reflect.DeepEqual(evs, tc.want) {
			t.Errorf("%s: events %v, error %v; want %v", tc.name, described(evs), err, described(tc.want))
		}
	}
}

func TestMapGitLab_YieldsNoEventsForAnUpdateWithoutARelayedChange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		event string
		extra glObject
	}{
		{"Issue Hook", glObject{"object_attributes": glObject{"iid": 12}, "changes": labelChanges([]string{"a"}, []string{"a"})}},
		{"Issue Hook", glObject{"object_attributes": glObject{"iid": 12}, "changes": glObject{}}},
		{"Issue Hook", glObject{"object_attributes": glObject{"iid": 12}, "changes": glObject{"updated_at": glObject{"current": "2026-11-18"}}}},
		{"Merge Request Hook", attrs(glObject{"iid": 9})},
		{"Merge Request Hook", attrs(glObject{"iid": 9, "oldrev": ""})},
	}
	for _, tc := range cases {
		body := glPayload("update", "acme/widgets", tc.extra)
		if evs, err := forge.MapGitLab(tc.event, "update", body); err != nil || len(evs) != 0 {
			t.Errorf("%s update %s: events %+v, error %v; want none", tc.event, jsonOf(tc.extra), evs, err)
		}
		if red := forge.Reduce(forge.KindGitLab, tc.event, body, widgets); red.Outcome != forge.OutcomeIgnored || len(red.Events) != 0 {
			t.Errorf("Reduce %s update %s: outcome %q with %d events; want ignored with none", tc.event, jsonOf(tc.extra), red.Outcome, len(red.Events))
		}
	}
}

func TestMapGitLab_ReportsARequiredFieldThatIsMissingOrOfTheWrongType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		event, action string
		extra         glObject
	}{
		{"Issue Hook", "open", nil},
		{"Issue Hook", "close", attrs(glObject{"iid": "12"})},
		{"Issue Hook", "reopen", attrs(glObject{"iid": 0})},
		{"Issue Hook", "update", glObject{"changes": glObject{"title": glObject{}}}},
		{"Note Hook", "", glObject{"object_attributes": glObject{"noteable_type": "Issue"}}},
		{"Note Hook", "", glObject{"object_attributes": glObject{"noteable_type": "Issue"}, "merge_request": glObject{"iid": 9}}},
		{"Note Hook", "", glObject{"object_attributes": glObject{"noteable_type": "MergeRequest"}, "merge_request": glObject{"iid": "9"}}},
		{"Merge Request Hook", "open", nil},
		{"Merge Request Hook", "merge", attrs(glObject{"iid": 1.5})},
		{"Merge Request Hook", "update", attrs(glObject{"oldrev": "abc"})},
		{"Push Hook", "", nil},
		{"Push Hook", "", glObject{"ref": ""}},
		{"Tag Push Hook", "", glObject{"ref": 1}},
	}
	for _, tc := range cases {
		evs, err := forge.MapGitLab(tc.event, tc.action, glPayload(tc.action, "acme/widgets", tc.extra))
		if !errors.Is(err, forge.ErrMissingField) || len(evs) != 0 {
			t.Errorf("%s %s %s: events %+v, error %v; want none and a missing field", tc.event, tc.action, jsonOf(tc.extra), evs, err)
		}
	}
}

func TestMapGitLab_HasNoMappingForAnUnlistedHookOrAction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		event         string
		action, notes any
	}{
		{"Issue Hook", nil, nil},
		{"Issue Hook", "Open", nil},
		{"Issue Hook", "opened", nil},
		{"issue hook", "open", nil},
		{"Merge Request Hook", "approved", nil},
		{"Merge Request Hook", "unapproved", nil},
		{"Note Hook", nil, "Commit"},
		{"Note Hook", "create", "Snippet"},
		{"Note Hook", nil, "issue"},
		{"Note Hook", nil, nil},
		{"Note Hook", nil, 1},
		{"Pipeline Hook", nil, nil},
		{"Job Hook", nil, nil},
		{"Wiki Page Hook", "create", nil},
		{"push hook", nil, nil},
	}
	for _, tc := range cases {
		action, _ := tc.action.(string)
		oa := glObject{"iid": 9, "oldrev": "abc"}
		if tc.notes != nil {
			oa["noteable_type"] = tc.notes
		}
		body := glPayload(tc.action, "acme/widgets", glObject{"object_attributes": oa,
			"issue": glObject{"iid": 5}, "merge_request": glObject{"iid": 9}, "ref": "refs/heads/main"})
		if evs, err := forge.MapGitLab(tc.event, action, body); !errors.Is(err, forge.ErrNoMapping) || len(evs) != 0 {
			t.Errorf("%s %v %v: events %+v, error %v; want no mapping", tc.event, tc.action, tc.notes, evs, err)
		}
		if red := forge.Reduce(forge.KindGitLab, tc.event, body, widgets); red.Outcome != forge.OutcomeIgnored || len(red.Events) != 0 {
			t.Errorf("Reduce %s %v %v: outcome %q with %d events; want ignored with none", tc.event, tc.action, tc.notes, red.Outcome, len(red.Events))
		}
	}
}
