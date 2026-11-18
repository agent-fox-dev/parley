package forge_test

import (
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// sameDraft reports whether got is the event want names: the same type and
// resource, and every metadata key of want with its value. got may hold
// more keys; the description tests check those.
func sameDraft(got, want forge.HookEvent) bool {
	if got.Type != want.Type || got.ResourceType != want.ResourceType || got.ResourceID != want.ResourceID {
		return false
	}
	for k, v := range want.Metadata {
		if !reflect.DeepEqual(got.Metadata[k], v) {
			return false
		}
	}
	return true
}

// sameDrafts reports whether got are the events want names, in order.
func sameDrafts(got, want []forge.HookEvent) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !sameDraft(got[i], want[i]) {
			return false
		}
	}
	return true
}

// hookCase is a delivery for acme/widgets that the hub relays, and what its
// first event says.
type hookCase struct {
	name   string
	kind   forge.ForgeKind
	event  string
	action any // nil leaves the action out
	extra  map[string]any
	typ    string // the first event's type; "" not checked
	id     string // the first event's resource id; "" not checked
	want   map[string]any
}

// relayed reduces the case's delivery and is its events; the delivery
// must be relayed, whatever optional value it lacks.
func (c hookCase) relayed(t *testing.T) []forge.HookEvent {
	t.Helper()
	body := ghPayload(c.action, "acme/widgets", c.extra)
	if c.kind == forge.KindGitLab {
		body = glPayload(c.action, "acme/widgets", c.extra)
	}
	red := forge.Reduce(c.kind, c.event, body, widgets)
	if red.Outcome != forge.OutcomeRelayed || len(red.Events) == 0 {
		t.Errorf("%s: outcome %q with %d events; want relayed", c.name, red.Outcome, len(red.Events))
	}
	return red.Events
}

// checkMetadata asserts that the first event of each case has exactly the
// metadata the case wants.
func checkMetadata(t *testing.T, cases []hookCase) {
	t.Helper()
	for _, tc := range cases {
		evs := tc.relayed(t)
		if len(evs) == 0 {
			continue
		}
		ev := evs[0]
		if !reflect.DeepEqual(ev.Metadata, tc.want) {
			t.Errorf("%s: metadata %s; want %s", tc.name, jsonOf(ev.Metadata), jsonOf(tc.want))
		}
		if tc.typ != "" && ev.Type != tc.typ {
			t.Errorf("%s: type %q; want %q", tc.name, ev.Type, tc.typ)
		}
		if tc.id != "" && ev.ResourceID != tc.id {
			t.Errorf("%s: resource id %q; want %q", tc.name, ev.ResourceID, tc.id)
		}
	}
}

// labelList is a label list of entries with the key and the names given.
func labelList(key string, list ...string) []any {
	out := make([]any, 0, len(list))
	for _, n := range list {
		out = append(out, map[string]any{key: n})
	}
	return out
}

// overlay is a copy of base with the members of more added or replaced.
func overlay(base, more map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(more))
	maps.Copy(out, base)
	maps.Copy(out, more)
	return out
}

const issueURL = "https://forge.example/acme/widgets/issues/7"

func TestHookEvent_DescribesAnIssueByItsNumberTitleURLAndLabels(t *testing.T) {
	t.Parallel()
	ghIssue := func(members map[string]any) map[string]any {
		return map[string]any{"issue": overlay(map[string]any{"number": 7}, members)}
	}
	full := ghIssue(map[string]any{"title": "T", "html_url": issueURL, "labels": labelList("name", "bug", "p1")})
	labeled := overlay(full, map[string]any{"label": map[string]any{"name": "bug"}})
	glIssue := func(more map[string]any) map[string]any {
		return overlay(map[string]any{"object_attributes": map[string]any{"iid": 12, "title": "T", "url": issueURL},
			"labels": labelList("title", "a", "c")}, more)
	}
	many := make([]string, 150)
	for i := range many {
		many[i] = fmt.Sprintf("l%03d", i)
	}
	cases := []hookCase{
		{name: "github opened", kind: forge.KindGitHub, event: "issues", action: "opened", extra: full,
			want: map[string]any{"number": int64(7), "title": "T", "url": issueURL, "labels": []string{"bug", "p1"}}},
		{name: "github closed", kind: forge.KindGitHub, event: "issues", action: "closed", extra: full,
			want: map[string]any{"number": int64(7), "title": "T", "url": issueURL, "labels": []string{"bug", "p1"}}},
		{name: "github labeled", kind: forge.KindGitHub, event: "issues", action: "labeled", extra: labeled,
			want: map[string]any{"number": int64(7), "label": "bug", "url": issueURL, "labels": []string{"bug", "p1"}}},
		{name: "gitlab open", kind: forge.KindGitLab, event: "Issue Hook", action: "open", extra: glIssue(nil),
			want: map[string]any{"number": int64(12), "title": "T", "url": issueURL, "labels": []string{"a", "c"}}},
		{name: "gitlab update edited", kind: forge.KindGitLab, event: "Issue Hook", action: "update",
			extra: glIssue(map[string]any{"changes": map[string]any{"title": map[string]any{}}}),
			want:  map[string]any{"number": int64(12), "title": "T", "url": issueURL, "labels": []string{"a", "c"}}},
		{name: "gitlab update labeled", kind: forge.KindGitLab, event: "Issue Hook", action: "update",
			extra: glIssue(map[string]any{"changes": labelChanges([]string{"a"}, []string{"a", "c"})}), typ: "parley.forge.issue.labeled",
			want: map[string]any{"number": int64(12), "label": "c", "url": issueURL, "labels": []string{"a", "c"}}},
		{name: "title cut at a character boundary", kind: forge.KindGitHub, event: "issues", action: "opened",
			extra: ghIssue(map[string]any{"title": "a" + strings.Repeat("é", 300)}),
			want:  map[string]any{"number": int64(7), "title": "a" + strings.Repeat("é", 127), "url": "", "labels": []string{}}},
		{name: "the first 100 of 150 labels", kind: forge.KindGitHub, event: "issues", action: "reopened",
			extra: ghIssue(map[string]any{"labels": labelList("name", many...)}),
			want:  map[string]any{"number": int64(7), "title": "", "url": "", "labels": many[:100]}},
		{name: "nothing optional", kind: forge.KindGitHub, event: "issues", action: "edited", extra: ghIssue(nil),
			want: map[string]any{"number": int64(7), "title": "", "url": "", "labels": []string{}}},
		{name: "optional values of the wrong type", kind: forge.KindGitHub, event: "issues", action: "opened",
			extra: ghIssue(map[string]any{"title": 5, "html_url": 7, "labels": "bug"}),
			want:  map[string]any{"number": int64(7), "title": "", "url": "", "labels": []string{}}},
		{name: "label entries without a string name", kind: forge.KindGitHub, event: "issues", action: "opened",
			extra: ghIssue(map[string]any{"labels": []any{"bug", map[string]any{"name": 5}, map[string]any{}, nil, map[string]any{"name": "p1"}}}),
			want:  map[string]any{"number": int64(7), "title": "", "url": "", "labels": []string{"p1"}}},
		{name: "gitlab reads label titles", kind: forge.KindGitLab, event: "Issue Hook", action: "close",
			extra: glIssue(map[string]any{"labels": []any{map[string]any{"name": "x"}, map[string]any{"title": "a"}}}),
			want:  map[string]any{"number": int64(12), "title": "T", "url": issueURL, "labels": []string{"a"}}},
	}
	checkMetadata(t, cases)
}

func TestHookEvent_DescribesACommentByItsNumberURLAndKindWithoutItsText(t *testing.T) {
	t.Parallel()
	const commentURL, noteURL = issueURL + "#issuecomment-1", "https://forge.example/acme/widgets/-/merge_requests/9#note_1"
	cases := []hookCase{
		{name: "github comment", kind: forge.KindGitHub, event: "issue_comment", action: "created",
			extra: map[string]any{"issue": map[string]any{"number": 7}, "comment": map[string]any{"body": "SECRETTEXT", "html_url": commentURL}},
			want:  map[string]any{"number": int64(7), "comment_url": commentURL, "pull_request": false}},
		{name: "gitlab note on a merge request", kind: forge.KindGitLab, event: "Note Hook", action: nil,
			extra: map[string]any{"object_attributes": map[string]any{"noteable_type": "MergeRequest", "note": "SECRETTEXT", "url": noteURL},
				"merge_request": map[string]any{"iid": 9, "url": "https://forge.example/acme/widgets/-/merge_requests/9"}},
			want: map[string]any{"number": int64(9), "comment_url": noteURL, "pull_request": true}},
		{name: "gitlab note on an issue without a url", kind: forge.KindGitLab, event: "Note Hook", action: nil,
			extra: map[string]any{"object_attributes": map[string]any{"noteable_type": "Issue", "note": "SECRETTEXT"}, "issue": map[string]any{"iid": 5}},
			want:  map[string]any{"number": int64(5), "comment_url": "", "pull_request": false}},
	}
	checkMetadata(t, cases)
	for _, tc := range cases {
		for _, ev := range tc.relayed(t) {
			if strings.Contains(jsonOf(ev), "SECRETTEXT") {
				t.Errorf("%s: event %s holds the comment's text", tc.name, jsonOf(ev))
			}
		}
	}
}

func TestHookEvent_DescribesAPullRequestByItsBranchesAndWhetherItMerged(t *testing.T) {
	t.Parallel()
	const prURL = "https://forge.example/acme/widgets/pull/7"
	ghPull := func(members map[string]any) map[string]any {
		return map[string]any{"pull_request": overlay(map[string]any{"number": 7, "html_url": prURL,
			"head": map[string]any{"ref": "feat"}, "base": map[string]any{"ref": "main"}}, members)}
	}
	glMR := func(members map[string]any) map[string]any {
		return map[string]any{"object_attributes": overlay(map[string]any{"iid": 9, "url": prURL,
			"source_branch": "feat", "target_branch": "main"}, members)}
	}
	pr := func(number int64, head, base string, merged bool) map[string]any {
		return map[string]any{"number": number, "url": prURL, "head": head, "base": base, "merged": merged}
	}
	cases := []hookCase{
		{name: "github opened", kind: forge.KindGitHub, event: "pull_request", action: "opened", extra: ghPull(nil),
			typ: "parley.forge.pull_request.opened", want: pr(7, "feat", "main", false)},
		{name: "github closed merged", kind: forge.KindGitHub, event: "pull_request", action: "closed", extra: ghPull(map[string]any{"merged": true}),
			typ: "parley.forge.pull_request.merged", want: pr(7, "feat", "main", true)},
		{name: "github closed unmerged", kind: forge.KindGitHub, event: "pull_request", action: "closed", extra: ghPull(map[string]any{"merged": false}),
			typ: "parley.forge.pull_request.closed", want: pr(7, "feat", "main", false)},
		{name: "github without a head", kind: forge.KindGitHub, event: "pull_request", action: "synchronize", extra: ghPull(map[string]any{"head": nil}),
			typ: "parley.forge.pull_request.synchronize", want: pr(7, "", "main", false)},
		{name: "gitlab merge", kind: forge.KindGitLab, event: "Merge Request Hook", action: "merge", extra: glMR(nil),
			typ: "parley.forge.pull_request.merged", want: pr(9, "feat", "main", true)},
		{name: "gitlab update with a new revision", kind: forge.KindGitLab, event: "Merge Request Hook", action: "update",
			extra: glMR(map[string]any{"oldrev": "abc"}), typ: "parley.forge.pull_request.synchronize", want: pr(9, "feat", "main", false)},
		{name: "gitlab branch of the wrong type", kind: forge.KindGitLab, event: "Merge Request Hook", action: "reopen",
			extra: glMR(map[string]any{"source_branch": 5}), typ: "parley.forge.pull_request.reopened", want: pr(9, "", "main", false)},
	}
	checkMetadata(t, cases)
}

func TestHookEvent_DescribesAPushByItsRefAndRevisions(t *testing.T) {
	t.Parallel()
	push := func(ref string, more map[string]any) map[string]any { return overlay(map[string]any{"ref": ref}, more) }
	revisions := map[string]any{"before": "B", "after": "A"}
	cases := []hookCase{
		{name: "github push", kind: forge.KindGitHub, event: "push", extra: push("refs/heads/main", revisions),
			id: "refs/heads/main", want: map[string]any{"ref": "refs/heads/main", "before": "B", "after": "A"}},
		{name: "gitlab tag push", kind: forge.KindGitLab, event: "Tag Push Hook", extra: push("refs/tags/v1", revisions),
			id: "refs/tags/v1", want: map[string]any{"ref": "refs/tags/v1", "before": "B", "after": "A"}},
		{name: "gitlab push without revisions", kind: forge.KindGitLab, event: "Push Hook", extra: push("refs/heads/main", nil),
			id: "refs/heads/main", want: map[string]any{"ref": "refs/heads/main", "before": "", "after": ""}},
		{name: "a revision of the wrong type", kind: forge.KindGitHub, event: "push", extra: push("refs/heads/main", map[string]any{"before": 5, "after": "A"}),
			id: "refs/heads/main", want: map[string]any{"ref": "refs/heads/main", "before": "", "after": "A"}},
	}
	checkMetadata(t, cases)
}

func TestHookEvent_NamesTheForgeLoginAsTheActor(t *testing.T) {
	t.Parallel()
	issue := map[string]any{"issue": map[string]any{"number": 7}}
	glIssue := map[string]any{"object_attributes": map[string]any{"iid": 12}}
	cases := []struct {
		hookCase
		actor string
	}{
		{hookCase{name: "github sender", kind: forge.KindGitHub, event: "issues", action: "opened",
			extra: overlay(issue, map[string]any{"sender": map[string]any{"login": "octo"}})}, "octo"},
		{hookCase{name: "github without a sender", kind: forge.KindGitHub, event: "push", extra: map[string]any{"ref": "refs/heads/main"}}, ""},
		{hookCase{name: "github login of the wrong type", kind: forge.KindGitHub, event: "issues", action: "opened",
			extra: overlay(issue, map[string]any{"sender": map[string]any{"login": 5}})}, ""},
		{hookCase{name: "gitlab user", kind: forge.KindGitLab, event: "Issue Hook", action: "open",
			extra: overlay(glIssue, map[string]any{"user": map[string]any{"username": "gl"}})}, "gl"},
		{hookCase{name: "gitlab update, every event", kind: forge.KindGitLab, event: "Issue Hook", action: "update",
			extra: overlay(glIssue, map[string]any{"user": map[string]any{"username": "gl"},
				"changes": overlay(labelChanges([]string{"a"}, []string{"b"}), map[string]any{"title": map[string]any{}})})}, "gl"},
		{hookCase{name: "gitlab push", kind: forge.KindGitLab, event: "Push Hook",
			extra: map[string]any{"ref": "refs/heads/main", "user_username": "gl", "user": map[string]any{"username": "other"}}}, "gl"},
		{hookCase{name: "gitlab tag push names no user_username", kind: forge.KindGitLab, event: "Tag Push Hook",
			extra: map[string]any{"ref": "refs/tags/v1", "user": map[string]any{"username": "gl"}}}, ""},
	}
	for _, tc := range cases {
		for i, ev := range tc.relayed(t) {
			if ev.Actor != tc.actor {
				t.Errorf("%s: event %d actor %q; want %q", tc.name, i, ev.Actor, tc.actor)
			}
		}
	}
}

func TestHookEvent_CarriesNoTextOfThePayloadAndNoUserinfo(t *testing.T) {
	t.Parallel()
	const withUser = "https://user:pw@forge.example/acme/widgets/issues/7"
	gh := map[string]any{
		"issue": map[string]any{"number": 7, "title": "T", "body": "BODYMARK", "html_url": withUser,
			"labels": []any{map[string]any{"name": "bug", "description": "BODYMARK"}}},
		"label":   map[string]any{"name": "bug", "description": "BODYMARK"},
		"comment": map[string]any{"body": "BODYMARK", "html_url": withUser},
		"pull_request": map[string]any{"number": 7, "title": "T", "body": "BODYMARK", "html_url": withUser, "merged": true,
			"head": map[string]any{"ref": "feat", "label": "BODYMARK"}, "base": map[string]any{"ref": "main"}},
		"ref": "refs/heads/main", "before": "B", "after": "A",
		"commits":     []any{map[string]any{"message": "BODYMARK", "url": withUser}},
		"head_commit": map[string]any{"message": "BODYMARK"},
		"sender":      map[string]any{"login": "octo"},
	}
	gl := func(noteable string) map[string]any {
		return map[string]any{
			"object_attributes": map[string]any{"iid": 12, "title": "T", "description": "BODYMARK", "note": "BODYMARK",
				"url": withUser, "oldrev": "abc", "source_branch": "feat", "target_branch": "main", "noteable_type": noteable,
				"last_commit": map[string]any{"message": "BODYMARK"}},
			"labels": []any{map[string]any{"title": "a", "description": "BODYMARK"}},
			"changes": map[string]any{"description": map[string]any{"previous": "BODYMARK", "current": "BODYMARK"},
				"labels": map[string]any{"previous": labelList("title", "b"), "current": labelList("title", "a")}},
			"issue":         map[string]any{"iid": 12, "description": "BODYMARK", "url": withUser},
			"merge_request": map[string]any{"iid": 9, "description": "BODYMARK", "url": withUser},
			"ref":           "refs/heads/main", "before": "B", "after": "A",
			"commits": []any{map[string]any{"message": "BODYMARK", "url": withUser}},
			"user":    map[string]any{"username": "gl"}, "user_username": "gl",
		}
	}
	cases := []hookCase{
		{kind: forge.KindGitHub, event: "issues", action: "opened", extra: gh},
		{kind: forge.KindGitHub, event: "issues", action: "edited", extra: gh},
		{kind: forge.KindGitHub, event: "issues", action: "unlabeled", extra: gh},
		{kind: forge.KindGitHub, event: "issue_comment", action: "created", extra: gh},
		{kind: forge.KindGitHub, event: "pull_request", action: "opened", extra: gh},
		{kind: forge.KindGitHub, event: "pull_request", action: "closed", extra: gh},
		{kind: forge.KindGitHub, event: "push", extra: gh},
		{kind: forge.KindGitLab, event: "Issue Hook", action: "open", extra: gl("")},
		{kind: forge.KindGitLab, event: "Issue Hook", action: "update", extra: gl("")},
		{kind: forge.KindGitLab, event: "Note Hook", action: "create", extra: gl("Issue")},
		{kind: forge.KindGitLab, event: "Note Hook", action: "create", extra: gl("MergeRequest")},
		{kind: forge.KindGitLab, event: "Merge Request Hook", action: "update", extra: gl("")},
		{kind: forge.KindGitLab, event: "Merge Request Hook", action: "merge", extra: gl("")},
		{kind: forge.KindGitLab, event: "Push Hook", extra: gl("")},
		{kind: forge.KindGitLab, event: "Tag Push Hook", extra: gl("")},
	}
	for _, tc := range cases {
		tc.name = fmt.Sprintf("%s %s %v", tc.kind, tc.event, tc.action)
		for _, ev := range tc.relayed(t) {
			if j := jsonOf(ev); strings.Contains(j, "BODYMARK") || strings.Contains(j, "user:pw@") {
				t.Errorf("%s: event %s holds a text of the payload or a URL's userinfo", tc.name, j)
			}
			for _, key := range []string{"url", "comment_url"} {
				if u, ok := ev.Metadata[key]; ok && u != issueURL {
					t.Errorf("%s: %s %q; want %q", tc.name, key, u, issueURL)
				}
			}
		}
	}

	// Every URL passes through one rule: an absolute URL with a host,
	// without its userinfo, or "".
	urls := []struct{ raw, want string }{
		{withUser, issueURL},
		{"http://user@forge.example/acme/widgets/issues/7?x=1#c", "http://forge.example/acme/widgets/issues/7?x=1#c"},
		{"not a url", ""},
		{"/acme/widgets/issues/7", ""},
		{"mailto:octo@forge.example", ""},
		{"https://forge.example/%zz", ""},
		{"", ""},
	}
	for _, u := range urls {
		checkMetadata(t, []hookCase{{name: "html_url " + u.raw, kind: forge.KindGitHub, event: "issues", action: "opened",
			extra: map[string]any{"issue": map[string]any{"number": 7, "html_url": u.raw}},
			want:  map[string]any{"number": int64(7), "title": "", "url": u.want, "labels": []string{}}}})
	}
}

func TestHookEvent_RecordsTheForgeTheTargetTheDeliveryAndTheActorOfEveryEvent(t *testing.T) {
	t.Parallel()
	w := newDeliveryWorld(t, deliveryTime, nil)
	w.addWorkspace(t, workspace.Workspace{Slug: "cp", Mode: workspace.ModeCarryPatch,
		GitURL: new("https://gitlab.com/acme/cp.git"), UpstreamURL: new("https://gitlab.com/up/widgets.git")})
	push := map[string]any{"ref": "refs/heads/main", "before": "B", "after": "A"}
	deliveries := []struct {
		slug  string
		kind  forge.ForgeKind
		id    string
		event string
		body  []byte
		want  map[string]any
	}{
		{"demo", forge.KindGitHub, "d1", "issues", ghIssue("opened", map[string]any{"sender": map[string]any{"login": "octo"}}),
			map[string]any{"forge": "github", "target": "origin", "delivery_id": "d1", "actor": "octo"}},
		{"cp", forge.KindGitLab, "d2", "Push Hook", glPayload(nil, "up/widgets", overlay(push, map[string]any{"user_username": "gl"})),
			map[string]any{"forge": "gitlab", "target": "upstream", "delivery_id": "d2", "actor": "gl"}},
		{"demo", forge.KindGitHub, "d3", "push", ghPayload(nil, "acme/widgets", push),
			map[string]any{"forge": "github", "target": "origin", "delivery_id": "d3", "actor": ""}},
	}
	for _, d := range deliveries {
		if out := w.record(t, d.slug, d.kind, d.id, d.event, d.body); out != forge.OutcomeRelayed {
			t.Fatalf("delivery %s: outcome %q, want relayed", d.id, out)
		}
		events := w.eventsOf(t, "parley.forge.", d.slug)
		if len(events) == 0 {
			t.Fatalf("delivery %s: no event of %s", d.id, d.slug)
		}
		r := events[len(events)-1]
		if r.ActorType != event.ActorForge || r.ActorID != nil || r.Workspace == nil || *r.Workspace != d.slug ||
			r.Action != r.Type[strings.LastIndex(r.Type, ".")+1:] {
			t.Errorf("delivery %s: event %s", d.id, jsonOf(r))
		}
		meta := recorded{Record: r}.meta(t)
		for k, v := range d.want {
			if meta[k] != v {
				t.Errorf("delivery %s: metadata %s = %v, want %v", d.id, k, meta[k], v)
			}
		}
	}
}
