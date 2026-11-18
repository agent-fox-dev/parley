package forge

// The reduction's mapping contract, for tests that reduce with a mapping of
// their own.
var (
	ErrNoMapping    = errNoMapping
	ErrMissingField = errMissingField
)

// MapGitHub maps a GitHub delivery of the event and action whose payload is
// the JSON object body, as the reduction does.
func MapGitHub(event, action string, body []byte) ([]HookEvent, error) {
	payload, _ := decodeObject(body)
	return mapGitHub(event, action, payload)
}

// MapGitLab maps a GitLab delivery of the event and action whose payload is
// the JSON object body, as the reduction does.
func MapGitLab(event, action string, body []byte) ([]HookEvent, error) {
	payload, _ := decodeObject(body)
	return mapGitLab(event, action, payload)
}
