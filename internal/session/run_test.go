package session_test

import (
	"context"
	"strings"
	"testing"
)

func TestRunOf_AnswersTheRunIdOfAStoredSession(t *testing.T) {
	w := newWorld(t)
	const run = "20260102_030405_a1b2c3"
	withRun := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo, "run_id": run})["id"].(string)
	withoutRun := w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)

	cases := []struct {
		name, id, run string
		found         bool
	}{
		{"a session with a run id", withRun, run, true},
		{"its id in upper case", strings.ToUpper(withRun), run, true},
		{"a session without a run id", withoutRun, "", true},
		{"an id that names no session", freshID(), "", false},
		{"an id that is not a UUID", "s1", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run, found, err := w.sessions.RunOf(context.Background(), c.id)
			if err != nil || run != c.run || found != c.found {
				t.Errorf("RunOf(%q) = %q, %v, %v; want %q, %v, nil", c.id, run, found, err, c.run, c.found)
			}
		})
	}
}
