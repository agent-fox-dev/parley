package hub_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// pushChangesHub starts a hub with the ready repository workspace demo,
// whose main holds one commit, and returns the hub, its push change
// service, the git directory of demo, and commit, which commits file onto
// branch from the commit from ("" for a root commit) in demo's repository
// and returns the branch's new head, and the head of main.
func pushChangesHub(t *testing.T) (*hubtest.Hub, gitserver.ChangeService, string, func(branch, from, file string) string, string) {
	t.Helper()
	var c *hub.Composition
	h := hubtest.Start(t, func(comp *hub.Composition) { c = comp })
	ctx, now, dir := context.Background(), time.Now().UTC(), c.Workspaces().RepoPath("demo")
	owner, org := h.CreateUser(t, "owner"), h.CreateOrg(t, "acme")
	commit := func(branch, from, file string) string {
		data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
		stream := "commit refs/heads/" + branch + "\ncommitter T <t@example.invalid> 1700000000 +0000\n" + data(file)
		if from != "" {
			stream += "from " + from + "\n"
		}
		if _, err := gitrepotest.Run(dir, stream+"M 100644 inline "+file+"\n"+data(file)+"\n", "fast-import", "--quiet"); err != nil {
			t.Fatal(err)
		}
		return gitrepotest.Refs(t, dir, "refs/heads/")[branch]
	}
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	base := commit("main", "", "a.txt")
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		return workspace.Insert(ctx, tx, workspace.Workspace{
			Slug: "demo", Kind: access.KindRepository, Mode: workspace.ModeStandard, DefaultBranch: new("main"),
			GitURL: new("https://origin.test/demo.git"), OwnerID: owner, OrgID: org, Status: workspace.StatusReady,
			DisplayName: "demo", HeadSHA: &base, CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
		})
	}); err != nil {
		t.Fatal(err)
	}
	return h, hub.PushChanges(c), dir, commit, base
}

// A push that lost the race to open the change of its branch is answered
// with the change the other push opened, and records its next version.
func TestPushChanges_AnswersALostOpenWithTheChangeAndRecordsItsVersion(t *testing.T) {
	ctx := context.Background()
	_, changes, _, commit, base := pushChangesHub(t)
	admin := gitserver.PushCaller{CredentialType: gitserver.CredentialAdminToken}
	open := gitserver.OpenRequest{Workspace: "demo", Source: "demo", Branch: "topic", Head: commit("topic", base, "b.txt"),
		Intent: new("fix it"), Caller: admin}
	opened, err := changes.OpenChange(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	_, err = changes.OpenChange(ctx, open)
	var exists *gitserver.ChangeExistsError
	if !errors.As(err, &exists) || *exists != (gitserver.ChangeExistsError{Workspace: "demo", Number: opened.Number}) {
		t.Fatalf("second open = %v, want the change %d of demo", err, opened.Number)
	}

	record := gitserver.VersionRequest{Workspace: "demo", Number: opened.Number, Source: "demo", Branch: "topic",
		Head: commit("topic", open.Head, "c.txt"), Caller: admin}
	v, err := changes.RecordPushVersion(ctx, record)
	want := &pushrule.Version{WorkspaceSlug: "demo", Number: opened.Number, Version: 2}
	if err != nil || !reflect.DeepEqual(v, want) {
		t.Fatalf("version = %+v, %v; want %+v", v, err, want)
	}
	if v, err := changes.RecordPushVersion(ctx, record); err != nil || !reflect.DeepEqual(v, want) {
		t.Errorf("the same head again = %+v, %v; want %+v, nothing recorded", v, err, want)
	}
}

// A push whose head an open followed was answered by the hub: the
// version is the push's, by its caller; a push whose head a later write
// replaced is answered with the change at its current version, and one
// whose change left draft and open since gets no version.
func TestPushChanges_AnswersARacedPushWithTheChangeAtItsCurrentVersion(t *testing.T) {
	ctx := context.Background()
	h, changes, dir, commit, base := pushChangesHub(t)
	admin := gitserver.PushCaller{CredentialType: gitserver.CredentialAdminToken}
	first := commit("topic", base, "b.txt")
	second := commit("topic", first, "c.txt")
	opened, err := changes.OpenChange(ctx, gitserver.OpenRequest{Workspace: "demo", Source: "demo", Branch: "topic", Head: first,
		Intent: new("fix it"), Caller: admin})
	if err != nil || opened.Abandoned {
		t.Fatalf("open = %+v, %v; want an open change", opened, err)
	}
	version := func(n int) map[string]any {
		t.Helper()
		status, body := h.Call(t, http.MethodGet, fmt.Sprintf("/api/v1/workspaces/demo/changes/%d/versions/%d", opened.Number, n), h.Token, nil)
		var v map[string]any
		if status != http.StatusOK || json.Unmarshal(body, &v) != nil {
			t.Fatalf("GET version %d = %d %s", n, status, body)
		}
		return v
	}
	want := &pushrule.Version{WorkspaceSlug: "demo", Number: opened.Number, Version: 2}
	for _, c := range []struct{ name, head string }{{"the followed head", second}, {"a head a later write replaced", first}} {
		v, err := changes.RecordPushVersion(ctx, gitserver.VersionRequest{Workspace: "demo", Number: opened.Number, Source: "demo",
			Branch: "topic", Head: c.head, Caller: admin})
		if err != nil || !reflect.DeepEqual(v, want) {
			t.Errorf("%s: version = %+v, %v; want %+v", c.name, v, err, want)
		}
		if v2 := version(2); v2["head_sha"] != second || v2["kind"] != "push" || v2["created_by"].(map[string]any)["actor_type"] != "admin_token" {
			t.Errorf("%s: version 2 = %v, want the head %s of kind push by the admin token", c.name, v2, second)
		}
	}

	// An open whose branch was deleted after the push moved it abandons the
	// change it opened, which then takes no version.
	gone := commit("gone", base, "d.txt")
	gitrepotest.Git(t, dir, "update-ref", "-d", "refs/heads/gone")
	abandoned, err := changes.OpenChange(ctx, gitserver.OpenRequest{Workspace: "demo", Source: "demo", Branch: "gone", Head: gone,
		Intent: new("fix it"), Caller: admin})
	if err != nil || !abandoned.Abandoned {
		t.Fatalf("open of a deleted branch = %+v, %v; want a change abandoned at once", abandoned, err)
	}
	_, err = changes.RecordPushVersion(ctx, gitserver.VersionRequest{Workspace: "demo", Number: abandoned.Number, Source: "demo",
		Branch: "gone", Head: gone, Caller: admin})
	var notOpen *gitserver.ChangeNotOpenError
	if want := (gitserver.ChangeNotOpenError{Workspace: "demo", Number: abandoned.Number, State: "abandoned"}); !errors.As(err, &notOpen) || *notOpen != want {
		t.Errorf("the version of an abandoned change = %v, want %+v", err, want)
	}
}
