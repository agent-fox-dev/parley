package forge

import (
	"maps"
	"slices"
)

// maxLabelEvents is the most label events one GitLab issue update relays,
// so that a delivery yields a bounded number of events.
const maxLabelEvents = 100

// mapGitLab is GitLab's mapping. The event name is the X-Gitlab-Event
// header's value and, like the action, matches exactly, case included. A
// note's action and a push's are not looked at. The events' actor is the
// username of the delivery's user, which a push hook sends as a top-level
// member instead of an object.
func mapGitLab(event, action string, payload map[string]any) ([]HookEvent, error) {
	events, err := glEvents(event, action, payload)
	if err != nil {
		return nil, err
	}
	actor := optString(payload, "user", "username")
	if event == "Push Hook" || event == "Tag Push Hook" {
		actor = optString(payload, "user_username")
	}
	return withActor(events, actor), nil
}

// glEvents are the events a listed GitLab hook and action relays.
func glEvents(event, action string, payload map[string]any) ([]HookEvent, error) {
	switch event {
	case "Issue Hook":
		switch action {
		case "open", "close", "reopen":
			iid, err := requiredInt(payload, "object_attributes", "iid")
			if err != nil {
				return nil, err
			}
			return []HookEvent{issueEvent(pastTense[action], iid, glIssue(payload), "")}, nil
		case "update":
			return glIssueUpdate(payload)
		}
	case "Note Hook":
		// The note's kind says which object its number is read from.
		var on string
		switch member(payload, "object_attributes", "noteable_type") {
		case "Issue":
			on = "issue"
		case "MergeRequest":
			on = "merge_request"
		default:
			return nil, errNoMapping
		}
		iid, err := requiredInt(payload, on, "iid")
		if err != nil {
			return nil, err
		}
		return []HookEvent{commentEvent(iid, optString(payload, "object_attributes", "url"), on == "merge_request")}, nil
	case "Merge Request Hook":
		kind, ok := pastTense[action]
		switch {
		case action == "update":
			kind = "synchronize"
		case !ok:
			return nil, errNoMapping
		}
		iid, err := requiredInt(payload, "object_attributes", "iid")
		if err != nil {
			return nil, err
		}
		// An update without a new revision changed only what the hub does
		// not relay, such as the title or the assignees.
		if action == "update" && optString(payload, "object_attributes", "oldrev") == "" {
			return nil, nil
		}
		return []HookEvent{pullEvent(kind, iid, optString(payload, "object_attributes", "url"),
			optString(payload, "object_attributes", "source_branch"), optString(payload, "object_attributes", "target_branch"))}, nil
	case "Push Hook", "Tag Push Hook":
		ref, err := requiredString(payload, "ref")
		if err != nil {
			return nil, err
		}
		return []HookEvent{pushEvent(ref, payload)}, nil
	}
	return nil, errNoMapping
}

// glIssue is what a GitLab issue hook says of its issue; its labels are
// the issue's current ones, at the top level of the payload.
func glIssue(payload map[string]any) issueFields {
	return issueFields{
		title:  optString(payload, "object_attributes", "title"),
		url:    optString(payload, "object_attributes", "url"),
		labels: labelNames(member(payload, "labels"), "title"),
	}
}

// pastTense is the hub's name of what a GitLab action did.
var pastTense = map[string]string{"open": "opened", "close": "closed", "reopen": "reopened", "merge": "merged"}

// glIssueUpdate relays a GitLab issue update, which reports a change of the
// title or description and of the labels in one delivery: edited first,
// then each label removed, then each label added, each group in the byte
// order of the titles, with at most maxLabelEvents label events.
func glIssueUpdate(payload map[string]any) ([]HookEvent, error) {
	iid, err := requiredInt(payload, "object_attributes", "iid")
	if err != nil {
		return nil, err
	}
	issue := glIssue(payload)
	var events []HookEvent
	changes, _ := member(payload, "changes").(map[string]any)
	_, title := changes["title"]
	_, description := changes["description"]
	if title || description {
		events = append(events, issueEvent("edited", iid, issue, ""))
	}
	previous := labelTitles(member(changes, "labels", "previous"))
	current := labelTitles(member(changes, "labels", "current"))
	labels := 0
	for _, group := range []struct {
		kind     string
		from, in map[string]bool
	}{{"unlabeled", previous, current}, {"labeled", current, previous}} {
		for _, name := range slices.Sorted(maps.Keys(group.from)) {
			if group.in[name] || labels == maxLabelEvents {
				continue
			}
			labels++
			events = append(events, issueEvent(group.kind, iid, issue, name))
		}
	}
	return events, nil
}

// labelTitles is the set of the titles of a GitLab label list; an entry
// that is not an object with a non-empty string title names no label.
func labelTitles(list any) map[string]bool {
	entries, _ := list.([]any)
	titles := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if obj, ok := entry.(map[string]any); ok {
			if title, ok := obj["title"].(string); ok && title != "" {
				titles[title] = true
			}
		}
	}
	return titles
}
