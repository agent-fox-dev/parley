package awareness_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/awareness"
)

func TestEntryOf_SummarisesTheOtherChangeAndCutsTheCommonFiles(t *testing.T) {
	for _, n := range []int{1, 100, 101, 2500} {
		for _, chars := range []int{0, 500, 501} {
			t.Run(fmt.Sprintf("%d files, intent of %d characters", n, chars), func(t *testing.T) {
				common := make([]string, n)
				for i := range common {
					common[i] = fmt.Sprintf("src/f%04d.go", n-1-i)
				}
				intent := strings.Repeat("é", chars)
				other := awareness.Change{
					Workspace: "demo", Number: 7, Title: "Rework a", Intent: intent, State: awareness.StateOpen,
					TargetBranch: "main", Version: 3, HeadSHA: strings.Repeat("a", 40),
					Author:    awareness.Actor{ActorType: "api_key", ActorID: new("u1"), Username: new("dev")},
					SessionID: new(session1), Mergeable: "clean", LandingStatus: "ready",
					UpdatedAt: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC),
				}

				e := awareness.EntryOf(other, common)
				if len(e.Paths) != min(n, 100) || !slices.IsSorted(e.Paths) || e.Paths[0] != "src/f0000.go" {
					t.Errorf("%d paths from %v, want the first %d in ascending order", len(e.Paths), e.Paths[:min(1, len(e.Paths))], min(n, 100))
				}
				if e.PathsTruncated != (n > 100) || e.Count != n {
					t.Errorf("paths_truncated %v, count %d; want %v, %d", e.PathsTruncated, e.Count, n > 100, n)
				}

				raw, err := json.Marshal(e)
				if err != nil {
					t.Fatal(err)
				}
				if !jsonHasKeys(t, raw, "change", "paths", "paths_truncated", "count") {
					t.Errorf("entry %s: want exactly change, paths, paths_truncated and count", raw)
				}
				var wire struct {
					Change json.RawMessage `json:"change"`
				}
				if err := json.Unmarshal(raw, &wire); err != nil {
					t.Fatal(err)
				}
				if !jsonHasKeys(t, wire.Change, "number", "ref", "title", "intent", "state", "author", "session_id", "version",
					"updated_at", "mergeable", "landing") {
					t.Errorf("change %s: want exactly the eleven members of the summary", wire.Change)
				}
				var change struct {
					Ref       string          `json:"ref"`
					Intent    string          `json:"intent"`
					Author    json.RawMessage `json:"author"`
					Landing   json.RawMessage `json:"landing"`
					UpdatedAt string          `json:"updated_at"`
				}
				if err := json.Unmarshal(wire.Change, &change); err != nil {
					t.Fatal(err)
				}
				if change.Ref != "demo#7" {
					t.Errorf("ref = %q, want demo#7", change.Ref)
				}
				if got := utf8.RuneCountInString(change.Intent); got != min(chars, 500) || !strings.HasPrefix(intent, change.Intent) {
					t.Errorf("intent of %d characters, want the first %d", got, min(chars, 500))
				}
				if string(change.Landing) != `{"status":"ready"}` {
					t.Errorf("landing = %s, want {\"status\":\"ready\"}", change.Landing)
				}
				if string(change.Author) != `{"actor_type":"api_key","actor_id":"u1","username":"dev"}` {
					t.Errorf("author = %s", change.Author)
				}
				if change.UpdatedAt != "2026-11-16T10:00:00Z" {
					t.Errorf("updated_at = %q, want 2026-11-16T10:00:00Z", change.UpdatedAt)
				}
			})
		}
	}
}
