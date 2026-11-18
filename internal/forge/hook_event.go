package forge

import (
	"maps"
	"net/url"
	"strconv"

	"github.com/agent-fox-dev/parley/internal/event"
)

// An event travels to every reader of the workspace and to event webhooks,
// so it carries identifiers and links only: what an event of each kind
// holds is built here, from the few values a forge's mapping reads, and
// nothing else of the payload reaches it.

// maxTitleBytes is the most bytes of an issue title an event keeps.
const maxTitleBytes = 256

// maxLabels is the most label names an issue event lists.
const maxLabels = 100

// issueFields are what a forge says of an issue beside its number.
type issueFields struct {
	title, url string
	labels     []string
}

// issueEvent is the event parley.forge.issue.<action> of the issue: its
// number, link and labels, with its title, or with the label of a label
// event.
func issueEvent(action string, number int64, f issueFields, label string) HookEvent {
	m := map[string]any{"number": number, "url": cleanURL(f.url), "labels": f.labels}
	if action == "labeled" || action == "unlabeled" {
		m["label"] = label
	} else {
		m["title"] = cutUTF8(f.title, maxTitleBytes)
	}
	return HookEvent{Type: "parley.forge.issue." + action, ResourceType: "issue", ResourceID: strconv.FormatInt(number, 10), Metadata: m}
}

// commentEvent is the event of a comment created on the issue or pull
// request of the number: its link and which of the two it is on, never its
// text.
func commentEvent(number int64, commentURL string, onPull bool) HookEvent {
	return HookEvent{Type: "parley.forge.comment.created", ResourceType: "comment", ResourceID: strconv.FormatInt(number, 10),
		Metadata: map[string]any{"number": number, "comment_url": cleanURL(commentURL), "pull_request": onPull}}
}

// pullEvent is the event parley.forge.pull_request.<kind> of the pull
// request: its number, link and branches, and whether it merged.
func pullEvent(kind string, number int64, link, head, base string) HookEvent {
	return HookEvent{Type: "parley.forge.pull_request." + kind, ResourceType: "pull_request", ResourceID: strconv.FormatInt(number, 10),
		Metadata: map[string]any{"number": number, "url": cleanURL(link), "head": head, "base": base, "merged": kind == "merged"}}
}

// pushEvent is the event of a push to the ref, with the revisions before
// and after it, which both forges send as top-level members.
func pushEvent(ref string, payload map[string]any) HookEvent {
	return HookEvent{Type: "parley.forge.push", ResourceType: "ref", ResourceID: ref,
		Metadata: map[string]any{"ref": ref, "before": optString(payload, "before"), "after": optString(payload, "after")}}
}

// input is the hub event of the draft, relayed for the workspace slug from
// the delivery deliveryID of the forge kind for the target repository: the
// forge is its actor, with no id, and its metadata names the forge, the
// target, the delivery and the forge login of its author beside the
// draft's own. The event log sets its action, the last segment of its type.
func (ev HookEvent) input(slug string, kind ForgeKind, target Target, deliveryID string) event.Input {
	m := maps.Clone(ev.Metadata)
	if m == nil {
		m = map[string]any{}
	}
	m["forge"], m["target"], m["delivery_id"], m["actor"] = string(kind), string(target), deliveryID, ev.Actor
	return event.Input{Type: ev.Type, Workspace: &slug, ActorType: event.ActorForge,
		ResourceType: ev.ResourceType, ResourceID: ev.ResourceID, Metadata: m}
}

// withActor gives every event the forge login of the delivery's author.
func withActor(events []HookEvent, actor string) []HookEvent {
	for i := range events {
		events[i].Actor = actor
	}
	return events
}

// optString is the string at path in payload, "" when it is missing or not
// a string: an optional value never makes a delivery malformed.
func optString(payload map[string]any, path ...string) string {
	s, _ := member(payload, path...).(string)
	return s
}

// labelNames are the first maxLabels names of a label list, in its order:
// the string member key of each entry; an entry that is not an object with
// a string under key names none. Never nil, so that it is a JSON list.
func labelNames(list any, key string) []string {
	entries, _ := list.([]any)
	names := []string{}
	for _, entry := range entries {
		if len(names) == maxLabels {
			break
		}
		if obj, ok := entry.(map[string]any); ok {
			if name, ok := obj[key].(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

// cleanURL is raw as an absolute URL with a host and without its userinfo,
// which may hold a credential; "" when raw is anything else.
func cleanURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return ""
	}
	u.User = nil
	return u.String()
}
