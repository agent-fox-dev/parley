package forge

import (
	"maps"
	"slices"
	"strconv"
)

// maxLabelEvents is the most label events one GitLab issue update relays,
// so that a delivery yields a bounded number of events.
const maxLabelEvents = 100

// mapGitLab is GitLab's mapping. The event name is the X-Gitlab-Event
// header's value and, like the action, matches exactly, case included. A
// note's action and a push's are not looked at.
func mapGitLab(event, action string, payload map[string]any) ([]HookEvent, error) {
	switch event {
	case "Issue Hook":
		switch action {
		case "open", "close", "reopen":
			return numbered(payload, "parley.forge.issue."+pastTense[action], "issue", nil, "object_attributes", "iid")
		case "update":
			return glIssueUpdate(payload)
		}
	case "Note Hook":
		// The note's kind says which object its number is read from.
		switch member(payload, "object_attributes", "noteable_type") {
		case "Issue":
			return numbered(payload, "parley.forge.comment.created", "comment",
				map[string]any{"pull_request": false}, "issue", "iid")
		case "MergeRequest":
			return numbered(payload, "parley.forge.comment.created", "comment",
				map[string]any{"pull_request": true}, "merge_request", "iid")
		}
	case "Merge Request Hook":
		switch action {
		case "open", "close", "reopen", "merge":
			return numbered(payload, "parley.forge.pull_request."+pastTense[action], "pull_request", nil, "object_attributes", "iid")
		case "update":
			events, err := numbered(payload, "parley.forge.pull_request.synchronize", "pull_request", nil, "object_attributes", "iid")
			if err != nil {
				return nil, err
			}
			// An update without a new revision changed only what the hub
			// does not relay, such as the title or the assignees.
			if oldrev, _ := member(payload, "object_attributes", "oldrev").(string); oldrev == "" {
				return nil, nil
			}
			return events, nil
		}
	case "Push Hook", "Tag Push Hook":
		ref, err := requiredString(payload, "ref")
		if err != nil {
			return nil, err
		}
		return []HookEvent{{Type: "parley.forge.push", ResourceType: "ref", ResourceID: ref}}, nil
	}
	return nil, errNoMapping
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
	id := strconv.FormatInt(iid, 10)
	var events []HookEvent
	changes, _ := member(payload, "changes").(map[string]any)
	_, title := changes["title"]
	_, description := changes["description"]
	if title || description {
		events = append(events, HookEvent{Type: "parley.forge.issue.edited", ResourceType: "issue", ResourceID: id})
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
			events = append(events, HookEvent{Type: "parley.forge.issue." + group.kind, ResourceType: "issue", ResourceID: id,
				Metadata: map[string]any{"label": name}})
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
