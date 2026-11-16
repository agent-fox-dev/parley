package land_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/land"
)

func TestAttempt_StoresAndEncodesEveryMember(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	east := time.FixedZone("+02:00", 2*60*60)
	created := time.Date(2026, 11, 1, 14, 0, 5, 900_000_000, east)
	sha := func(c string) string { return strings.Repeat(c, 40) }

	fresh := running(attemptID(1), "api", "main", created, 5)
	fresh.SessionID = new("11111111-1111-4111-8111-111111111111")
	fresh.RequestedOverride = &land.Override{Reason: "asked"}

	conflict := running(attemptID(2), "api", "release/1", created, 5, 7)
	conflict.Covered = []land.CoveredChange{
		{Number: 5, Version: 3, Head: sha("1"), Base: sha("2"), SourceWorkspace: "api--f1", SourceBranch: "feat"},
		{Number: 7, Version: 1, Head: sha("3"), Base: sha("1"), SourceWorkspace: "api", SourceBranch: "more"},
	}
	conflict.Strategy, conflict.Trigger, conflict.Attempts = land.StrategySquash, land.TriggerAutoLand, 2
	conflict.RequestedBy = change.Actor{ActorType: change.CredentialPAT, ActorID: new("u1"), Username: new("owner")}
	conflict.Status, conflict.FinishedAt = land.StatusConflict, new(created.Add(90*time.Second))
	conflict.Conflict = &gitrepo.ConflictReport{TargetSHA: sha("4"), HeadSHA: sha("1"), CommitSHA: sha("1"),
		Files: []gitrepo.ConflictFile{{Path: "a.txt", Kind: gitrepo.ConflictContent, Ours: &gitrepo.ConflictSide{SHA: sha("5"), Mode: "100644", Path: "a.txt"}}}}
	conflict.Override, conflict.PolicyRevision = &land.Override{Reason: "hotfix"}, new(int64(4))

	blocked := finished(running(attemptID(3), "api", "main", created, 5), land.StatusBlocked)
	blocked.Blocked = &land.Blocked{Change: 5, Status: "pending"}

	cases := []struct {
		name    string
		attempt land.Attempt
		want    string
	}{
		{"a running attempt has null results and no commits", fresh, `{"id":"` + attemptID(1) + `","workspace_slug":"api",
			"target_branch":"main","changes":[5],"status":"running","trigger":"manual","strategy":"rebase","attempts":1,
			"onto_sha":null,"landed_sha":null,"noop":false,"commits":[],"conflict":null,"blocked":null,"error":null,
			"override":null,"policy_revision":null,"requested_by":{"actor_type":"api_key","actor_id":null,"username":null},
			"session_id":"11111111-1111-4111-8111-111111111111","created_at":"2026-11-01T12:00:05Z","finished_at":null}`},
		{"a conflict keeps its report, override and revision", conflict, `{"id":"` + attemptID(2) + `","workspace_slug":"api",
			"target_branch":"release/1","changes":[5,7],"status":"conflict","trigger":"auto_land","strategy":"squash",
			"attempts":2,"onto_sha":null,"landed_sha":null,"noop":false,"commits":[],
			"conflict":{"target_sha":"` + sha("4") + `","head_sha":"` + sha("1") + `","base_sha":null,"commit_sha":"` + sha("1") + `",
			"files":[{"path":"a.txt","kind":"content","base":null,"ours":{"sha":"` + sha("5") + `","mode":"100644","path":"a.txt"},
			"theirs":null,"landed_by":[]}],"landed_changes":[],"change":null},"blocked":null,"error":null,
			"override":{"reason":"hotfix"},"policy_revision":4,
			"requested_by":{"actor_type":"pat","actor_id":"u1","username":"owner"},"session_id":null,
			"created_at":"2026-11-01T12:00:05Z","finished_at":"2026-11-01T12:01:35Z"}`},
		{"a block names its change with requirements never null", blocked, `{"id":"` + attemptID(3) + `","workspace_slug":"api",
			"target_branch":"main","changes":[5],"status":"blocked","trigger":"manual","strategy":"rebase","attempts":1,
			"onto_sha":null,"landed_sha":null,"noop":false,"commits":[],"conflict":null,
			"blocked":{"change":5,"status":"pending","requirements":[]},"error":null,"override":null,"policy_revision":null,
			"requested_by":{"actor_type":"api_key","actor_id":null,"username":null},"session_id":null,
			"created_at":"2026-11-01T12:00:05Z","finished_at":"2026-11-01T12:01:05Z"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			insertInto(t, s, tc.attempt)
			got, found, err := land.ReadAttempt(context.Background(), s, "api", tc.attempt.ID)
			if err != nil || !found {
				t.Fatalf("ReadAttempt = %v, %v", found, err)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if enc := asJSON(t, got); !reflect.DeepEqual(enc, want) {
				t.Errorf("stored attempt encodes as\n%v\nwant\n%v", enc, want)
			}
			if !reflect.DeepEqual(got.Covered, tc.attempt.Covered) || got.CredentialID != tc.attempt.CredentialID ||
				!reflect.DeepEqual(got.RequestedOverride, tc.attempt.RequestedOverride) {
				t.Errorf("stored attempt covers %+v by credential %q asking %v, want %+v by %q asking %v", got.Covered,
					got.CredentialID, got.RequestedOverride, tc.attempt.Covered, tc.attempt.CredentialID, tc.attempt.RequestedOverride)
			}
		})
	}
}

func TestFinish_RefusesAnAttemptThatIsNotRunning(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	landed := finished(running(attemptID(1), "api", "main", at, 5), land.StatusLanded)
	insertInto(t, s, landed)
	cases := []struct {
		name    string
		attempt land.Attempt
	}{
		{"a second final status", finished(landed, land.StatusFailed)},
		{"the same final status again", landed},
		{"an attempt of another workspace", finished(running(attemptID(1), "other", "main", at, 5), land.StatusFailed)},
		{"an attempt that was never created", finished(running(attemptID(2), "api", "main", at, 5), land.StatusFailed)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := land.FinishAttempt(ctx, s, tc.attempt); !errors.Is(err, land.ErrFinal) {
				t.Errorf("FinishAttempt = %v, want ErrFinal", err)
			}
			got, _, err := land.ReadAttempt(ctx, s, "api", landed.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(asJSON(t, got), asJSON(t, landed)) {
				t.Errorf("the final attempt became %v, want %v", asJSON(t, got), asJSON(t, landed))
			}
		})
	}
}

func TestWorkspaceDeleted_RemovesOnlyThatWorkspacesAttempts(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	insertInto(t, s, running(attemptID(1), "api", "main", at, 1, 2),
		finished(running(attemptID(2), "api", "main", at, 3), land.StatusLanded),
		running(attemptID(3), "other", "main", at, 1))
	if err := s.Tx(ctx, func(tx *sql.Tx) error { return (&land.Component{}).WorkspaceDeleted(ctx, tx, "api") }); err != nil {
		t.Fatal(err)
	}
	for _, k := range []struct {
		workspace, id string
		kept          bool
	}{{"api", attemptID(1), false}, {"api", attemptID(2), false}, {"other", attemptID(3), true}} {
		if _, found, err := land.ReadAttempt(ctx, s, k.workspace, k.id); err != nil || found != k.kept {
			t.Errorf("attempt %s of %s found = %v (%v), want %v", k.id, k.workspace, found, err, k.kept)
		}
	}
	var rows int
	if err := s.Main().QueryRow(`SELECT count(*) FROM land_attempt_changes`).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("covered changes left = %d (%v), want 1, the other workspace's", rows, err)
	}
}

// versionSessions is a session linker of the change component that accepts
// every session.
type versionSessions struct{}

func (versionSessions) Check(_ context.Context, _ change.Caller, id, _ string) (string, error) {
	return strings.ToLower(id), nil
}

func (versionSessions) Record(context.Context, *sql.Tx, string, string, int64, int64) error {
	return nil
}

func TestCommitReads_NameTheSessionThatIntroducedEachCommit(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.changes.RegisterSessionLinker(versionSessions{})
	pushed, landedIn := uuid.New().String(), uuid.New().String()
	w.sessions.add(landedIn, w.owner, "api")
	c1 := commit(t, w.api, "feature", w.base, "c1", map[string]string{"c1.txt": "c1\n"})
	c2 := commit(t, w.api, "feature", c1, "c2", map[string]string{"c2.txt": "c2\n"})
	commit(t, w.api, "main", w.base, "moved", map[string]string{"b.txt": "b\n"})
	ctx := context.Background()
	var ch change.Change
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		ch, err = w.changes.Open(ctx, tx, change.OpenInput{Workspace: "api", SourceBranch: "feature", Intent: new("why"),
			Kind: change.KindPush, Caller: w.caller(), SessionID: new(pushed)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	a := w.landNow(t, ch, w.caller(), &landedIn)
	if a.Status != land.StatusLanded || len(a.Commits) != 2 || a.Commits[0].LandedSHA == c1 {
		t.Fatalf("attempt = %s with commits %+v, want landed as two new commits", a.Status, a.Commits)
	}

	for _, c := range []struct {
		name, sha string
		want      any
	}{
		{"the head of the version a push in a session recorded", c2, pushed},
		{"a commit the land rewrote", a.Commits[0].LandedSHA, landedIn},
		{"the commit the land made the target's head", a.Commits[1].LandedSHA, landedIn},
		{"a commit of no change", w.base, nil},
	} {
		status, got := w.get(t, "/workspaces/api/scm/commits/"+c.sha, w.ownerKey)
		if status != 200 || got["session_id"] != c.want {
			t.Errorf("%s: GET the commit = %d, session_id %v; want %v", c.name, status, got["session_id"], c.want)
		}
	}
}
