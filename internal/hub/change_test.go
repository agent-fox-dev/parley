package hub_test

import (
	"context"
	"database/sql"
	"errors"
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

// A push that lost the race to open the change of its branch is answered
// with the change the other push opened, and records its next version.
func TestPushChanges_AnswersALostOpenWithTheChangeAndRecordsItsVersion(t *testing.T) {
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

	changes := hub.PushChanges(c)
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
	if want := (&pushrule.Version{WorkspaceSlug: "demo", Number: opened.Number, Version: 2}); err != nil || !reflect.DeepEqual(v, want) {
		t.Fatalf("version = %+v, %v; want %+v", v, err, want)
	}
	if v, err := changes.RecordPushVersion(ctx, record); err != nil || v != nil {
		t.Errorf("the same head again = %+v, %v; want nothing recorded", v, err)
	}
}
