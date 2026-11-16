package land_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/land"
)

// lander is the landing identity of the revert tests' worlds.
var lander = land.Committer{Name: "Lander", Email: "lander@example.org"}

// newRevertWorld is a world whose landing identity is lander, with the
// automatic lander following the event log every 50 ms.
func newRevertWorld(t *testing.T) *world {
	t.Helper()
	return startWorld(t, autoLand(50*time.Millisecond, nil), func(d *land.Deps) { d.Committer = lander })
}

// revertCall sends the revert of the change number of api with the body
// (nil for none) and the credential, and returns the status and the
// decoded answer.
func (w *world) revertCall(t *testing.T, credential string, number any, body any) (int, map[string]any) {
	t.Helper()
	status, raw := w.Call(t, http.MethodPost, fmt.Sprintf("/api/v1/workspaces/api/changes/%v/revert", number), credential, body)
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("POST revert of %v answered %d %q: %v", number, status, raw, err)
	}
	return status, out
}

// reverted is revertCall by the owner with the body, which must answer
// 201; it returns the number of the reverting change.
func (w *world) reverted(t *testing.T, number int64, body any) int64 {
	t.Helper()
	status, answer := w.revertCall(t, w.ownerKey, number, body)
	if status != http.StatusCreated {
		t.Fatalf("revert of change %d = %d %v, want 201", number, status, answer)
	}
	return int64(answer["number"].(float64))
}

// landed opens a change of api titled title by the owner from a new branch
// of main's head with one commit per entry of files, oldest first, each
// setting its files, a file of content "" being deleted, and lands it
// through the land route; it returns the change as stored then and the
// tree main had before.
func (w *world) landed(t *testing.T, branch, title string, files ...map[string]string) (change.Change, string) {
	t.Helper()
	main := head(t, w.api, "main")
	before, from := w.tree(t, main), main
	for i, f := range files {
		entries := map[string]treeEntry{}
		for path, content := range f {
			if content != "" {
				entries[path] = treeEntry{"100644", content}
			} else {
				entries[path] = treeEntry{}
			}
		}
		from = commitEntries(t, w.api, branch, from, "", branch+" commit "+strconv.Itoa(i+1), entries)
	}
	ch := w.opened(t, change.OpenInput{Workspace: "api", SourceBranch: branch, Title: &title, Intent: new("why"),
		Kind: change.KindRef, Caller: w.caller()})
	w.landChange(t, ch.Number)
	return w.readChange(t, ch.Number), before
}

// landChange lands the change number of api through the land route.
func (w *world) landChange(t *testing.T, number int64) {
	t.Helper()
	if status, a := w.landCall(t, w.ownerKey, number, "", nil); status != http.StatusOK || a["status"] != land.StatusLanded {
		t.Fatalf("land of change %d = %d %v, want 200 landed", number, status, a)
	}
}

// tree is the tree of the revision rev of api.
func (w *world) tree(t *testing.T, rev string) string {
	t.Helper()
	return strings.TrimSpace(gitrepotest.Git(t, w.api, "rev-parse", rev+"^{tree}"))
}

// changeCount counts the changes in the store.
func (w *world) changeCount(t *testing.T) (n int) {
	t.Helper()
	if err := w.Store.Main().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM changes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// revertEvents counts the events of the types a revert writes.
func (w *world) revertEvents(t *testing.T) (n int) {
	t.Helper()
	for _, typ := range []string{"parley.change.revert", change.EventVersion, change.EventOpen, "parley.git.push"} {
		n += len(w.eventsOf(t, typ))
	}
	return n
}

// eventsAfter are the hub events after the position seq, oldest first.
func (w *world) eventsAfter(t *testing.T, seq int64) []event.Record {
	t.Helper()
	recs, _, err := w.events.ReadAfter(context.Background(), seq, 10000)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// lastSeq is the position of the newest hub event.
func (w *world) lastSeq(t *testing.T) int64 {
	t.Helper()
	recs := w.eventsAfter(t, 0)
	if len(recs) == 0 {
		return 0
	}
	return recs[len(recs)-1].Seq
}

// revertedBy is the reverted_by of the change number of api, as GET of
// the change shows it.
func (w *world) revertedBy(t *testing.T, number int64) any {
	t.Helper()
	status, answer := w.get(t, fmt.Sprintf("/workspaces/api/changes/%d", number), w.ownerKey)
	if status != http.StatusOK {
		t.Fatalf("GET change %d = %d %v", number, status, answer)
	}
	return answer["reverted_by"]
}

// changeSessions is the change component's session linker over the
// sessions the world's session stub knows.
type changeSessions struct{ s *sessionStub }

func (l changeSessions) Check(ctx context.Context, caller change.Caller, id, workspace string) (string, error) {
	return l.s.Check(ctx, caller, id, workspace)
}

func (changeSessions) Record(context.Context, *sql.Tx, string, string, int64, int64) error {
	return nil
}

func TestRevert_OpensAChangeThatUndoesTheLandedCommits(t *testing.T) {
	t.Parallel()
	w := newRevertWorld(t)
	for i, c := range []struct {
		name, strategy, title string
	}{
		{"landed with strategy rebase", land.StrategyRebase, "Change line two"},
		{"landed with strategy squash", land.StrategySquash, "Squashed change"},
		{"titled with 200 characters", land.StrategyRebase, strings.Repeat("é", 200)},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.configure(t, `{"changes.auto_restack":false,"land.strategy":"`+c.strategy+`"}`)
			branch := "orig" + strconv.Itoa(i)
			orig, before := w.landed(t, branch, c.title,
				map[string]string{"a.txt": lines(branch)}, map[string]string{branch + ".txt": branch + "\n"})
			undone := orig.Landed.Commits
			if want := map[bool]int{true: 1, false: 2}[c.strategy == land.StrategySquash]; len(undone) != want {
				t.Fatalf("%s landed %d commits, want %d", orig.Ref, len(undone), want)
			}
			main := head(t, w.api, "main")

			status, got := w.revertCall(t, w.ownerKey, orig.Number, map[string]any{})

			if status != http.StatusCreated {
				t.Fatalf("revert = %d %v, want 201", status, got)
			}
			revertBranch := "revert/" + strconv.FormatInt(orig.Number, 10)
			wantTitle := []rune(`Revert "` + c.title + `"`)
			wantTitle = wantTitle[:min(len(wantTitle), 200)]
			author := map[string]any{"actor_type": change.CredentialAPIKey, "actor_id": w.owner, "username": "owner"}
			if got["reverts"] != float64(orig.Number) || got["target_branch"] != "main" || got["state"] != change.StateOpen ||
				!reflect.DeepEqual(got["source"], map[string]any{"workspace_slug": "api", "branch": revertBranch}) ||
				!reflect.DeepEqual(got["author"], author) || got["title"] != string(wantTitle) ||
				got["intent"] != "Reverts change api#"+strconv.FormatInt(orig.Number, 10)+": "+c.title+"." {
				t.Errorf("the reverting change is %v, want reverts %d from %s into main, open, by the owner, titled %q",
					got, orig.Number, revertBranch, string(wantTitle))
			}
			if n := utf8.RuneCountInString(got["title"].(string)); n > 200 {
				t.Errorf("the title has %d characters, want at most 200", n)
			}
			_, versions := w.get(t, fmt.Sprintf("/workspaces/api/changes/%v/versions", got["number"]), w.ownerKey)
			if items, _ := versions["items"].([]any); len(items) != 1 || items[0].(map[string]any)["kind"] != change.KindRevert {
				t.Errorf("the reverting change's versions are %v, want one of kind revert", versions)
			}
			made := strings.Fields(gitrepotest.Git(t, w.api, "rev-list", "--reverse", main+".."+revertBranch))
			if len(made) != len(undone) {
				t.Fatalf("%s holds %d commits on main, want %d", revertBranch, len(made), len(undone))
			}
			if parent := strings.TrimSpace(gitrepotest.Git(t, w.api, "rev-parse", made[0]+"^")); parent != main {
				t.Errorf("the first reverting commit's parent is %s, want main %s", parent, main)
			}
			for j, sha := range made {
				reverts := undone[len(undone)-1-j].LandedSHA
				subject := strings.TrimSpace(gitrepotest.Git(t, w.api, "log", "-1", "--format=%s", reverts))
				want := "Revert \"" + subject + "\"\n\nThis reverts commit " + reverts + ".\n|Lander <lander@example.org>|Lander <lander@example.org>"
				if got := strings.TrimSpace(gitrepotest.Git(t, w.api, "log", "-1", "--format=%B|%an <%ae>|%cn <%ce>", sha)); got != strings.TrimSpace(want) {
					t.Errorf("reverting commit %d is %q, want %q", j+1, got, want)
				}
			}
			if tree := w.tree(t, revertBranch); tree != before {
				t.Errorf("the tree of %s is %s, want main's before the land, %s", revertBranch, tree, before)
			}
		})
	}
}

func TestRevert_OpensTheRevertWithTheMembersOfTheBody(t *testing.T) {
	t.Parallel()
	w := newRevertWorld(t)
	w.changes.RegisterSessionLinker(changeSessions{w.sessions})
	session := uuid.New().String()
	w.sessions.add(session, w.owner, "api")
	for i, c := range []struct {
		name  string
		body  map[string]any
		check func(t *testing.T, got map[string]any, seq int64)
	}{
		{"a title and an intent", map[string]any{"title": "Undo it", "intent": "It broke the build."},
			func(t *testing.T, got map[string]any, _ int64) {
				if got["title"] != "Undo it" || got["intent"] != "It broke the build." {
					t.Errorf("title, intent = %v, %v; want the body's", got["title"], got["intent"])
				}
			}},
		{"draft", map[string]any{"draft": true}, func(t *testing.T, got map[string]any, seq int64) {
			if got["state"] != change.StateDraft {
				t.Errorf("state = %v, want draft", got["state"])
			}
			for _, e := range w.eventsAfter(t, seq) {
				if e.Type == change.EventOpen {
					t.Errorf("a draft revert wrote %+v", e)
				}
			}
		}},
		{"auto_land", map[string]any{"auto_land": true}, func(t *testing.T, got map[string]any, _ int64) {
			number := int64(got["number"].(float64))
			setter, on, err := w.changes.AutoLandSetter(context.Background(), nil, "api", number)
			if got["auto_land"] != true || err != nil || !on || setter.UserID == nil || *setter.UserID != w.owner {
				t.Errorf("auto_land = %v, set by %+v, %v, %v; want true, set by the owner", got["auto_land"], setter, on, err)
			}
			until(t, "the reverting change lands", func() bool { return w.readChange(t, number).State == change.StateLanded })
			if lands := w.lands(t, number); len(lands) != 1 || lands[0]["trigger"] != land.TriggerAutoLand {
				t.Errorf("the reverting change's lands are %v, want one with trigger auto_land", lands)
			}
		}},
		{"a session of the caller", map[string]any{"session_id": session}, func(t *testing.T, got map[string]any, _ int64) {
			if got["session_id"] != session {
				t.Errorf("session_id = %v, want %s", got["session_id"], session)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			branch := "opt" + strconv.Itoa(i)
			orig, _ := w.landed(t, branch, "Add "+branch, map[string]string{branch + ".txt": branch + "\n"})
			seq := w.lastSeq(t)
			status, got := w.revertCall(t, w.ownerKey, orig.Number, c.body)
			if status != http.StatusCreated {
				t.Fatalf("revert with %v = %d %v, want 201", c.body, status, got)
			}
			c.check(t, got, seq)
		})
	}
}

func TestRevert_CreatesTheBranchUnderTheFirstFreeName(t *testing.T) {
	t.Parallel()
	w := newRevertWorld(t)
	orig, _ := w.landed(t, "named", "Add named", map[string]string{"named.txt": "n\n"})
	n := strconv.FormatInt(orig.Number, 10)
	byHand := commit(t, w.api, "revert/"+n, w.base, "by hand", map[string]string{"hand.txt": "h\n"})

	status, got := w.revertCall(t, w.ownerKey, orig.Number, nil)
	if status != http.StatusCreated || got["source"].(map[string]any)["branch"] != "revert/"+n+"-2" {
		t.Fatalf("revert beside revert/%s = %d %v, want 201 from revert/%s-2", n, status, got, n)
	}
	if h := head(t, w.api, "revert/"+n); h != byHand {
		t.Errorf("revert/%s moved to %s from %s", n, h, byHand)
	}
	if status, answer := w.Call(t, http.MethodPost, fmt.Sprintf("/api/v1/workspaces/api/changes/%v/abandon", got["number"]), w.ownerKey, nil); status != http.StatusOK {
		t.Fatalf("abandon of the reverting change = %d %s", status, answer)
	}

	status, got = w.revertCall(t, w.ownerKey, orig.Number, nil)
	if status != http.StatusCreated || got["source"].(map[string]any)["branch"] != "revert/"+n+"-3" {
		t.Errorf("revert beside revert/%s and revert/%s-2 = %d %v, want 201 from revert/%s-3", n, n, status, got, n)
	}
}

func TestRevert_RecordsItsEventsAndShowsTheRevertOnTheOriginal(t *testing.T) {
	t.Parallel()
	w := newRevertWorld(t)
	orig, _ := w.landed(t, "evented", "Add evented", map[string]string{"evented.txt": "e\n"})
	seq := w.lastSeq(t)

	m := w.reverted(t, orig.Number, nil)

	if by := w.revertedBy(t, orig.Number); by != float64(m) {
		t.Errorf("reverted_by of the original = %v, want %d", by, m)
	}
	n, mm := strconv.FormatInt(orig.Number, 10), strconv.FormatInt(m, 10)
	var found []string
	for _, e := range w.eventsAfter(t, seq) {
		meta := metadata(t, e)
		switch {
		case e.Type == "parley.change.revert" && e.ResourceID == n && e.ActorType == change.CredentialAPIKey &&
			e.ActorID != nil && *e.ActorID == w.owner && meta["reverted_by"] == float64(m):
			found = append(found, "revert")
		case e.Type == change.EventVersion && e.ResourceID == mm && meta["kind"] == change.KindRevert:
			found = append(found, "version")
		case e.Type == change.EventOpen && e.ResourceID == mm:
			found = append(found, "open")
		case e.Type == "parley.git.push" && meta["ref"] == "refs/heads/revert/"+n && meta["created"] == true && meta["via"] == "api":
			found = append(found, "push")
		}
	}
	slices.Sort(found)
	if want := []string{"open", "push", "revert", "version"}; !reflect.DeepEqual(found, want) {
		t.Errorf("the revert wrote the events %v, want one of each of %v", found, want)
	}
}

func TestRevert_RefusesARequestThatIsMalformedOrCannotApply(t *testing.T) {
	t.Parallel()
	w := newRevertWorld(t)
	a, _ := w.landed(t, "ra", "Add ra", map[string]string{"ra.txt": "a\n"})
	draft, open := w.openDraft(t, "rdraft"), w.open(t, "ropen")
	abandoned := w.open(t, "rabandoned")
	if status, raw := w.Call(t, http.MethodPost, fmt.Sprintf("/api/v1/workspaces/api/changes/%d/abandon", abandoned.Number), w.ownerKey, nil); status != http.StatusOK {
		t.Fatalf("abandon = %d %s", status, raw)
	}
	byDraft, _ := w.landed(t, "rb", "Add rb", map[string]string{"rb.txt": "b\n"})
	draftReverter := w.reverted(t, byDraft.Number, map[string]any{"draft": true})
	byOpen, _ := w.landed(t, "rc", "Add rc", map[string]string{"rc.txt": "c\n"})
	openReverter := w.reverted(t, byOpen.Number, nil)
	byLanded, _ := w.landed(t, "rd", "Add rd", map[string]string{"rd.txt": "d\n"})
	landedReverter := w.reverted(t, byLanded.Number, nil)
	w.landChange(t, landedReverter)
	gitrepotest.Git(t, w.api, "branch", "release", "main")
	commit(t, w.api, "rrelease", head(t, w.api, "main"), "release", map[string]string{"rr.txt": "r\n"})
	released := w.opened(t, change.OpenInput{Workspace: "api", SourceBranch: "rrelease", TargetBranch: new("release"),
		Intent: new("why"), Kind: change.KindRef, Caller: w.caller()})
	w.landChange(t, released.Number)
	gitrepotest.Git(t, w.api, "update-ref", "-d", "refs/heads/release")
	noopHead := commit(t, w.api, "rnoop", head(t, w.api, "main"), "noop", map[string]string{"rnoop.txt": "n\n"})
	noop := w.openBranch(t, "rnoop", w.caller())
	gitrepotest.Git(t, w.api, "update-ref", "refs/heads/main", noopHead)
	w.landChange(t, noop.Number)
	empty, _ := w.landed(t, "rempty", "Add e", map[string]string{"e.txt": "e\n"})
	w.landed(t, "rgone", "Remove e", map[string]string{"e.txt": ""})

	for _, c := range []struct {
		name       string
		number     int64
		body       any
		before     func(t *testing.T)
		status     int
		errorType  string
		revertedBy int64
	}{
		{name: "a title that is not a string", number: a.Number, body: map[string]any{"title": 5}, status: 400, errorType: "invalid_request"},
		{name: "a draft that is not a boolean", number: a.Number, body: map[string]any{"draft": "yes"}, status: 400, errorType: "invalid_request"},
		{name: "an auto_land that is not a boolean", number: a.Number, body: map[string]any{"auto_land": 1}, status: 400, errorType: "invalid_request"},
		{name: "a session_id that is not a string", number: a.Number, body: map[string]any{"session_id": 7}, status: 400, errorType: "invalid_request"},
		{name: "an empty title", number: a.Number, body: map[string]any{"title": ""}, status: 400, errorType: "invalid_request"},
		{name: "a title of 201 characters", number: a.Number, body: map[string]any{"title": strings.Repeat("t", 201)}, status: 400, errorType: "invalid_request"},
		{name: "a title with a line break", number: a.Number, body: map[string]any{"title": "a\nb"}, status: 400, errorType: "invalid_request"},
		{name: "an intent of 16,001 characters", number: a.Number, body: map[string]any{"intent": strings.Repeat("i", 16001)}, status: 400, errorType: "invalid_request"},
		{name: "an unknown session", number: a.Number, body: map[string]any{"session_id": "nope"}, status: 400, errorType: "unknown_session"},
		{name: "a draft original", number: draft.Number, status: 409, errorType: "change_not_landed"},
		{name: "an open original", number: open.Number, status: 409, errorType: "change_not_landed"},
		{name: "an abandoned original", number: abandoned.Number, status: 409, errorType: "change_not_landed"},
		{name: "an original a draft reverts", number: byDraft.Number, status: 409, errorType: "already_reverted", revertedBy: draftReverter},
		{name: "an original an open change reverts", number: byOpen.Number, status: 409, errorType: "already_reverted", revertedBy: openReverter},
		{name: "an original a landed change reverts", number: byLanded.Number, status: 409, errorType: "already_reverted", revertedBy: landedReverter},
		{name: "a target branch that is gone", number: released.Number, status: 400, errorType: "target_not_found"},
		{name: "an original that landed nothing", number: noop.Number, status: 400, errorType: "nothing_to_land"},
		{name: "an original whose every undo is empty", number: empty.Number, status: 400, errorType: "nothing_to_land"},
		{name: "an invalid title on an original that is not landed", number: open.Number, body: map[string]any{"title": ""}, status: 400, errorType: "invalid_request"},
		{name: "an archived workspace", number: a.Number, status: 409, errorType: "workspace_archived", before: func(t *testing.T) {
			w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE workspaces SET status = 'archived' WHERE slug = 'api'`)
				return err
			})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.before != nil {
				c.before(t)
			}
			refs, changes, events := gitrepotest.Git(t, w.api, "for-each-ref"), w.changeCount(t), w.revertEvents(t)

			status, answer := w.revertCall(t, w.ownerKey, c.number, c.body)

			if status != c.status || errorType(answer) != c.errorType {
				t.Errorf("revert of %d with %v = %d %v, want %d %s", c.number, c.body, status, answer, c.status, c.errorType)
			}
			if c.revertedBy != 0 {
				details, _ := answer["error"].(map[string]any)["details"].(map[string]any)
				if details["reverted_by"] != float64(c.revertedBy) {
					t.Errorf("details = %v, want reverted_by %d", details, c.revertedBy)
				}
			}
			if r, n, e := gitrepotest.Git(t, w.api, "for-each-ref"), w.changeCount(t), w.revertEvents(t); r != refs || n != changes || e != events {
				t.Errorf("the refused revert changed the refs from\n%s\nto\n%s\nthe changes from %d to %d, the events from %d to %d",
					refs, r, changes, n, events, e)
			}
		})
	}
}

func TestRevert_RefusesAnUndoThatNoLongerAppliesWithAConflictReport(t *testing.T) {
	t.Parallel()
	w := newRevertWorld(t)
	first, _ := w.landed(t, "first", "Set one", map[string]string{"a.txt": lines("one")})
	w.landed(t, "second", "Set two", map[string]string{"a.txt": lines("two")})
	changes, events := w.changeCount(t), w.revertEvents(t)

	status, answer := w.revertCall(t, w.ownerKey, first.Number, nil)

	if status != http.StatusConflict || errorType(answer) != "revert_conflict" {
		t.Fatalf("revert = %d %v, want 409 revert_conflict", status, answer)
	}
	details, _ := answer["error"].(map[string]any)["details"].(map[string]any)
	report, _ := details["conflict"].(map[string]any)
	files, _ := report["files"].([]any)
	if report["target_sha"] != head(t, w.api, "main") || report["commit_sha"] != first.Landed.Commits[0].LandedSHA ||
		len(files) != 1 || files[0].(map[string]any)["path"] != "a.txt" || files[0].(map[string]any)["kind"] != "content" ||
		!reflect.DeepEqual(report["landed_changes"], []any{}) || report["change"] != nil {
		t.Errorf("the conflict report is %v, want target main, commit %s, a.txt of kind content, no landed changes, no change",
			report, first.Landed.Commits[0].LandedSHA)
	}
	if h := head(t, w.api, "revert/"+strconv.FormatInt(first.Number, 10)); h != "" || w.changeCount(t) != changes || w.revertEvents(t) != events {
		t.Errorf("the refused revert left revert/%d at %q, %d changes from %d, %d events from %d",
			first.Number, h, w.changeCount(t), changes, w.revertEvents(t), events)
	}
}

func TestRevert_ReportsTheNewestLiveRevertAsRevertedBy(t *testing.T) {
	t.Parallel()
	w := newRevertWorld(t)
	orig, _ := w.landed(t, "live", "Add live", map[string]string{"live.txt": "l\n"})
	if by := w.revertedBy(t, orig.Number); by != nil {
		t.Fatalf("reverted_by before any revert = %v, want null", by)
	}
	m := w.reverted(t, orig.Number, nil)
	if by := w.revertedBy(t, orig.Number); by != float64(m) {
		t.Errorf("reverted_by = %v, want %d", by, m)
	}
	if status, raw := w.Call(t, http.MethodPost, fmt.Sprintf("/api/v1/workspaces/api/changes/%d/abandon", m), w.ownerKey, nil); status != http.StatusOK {
		t.Fatalf("abandon = %d %s", status, raw)
	}
	if by := w.revertedBy(t, orig.Number); by != nil {
		t.Errorf("reverted_by once the revert is abandoned = %v, want null", by)
	}
	k := w.reverted(t, orig.Number, nil)
	if by := w.revertedBy(t, orig.Number); by != float64(k) {
		t.Errorf("reverted_by after the second revert = %v, want %d", by, k)
	}
}

func TestRevert_OfALandedRevertRestoresWhatTheOriginalLanded(t *testing.T) {
	t.Parallel()
	w := newRevertWorld(t)
	orig, _ := w.landed(t, "again", "Change twice", map[string]string{"a.txt": lines("again")}, map[string]string{"again.txt": "a\n"})
	after := w.tree(t, "main")
	b := w.reverted(t, orig.Number, nil)
	w.landChange(t, b)

	status, got := w.revertCall(t, w.ownerKey, b, nil)

	if status != http.StatusCreated || got["reverts"] != float64(b) {
		t.Fatalf("revert of the revert = %d %v, want 201 reverting %d", status, got, b)
	}
	w.landChange(t, int64(got["number"].(float64)))
	if tree := w.tree(t, "main"); tree != after {
		t.Errorf("main's tree is %s, want %s, what it was after the original landed", tree, after)
	}
}
