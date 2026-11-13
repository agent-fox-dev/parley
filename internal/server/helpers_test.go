package server

import "testing"

// errorMembers parses body as the REST error envelope and returns the members
// of its error object. It fails the test when body is not a JSON object whose
// only member is an object named error.
func errorMembers(t *testing.T, body []byte) map[string]any {
	t.Helper()
	doc := decodeObject(t, body)
	if len(doc) != 1 {
		t.Fatalf("body %s has %d top-level members, want only error", body, len(doc))
	}
	members, ok := doc["error"].(map[string]any)
	if !ok {
		t.Fatalf("body %s has no error object", body)
	}
	return members
}
