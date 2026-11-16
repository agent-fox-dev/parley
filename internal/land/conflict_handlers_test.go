package land_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/land"
)

// conflictOf sends GET …/changes/:number/conflict of the workspace slug
// with the credential and returns the status and the decoded answer.
func (w *world) conflictOf(t *testing.T, credential, slug string, number any) (int, map[string]any) {
	t.Helper()
	path := fmt.Sprintf("/api/v1/workspaces/%s/changes/%v/conflict", slug, number)
	status, raw := w.Call(t, http.MethodGet, path, credential, nil)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("GET %s answered %d %q: %v", path, status, raw, err)
	}
	return status, out
}

// decoded is the JSON value raw, nil for none.
func decoded(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

func TestConflict_AnswersTheStoredReportOfThePresentPairWithoutAReplay(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// setup makes a change whose check stored a conflict for its
		// present pair, and returns it with its onto commit.
		setup func(t *testing.T, w *world) (change.Change, string)
	}{
		{"an open change that conflicts with the target", func(t *testing.T, w *world) (change.Change, string) {
			target := commit(t, w.api, "main", w.base, "target", map[string]string{"a.txt": lines("target")})
			ch := w.openSetting(t, "f", map[string]string{"a.txt": lines("change")})
			w.mergeable(t, ch.Number, change.MergeableConflict)
			return ch, target
		}},
		{"a change on an open change whose head it conflicts with", func(t *testing.T, w *world) (change.Change, string) {
			lower, upper := w.conflictingAbove(t)
			w.mergeable(t, upper.Number, change.MergeableConflict)
			return upper, w.readChange(t, lower.Number).HeadSHA
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			replays := land.CountReplays(w.land)
			ch, onto := c.setup(t, w)
			stored := w.readChange(t, ch.Number)
			report, _ := decoded(t, stored.Conflict).(map[string]any)
			if report["target_sha"] != onto || report["head_sha"] != stored.HeadSHA {
				t.Fatalf("the stored report is of %v onto %v, want %s onto %s", report["head_sha"], report["target_sha"], stored.HeadSHA, onto)
			}
			status, answer := w.conflictOf(t, w.ownerKey, "api", ch.Number)
			want := map[string]any{"mergeable": "conflict", "conflict": report}
			if status != 200 || !reflect.DeepEqual(answer, want) {
				t.Errorf("GET conflict = %d %v, want 200 %v", status, answer, want)
			}
			if n := replays(); n != 0 {
				t.Errorf("the read ran %d replays, want none", n)
			}
		})
	}
}

func TestConflict_ReplaysTheChangeOntoThePresentPairWhenNothingStoredIsOfIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// setup makes the change and returns it with the onto commit of a
		// conflict, "" for clean.
		setup func(t *testing.T, w *world) (change.Change, string)
	}{
		{"stored clean, then the target gets a commit that conflicts", func(t *testing.T, w *world) (change.Change, string) {
			ch := w.openSetting(t, "f", map[string]string{"a.txt": lines("change")})
			w.mergeable(t, ch.Number, change.MergeableClean)
			return ch, commit(t, w.api, "main", w.base, "target", map[string]string{"a.txt": lines("target")})
		}},
		{"stored conflict, then the target gets a commit that removes it", func(t *testing.T, w *world) (change.Change, string) {
			target := commit(t, w.api, "main", w.base, "target", map[string]string{"a.txt": lines("target")})
			ch := w.openSetting(t, "f", map[string]string{"a.txt": lines("change")})
			w.mergeable(t, ch.Number, change.MergeableConflict)
			commit(t, w.api, "main", target, "agree", map[string]string{"a.txt": lines("change")})
			return ch, ""
		}},
		{"a draft stored unknown, without a conflict", func(t *testing.T, w *world) (change.Change, string) {
			ch := w.openDraft(t, "d")
			w.mergeable(t, ch.Number, change.MergeableClean)
			w.setMergeable(t, ch.Number, change.MergeableUnknown, nil)
			return ch, ""
		}},
		{"stored conflict of an earlier version, then a version without it", func(t *testing.T, w *world) (change.Change, string) {
			commit(t, w.api, "main", w.base, "target", map[string]string{"a.txt": lines("target")})
			ch := w.openSetting(t, "f", map[string]string{"a.txt": lines("change")})
			w.mergeable(t, ch.Number, change.MergeableConflict)
			earlier := w.readChange(t, ch.Number).Conflict
			head := commit(t, w.api, "f", w.base, "again", map[string]string{"b.txt": "b\n"})
			w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
				_, err := w.changes.RecordVersion(ctx, tx, change.VersionInput{Workspace: "api", Branch: "f", Head: head, Kind: change.KindRef})
				return err
			})
			w.mergeable(t, ch.Number, change.MergeableClean)
			w.setMergeable(t, ch.Number, change.MergeableConflict, earlier)
			return ch, ""
		}},
		{"a change on an open change whose head it conflicts with, not the target's", func(t *testing.T, w *world) (change.Change, string) {
			lower, upper := w.conflictingAbove(t)
			w.mergeable(t, upper.Number, change.MergeableConflict)
			w.setMergeable(t, upper.Number, change.MergeableUnknown, nil)
			return upper, w.readChange(t, lower.Number).HeadSHA
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			replays := land.CountReplays(w.land)
			ch, onto := c.setup(t, w)
			before, events := w.readChange(t, ch.Number), w.eventCount(t)
			status, answer := w.conflictOf(t, w.ownerKey, "api", ch.Number)
			if status != 200 {
				t.Fatalf("GET conflict = %d %v, want 200", status, answer)
			}
			if onto == "" {
				if want := map[string]any{"mergeable": "clean", "conflict": nil}; !reflect.DeepEqual(answer, want) {
					t.Errorf("GET conflict = %v, want %v", answer, want)
				}
			} else {
				report, _ := answer["conflict"].(map[string]any)
				files, _ := report["files"].([]any)
				if answer["mergeable"] != "conflict" || report["target_sha"] != onto || report["head_sha"] != before.HeadSHA || len(files) != 1 {
					t.Errorf("GET conflict = %v, want a conflict of %s onto %s in a.txt", answer, before.HeadSHA, onto)
				}
			}
			if n := replays(); n != 1 {
				t.Errorf("the read ran %d replays, want 1", n)
			}
			if after := w.readChange(t, ch.Number); !reflect.DeepEqual(after, before) {
				t.Errorf("the read changed the change from %+v to %+v", before, after)
			}
			if n := w.eventCount(t); n != events {
				t.Errorf("the read wrote %d events", n-events)
			}
		})
	}
}

func TestConflict_AnswersALandedOrAbandonedChangeFromWhatIsStored(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// setup makes the change and returns it; a change that holds a
		// report is answered with it.
		setup func(t *testing.T, w *world) change.Change
	}{
		{"a landed change with no report", func(t *testing.T, w *world) change.Change {
			ch := w.openSetting(t, "f", map[string]string{"a.txt": lines("change")})
			w.landNow(t, ch, w.caller(), nil)
			commit(t, w.api, "main", head(t, w.api, "main"), "target", map[string]string{"a.txt": lines("target")})
			return ch
		}},
		{"an abandoned change with a stored report", func(t *testing.T, w *world) change.Change {
			target := commit(t, w.api, "main", w.base, "target", map[string]string{"a.txt": lines("target")})
			ch := w.openSetting(t, "f", map[string]string{"a.txt": lines("change")})
			w.mergeable(t, ch.Number, change.MergeableConflict)
			w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.Abandon(ctx, tx, "api", ch.Number, "", w.caller())
			})
			commit(t, w.api, "main", target, "agree", map[string]string{"a.txt": lines("change")})
			return ch
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			replays := land.CountReplays(w.land)
			ch := c.setup(t, w)
			stored := w.readChange(t, ch.Number)
			want := map[string]any{"mergeable": "clean", "conflict": nil}
			if report := decoded(t, stored.Conflict); report != nil {
				want = map[string]any{"mergeable": "conflict", "conflict": report}
			}
			status, answer := w.conflictOf(t, w.ownerKey, "api", ch.Number)
			if status != 200 || !reflect.DeepEqual(answer, want) {
				t.Errorf("GET conflict of the %s change = %d %v, want 200 %v", stored.State, status, answer, want)
			}
			if n := replays(); n != 0 {
				t.Errorf("the read ran %d replays, want none", n)
			}
		})
	}
}

func TestConflict_RefusesACallerWithoutTheScopeTheAccessOrTheChange(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	replays := land.CountReplays(w.land)
	commit(t, w.api, "main", w.base, "target", map[string]string{"a.txt": lines("target")})
	ch := w.openSetting(t, "f", map[string]string{"a.txt": lines("change")})
	w.mergeable(t, ch.Number, change.MergeableConflict)
	listing := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"workspaces:read"}})
	stranger := w.IssueKey(t, w.CreateUser(t, "stranger"))
	_, member := w.member(t, "member")
	for _, c := range []struct {
		name, credential, slug string
		number                 any
		status                 int
		errorType              string
	}{
		{"a token without changes:read", listing, "api", ch.Number, 403, "insufficient_scope"},
		{"a token without changes:read on an unknown workspace", listing, "nosuch", ch.Number, 403, "insufficient_scope"},
		{"a user who cannot read the workspace", stranger, "api", ch.Number, 404, "not_found"},
		{"an unknown workspace", w.ownerKey, "nosuch", ch.Number, 404, "not_found"},
		{"an unknown change", w.ownerKey, "api", 999, 404, "change_not_found"},
		{"a number that is no number", w.ownerKey, "api", "two", 404, "change_not_found"},
		{"a member of the workspace's org", member, "api", ch.Number, 200, ""},
	} {
		status, answer := w.conflictOf(t, c.credential, c.slug, c.number)
		errObj, _ := answer["error"].(map[string]any)
		if status != c.status || errorType(answer) != c.errorType || c.status != 200 && errObj["code"] != float64(status) {
			t.Errorf("%s: GET conflict = %d %v, want %d %s", c.name, status, answer, c.status, c.errorType)
		}
	}
	if n := replays(); n != 0 {
		t.Errorf("the reads ran %d replays, want none", n)
	}
}

func TestConflict_ConfinesATokenBoundToAForkToTheForksChanges(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	replays := land.CountReplays(w.land)
	commit(t, w.api, "main", w.base, "target", map[string]string{"a.txt": lines("target")})
	var changes []change.Change
	for _, slug := range []string{"api--f", "api--g"} {
		dir := w.fork(t, slug, w.owner)
		commit(t, dir, "work", w.base, "work", map[string]string{"a.txt": lines(slug)})
		ch := w.opened(t, change.OpenInput{Workspace: "api", SourceWorkspace: slug, SourceBranch: "work", Intent: new("why"),
			Kind: change.KindRef, Caller: w.caller()})
		w.mergeable(t, ch.Number, change.MergeableConflict)
		changes = append(changes, ch)
	}
	bound := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"changes:read"}, Workspace: "api--f"})
	if status, answer := w.conflictOf(t, bound, "api", changes[0].Number); status != 200 || answer["mergeable"] != "conflict" {
		t.Errorf("GET conflict of the fork's change = %d %v, want 200 conflict", status, answer)
	}
	status, foreign := w.conflictOf(t, bound, "api", changes[1].Number)
	if status != 404 || errorType(foreign) != "change_not_found" {
		t.Errorf("GET conflict of another fork's change = %d %v, want 404 change_not_found", status, foreign)
	}
	if _, unknown := w.conflictOf(t, bound, "api", 999); !reflect.DeepEqual(errorType(unknown), errorType(foreign)) {
		t.Errorf("another fork's change is refused as %v, an unknown one as %v", foreign, unknown)
	}
	if n := replays(); n != 0 {
		t.Errorf("the reads ran %d replays, want none", n)
	}
}

func TestConflict_RefusesADraftOrOpenChangeWhoseTargetBranchIsGone(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	replays := land.CountReplays(w.land)
	gitrepotest.Git(t, w.api, "update-ref", "refs/heads/release", w.base)
	for _, draft := range []bool{false, true} {
		branch := fmt.Sprintf("draft-%t", draft)
		commit(t, w.api, branch, w.base, branch, map[string]string{branch + ".txt": branch + "\n"})
		ch := w.opened(t, change.OpenInput{Workspace: "api", SourceBranch: branch, Intent: new("why"), Kind: change.KindRef,
			Caller: w.caller(), TargetBranch: new("release"), Draft: draft})
		w.mergeable(t, ch.Number, change.MergeableClean)
		gitrepotest.Git(t, w.api, "update-ref", "-d", "refs/heads/release")
		status, answer := w.conflictOf(t, w.ownerKey, "api", ch.Number)
		if status != 409 || errorType(answer) != "conflict" {
			t.Errorf("GET conflict of the %s change = %d %v, want 409 conflict", w.readChange(t, ch.Number).State, status, answer)
		}
		gitrepotest.Git(t, w.api, "update-ref", "refs/heads/release", w.base)
	}
	if n := replays(); n != 0 {
		t.Errorf("the reads ran %d replays, want none", n)
	}
}
