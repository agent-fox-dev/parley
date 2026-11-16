package land_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
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
	return w.conflictRead(t, credential, slug, number, conflictRoute)
}

// The conflict reads: the report of a change, and its file a.txt.
const (
	conflictRoute = "/conflict"
	fileRoute     = "/conflict/file?path=a.txt"
)

// conflictRead sends GET …/changes/:number<route> of the workspace slug
// with the credential and returns the status and the decoded answer, nil
// for one that is not JSON.
func (w *world) conflictRead(t *testing.T, credential, slug string, number any, route string) (int, map[string]any) {
	t.Helper()
	status, contentType, raw := w.conflictCall(t, credential, slug, number, route)
	if !strings.HasPrefix(contentType, "application/json") {
		return status, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("GET %s answered %d %q: %v", route, status, raw, err)
	}
	return status, out
}

// conflictCall sends GET …/changes/:number<route> of the workspace slug
// with the credential and returns the status, the Content-Type and the
// body of the answer.
func (w *world) conflictCall(t *testing.T, credential, slug string, number any, route string) (int, string, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, w.URL(fmt.Sprintf("/api/v1/workspaces/%s/changes/%v%s", slug, number, route)), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, res.Header.Get("Content-Type"), body
}

// fileOf sends GET …/changes/:number/conflict/file of api with the
// owner's key and the path, percent-encoded, as its query, and returns
// the status, the Content-Type and the body of the answer.
func (w *world) fileOf(t *testing.T, number int64, path string) (int, string, []byte) {
	t.Helper()
	return w.conflictCall(t, w.ownerKey, "api", number, "/conflict/file?path="+url.PathEscape(path))
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
		// setup makes the change and returns it.
		setup func(t *testing.T, w *world) change.Change
		// state is the change's state; holdsReport whether it holds a
		// report, which is then the answer.
		state       string
		holdsReport bool
	}{
		{"a landed change with no report", func(t *testing.T, w *world) change.Change {
			ch := w.openSetting(t, "f", map[string]string{"a.txt": lines("change")})
			w.landNow(t, ch, w.caller(), nil)
			commit(t, w.api, "main", head(t, w.api, "main"), "target", map[string]string{"a.txt": lines("target")})
			return ch
		}, change.StateLanded, false},
		{"an abandoned change with a stored report", func(t *testing.T, w *world) change.Change {
			target := commit(t, w.api, "main", w.base, "target", map[string]string{"a.txt": lines("target")})
			ch := w.openSetting(t, "f", map[string]string{"a.txt": lines("change")})
			w.mergeable(t, ch.Number, change.MergeableConflict)
			w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
				return w.changes.Abandon(ctx, tx, "api", ch.Number, "", w.caller())
			})
			commit(t, w.api, "main", target, "agree", map[string]string{"a.txt": lines("change")})
			return ch
		}, change.StateAbandoned, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			replays := land.CountReplays(w.land)
			ch := c.setup(t, w)
			stored := w.readChange(t, ch.Number)
			report := decoded(t, stored.Conflict)
			if stored.State != c.state || (report != nil) != c.holdsReport {
				t.Fatalf("the change is %s holding the report %v; want %s, holding one %v", stored.State, report, c.state, c.holdsReport)
			}
			want := map[string]any{"mergeable": "clean", "conflict": nil}
			if c.holdsReport {
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
	for _, route := range []string{conflictRoute, fileRoute} {
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
			status, answer := w.conflictRead(t, c.credential, c.slug, c.number, route)
			errObj, _ := answer["error"].(map[string]any)
			if status != c.status || errorType(answer) != c.errorType || c.status != 200 && errObj["code"] != float64(status) {
				t.Errorf("%s: GET %s = %d %v, want %d %s", c.name, route, status, answer, c.status, c.errorType)
			}
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
	if status, _, text := w.conflictCall(t, bound, "api", changes[0].Number, fileRoute); status != 200 ||
		!strings.Contains(string(text), "<<<<<<< target\ntarget\n=======\napi--f\n>>>>>>> change\n") {
		t.Errorf("GET conflict/file of the fork's change = %d %q, want 200 and its text", status, text)
	}
	for _, route := range []string{conflictRoute, fileRoute} {
		status, foreign := w.conflictRead(t, bound, "api", changes[1].Number, route)
		if status != 404 || errorType(foreign) != "change_not_found" {
			t.Errorf("GET %s of another fork's change = %d %v, want 404 change_not_found", route, status, foreign)
		}
		if _, unknown := w.conflictRead(t, bound, "api", 999, route); !reflect.DeepEqual(errorType(unknown), errorType(foreign)) {
			t.Errorf("GET %s: another fork's change is refused as %v, an unknown one as %v", route, foreign, unknown)
		}
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
		for _, route := range []string{conflictRoute, fileRoute} {
			status, answer := w.conflictRead(t, w.ownerKey, "api", ch.Number, route)
			if status != 409 || errorType(answer) != "conflict" {
				t.Errorf("GET %s of the %s change = %d %v, want 409 conflict", route, w.readChange(t, ch.Number).State, status, answer)
			}
		}
		gitrepotest.Git(t, w.api, "update-ref", "refs/heads/release", w.base)
	}
	if n := replays(); n != 0 {
		t.Errorf("the reads ran %d replays, want none", n)
	}
}

func TestConflictFile_AnswersAContentConflictAsTextBetweenTargetAndChangeMarkers(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	paths := []string{"app.txt", "dir/a b.txt"}
	files := func(text string) map[string]string {
		return map[string]string{paths[0]: text, paths[1]: text}
	}
	start := commit(t, w.api, "main", w.base, "add", files("one\ntwo\nthree\n"))
	commit(t, w.api, "main", start, "target", files("one\nTARGET\nthree\n"))
	commit(t, w.api, "f", start, "change", files("one\nCHANGE\nthree\nfour\n"))
	ch := w.openBranch(t, "f", w.caller())
	for _, path := range paths {
		status, contentType, text := w.fileOf(t, ch.Number, path)
		want := "one\n<<<<<<< target\nTARGET\n=======\nCHANGE\n>>>>>>> change\nthree\nfour\n"
		if status != 200 || contentType != "text/plain; charset=utf-8" || string(text) != want {
			t.Errorf("GET conflict/file of %s = %d %s %q, want 200 text/plain; charset=utf-8 %q", path, status, contentType, text, want)
		}
	}
}

func TestConflictFile_AnswersTheFileOntoThePresentTargetHead(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	target := commit(t, w.api, "main", w.base, "target", map[string]string{"a.txt": lines("TARGET")})
	ch := w.openSetting(t, "f", map[string]string{"a.txt": lines("CHANGE")})
	w.mergeable(t, ch.Number, change.MergeableConflict)
	if stored := string(w.readChange(t, ch.Number).Conflict); !strings.Contains(stored, target) {
		t.Fatalf("the stored report %s is not of the target %s", stored, target)
	}
	commit(t, w.api, "main", target, "again", map[string]string{"a.txt": lines("TARGET2")})

	status, _, text := w.fileOf(t, ch.Number, "a.txt")

	if status != 200 || !strings.Contains(string(text), "TARGET2\n=======") || strings.Contains(string(text), "TARGET\n=======") {
		t.Errorf("GET conflict/file = %d %q, want 200 with the target's line TARGET2", status, text)
	}
}

func TestConflictFile_RefusesARequestWithoutAPath(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	commit(t, w.api, "main", w.base, "target", map[string]string{"a.txt": lines("target")})
	ch := w.openSetting(t, "f", map[string]string{"a.txt": lines("change")})
	for _, query := range []string{"", "?path="} {
		status, answer := w.conflictRead(t, w.ownerKey, "api", ch.Number, "/conflict/file"+query)
		if status != 400 || errorType(answer) != "invalid_request" {
			t.Errorf("GET conflict/file%s = %d %v, want 400 invalid_request", query, status, answer)
		}
	}
}

func TestConflictFile_RefusesAPathThatIsNoEntryOfTheCurrentReport(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	commit(t, w.api, "main", w.base, "target", map[string]string{"a.txt": lines("target")})
	conflicting := w.openSetting(t, "f", map[string]string{"a.txt": lines("change"), "b.txt": "b\n"})
	clean := w.openSetting(t, "g", map[string]string{"c.txt": "c\n"})
	for _, c := range []struct {
		name   string
		number int64
		path   string
	}{
		{"a path the conflicting change changed without a conflict", conflicting.Number, "b.txt"},
		{"a path that exists nowhere", conflicting.Number, "nosuch.txt"},
		{"a path the clean change changed", clean.Number, "c.txt"},
	} {
		status, answer := w.conflictRead(t, w.ownerKey, "api", c.number, "/conflict/file?path="+url.PathEscape(c.path))
		if status != 404 || errorType(answer) != "path_not_found" {
			t.Errorf("%s: GET conflict/file = %d %v, want 404 path_not_found", c.name, status, answer)
		}
	}
}

func TestConflictFile_RefusesAnEntryThatIsNoContentConflict(t *testing.T) {
	t.Parallel()
	text := func(content string) treeEntry { return treeEntry{"100644", content} }
	gitlink := func(c string) treeEntry { return treeEntry{"160000", strings.Repeat(c, 40)} }
	eight := "1\n2\n3\n4\n5\n6\n7\n8\n"
	for _, c := range []struct {
		kind, path           string
		base, target, change map[string]treeEntry
	}{
		{"add_add", "f.txt", nil, map[string]treeEntry{"f.txt": text("target\n")}, map[string]treeEntry{"f.txt": text("change\n")}},
		{"modify_delete", "f.txt", map[string]treeEntry{"f.txt": text("f\n")},
			map[string]treeEntry{"f.txt": text("target\n")}, map[string]treeEntry{"f.txt": {}}},
		{"delete_modify", "f.txt", map[string]treeEntry{"f.txt": text("f\n")},
			map[string]treeEntry{"f.txt": {}}, map[string]treeEntry{"f.txt": text("change\n")}},
		{"rename", "g.txt", map[string]treeEntry{"f.txt": text(eight)},
			map[string]treeEntry{"f.txt": {}, "g.txt": text(eight)}, map[string]treeEntry{"f.txt": {}, "h.txt": text(eight)}},
		{"mode", "f.sh", nil, map[string]treeEntry{"f.sh": {"100755", "s\n"}}, map[string]treeEntry{"f.sh": text("s\n")}},
		{"binary", "bin", map[string]treeEntry{"bin": text("\x00base")},
			map[string]treeEntry{"bin": text("\x00target")}, map[string]treeEntry{"bin": text("\x00change")}},
		{"submodule", "sub", map[string]treeEntry{"sub": gitlink("5")},
			map[string]treeEntry{"sub": gitlink("6")}, map[string]treeEntry{"sub": gitlink("7")}},
	} {
		t.Run(c.kind, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			start := commitEntries(t, w.api, "main", w.base, "", "base", c.base)
			commitEntries(t, w.api, "main", start, "", "target", c.target)
			commitEntries(t, w.api, "f", start, "", "change", c.change)
			ch := w.openBranch(t, "f", w.caller())
			_, answer := w.conflictOf(t, w.ownerKey, "api", ch.Number)
			report, _ := answer["conflict"].(map[string]any)
			if files, _ := report["files"].([]any); len(files) != 1 || !reflect.DeepEqual(
				[]any{files[0].(map[string]any)["path"], files[0].(map[string]any)["kind"]}, []any{c.path, c.kind}) {
				t.Fatalf("GET conflict = %v, want one %s conflict in %s", answer, c.kind, c.path)
			}

			status, answer := w.conflictRead(t, w.ownerKey, "api", ch.Number, "/conflict/file?path="+url.PathEscape(c.path))

			if status != 409 || errorType(answer) != "not_text_conflict" {
				t.Errorf("GET conflict/file of the %s conflict = %d %v, want 409 not_text_conflict", c.kind, status, answer)
			}
		})
	}
}
