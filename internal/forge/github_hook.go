package forge

// mapGitHub is GitHub's mapping. Event names and actions match exactly,
// case included: GitHub sends them in lower case, so another spelling is
// not an event the hub knows. A push has no action and its action is not
// looked at. The event's actor is the login of the delivery's sender.
func mapGitHub(event, action string, payload map[string]any) ([]HookEvent, error) {
	ev, err := ghEvent(event, action, payload)
	if err != nil {
		return nil, err
	}
	return withActor([]HookEvent{ev}, optString(payload, "sender", "login")), nil
}

// ghEvent is the one event a listed GitHub event and action relays.
func ghEvent(event, action string, payload map[string]any) (HookEvent, error) {
	switch event {
	case "issues":
		switch action {
		case "opened", "edited", "closed", "reopened":
			number, err := requiredInt(payload, "issue", "number")
			if err != nil {
				return HookEvent{}, err
			}
			return issueEvent(action, number, ghIssue(payload), ""), nil
		case "labeled", "unlabeled":
			label, err := requiredString(payload, "label", "name")
			if err != nil {
				return HookEvent{}, err
			}
			number, err := requiredInt(payload, "issue", "number")
			if err != nil {
				return HookEvent{}, err
			}
			return issueEvent(action, number, ghIssue(payload), label), nil
		}
	case "issue_comment":
		if action == "created" {
			number, err := requiredInt(payload, "issue", "number")
			if err != nil {
				return HookEvent{}, err
			}
			// GitHub sends a pull request's comments as comments of its
			// issue; issue.pull_request tells the two apart.
			_, onPull := member(payload, "issue", "pull_request").(map[string]any)
			return commentEvent(number, optString(payload, "comment", "html_url"), onPull), nil
		}
	case "pull_request":
		kind := action
		switch action {
		case "opened", "reopened", "synchronize":
		case "closed":
			if member(payload, "pull_request", "merged") == true {
				kind = "merged"
			}
		default:
			return HookEvent{}, errNoMapping
		}
		number, err := requiredInt(payload, "pull_request", "number")
		if err != nil {
			return HookEvent{}, err
		}
		return pullEvent(kind, number, optString(payload, "pull_request", "html_url"),
			optString(payload, "pull_request", "head", "ref"), optString(payload, "pull_request", "base", "ref")), nil
	case "push":
		ref, err := requiredString(payload, "ref")
		if err != nil {
			return HookEvent{}, err
		}
		return pushEvent(ref, payload), nil
	}
	return HookEvent{}, errNoMapping
}

// ghIssue is what a GitHub payload says of its issue.
func ghIssue(payload map[string]any) issueFields {
	return issueFields{
		title:  optString(payload, "issue", "title"),
		url:    optString(payload, "issue", "html_url"),
		labels: labelNames(member(payload, "issue", "labels"), "name"),
	}
}
