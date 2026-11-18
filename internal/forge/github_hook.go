package forge

import "strconv"

// mapGitHub is GitHub's mapping. Event names and actions match exactly,
// case included: GitHub sends them in lower case, so another spelling is
// not an event the hub knows. A push has no action and its action is not
// looked at.
func mapGitHub(event, action string, payload map[string]any) ([]HookEvent, error) {
	switch event {
	case "issues":
		switch action {
		case "opened", "edited", "closed", "reopened":
			return numbered(payload, "parley.forge.issue."+action, "issue", nil, "issue", "number")
		case "labeled", "unlabeled":
			label, err := requiredString(payload, "label", "name")
			if err != nil {
				return nil, err
			}
			return numbered(payload, "parley.forge.issue."+action, "issue", map[string]any{"label": label}, "issue", "number")
		}
	case "issue_comment":
		if action == "created" {
			// GitHub sends a pull request's comments as comments of its
			// issue; issue.pull_request tells the two apart.
			_, onPull := member(payload, "issue", "pull_request").(map[string]any)
			return numbered(payload, "parley.forge.comment.created", "comment",
				map[string]any{"pull_request": onPull}, "issue", "number")
		}
	case "pull_request":
		switch action {
		case "opened", "reopened", "synchronize":
			return numbered(payload, "parley.forge.pull_request."+action, "pull_request", nil, "pull_request", "number")
		case "closed":
			kind := "closed"
			if member(payload, "pull_request", "merged") == true {
				kind = "merged"
			}
			return numbered(payload, "parley.forge.pull_request."+kind, "pull_request", nil, "pull_request", "number")
		}
	case "push":
		ref, err := requiredString(payload, "ref")
		if err != nil {
			return nil, err
		}
		return []HookEvent{{Type: "parley.forge.push", ResourceType: "ref", ResourceID: ref}}, nil
	}
	return nil, errNoMapping
}

// numbered is the one event of type typ on the resource whose id is the
// required number at path in payload, with the metadata given.
func numbered(payload map[string]any, typ, resourceType string, metadata map[string]any, path ...string) ([]HookEvent, error) {
	number, err := requiredInt(payload, path...)
	if err != nil {
		return nil, err
	}
	return []HookEvent{{Type: typ, ResourceType: resourceType, ResourceID: strconv.FormatInt(number, 10), Metadata: metadata}}, nil
}
