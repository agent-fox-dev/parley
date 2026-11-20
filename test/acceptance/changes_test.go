package acceptance

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// changes is the path of demo's changes.
const changes = "/api/v1/workspaces/demo/changes"

// changePath is the path of change number of demo followed by rest.
func changePath(number int, rest string) string { return fmt.Sprintf("%s/%d%s", changes, number, rest) }

// demoHub starts a hub with alice signed in and the workspace demo she
// owns, registered from a seeded origin whose default branch is main.
func demoHub(t *testing.T) (*harness.Hub, *harness.Workspace) {
	t.Helper()
	hub := harness.Start(t)
	return hub, hub.Workspace(hub.User("alice"), "demo")
}

// pushFiveVersions pushes the new branch feat of a clone made with a PAT
// of demo's owner, asking for a change, then three more commits one push
// each, then the last commit amended in a forced push. It calls afterEach
// after every push and returns the change's number and the head each push
// sent.
func pushFiveVersions(t *testing.T, hub *harness.Hub, demo *harness.Workspace, afterEach func(number int)) (int, []string) {
	t.Helper()
	clone := hub.Clone(demo.Remote, hub.PAT(demo.Owner, harness.AgentScopes))
	hub.Git(clone, "", "checkout", "-q", "-b", "feat").Must(t)
	hub.CommitFiles(clone, "Add the feature", map[string]string{"feature.txt": "step 1\n"})
	hub.PushChange(clone, "", "feat", "add the feature").Must(t)
	number := harness.Num(hub.ChangeOn(demo.Owner.Key, demo.Slug, "feat")["number"])
	heads := []string{hub.Rev(clone, "HEAD")}
	afterEach(number)
	for step := 2; step <= 4; step++ {
		hub.CommitFiles(clone, fmt.Sprintf("Extend the feature, step %d", step),
			map[string]string{"feature.txt": strings.Repeat("step\n", step)})
		hub.Git(clone, "", "push", "-q", "origin", "feat").Must(t)
		heads = append(heads, hub.Rev(clone, "HEAD"))
		afterEach(number)
	}
	hub.Git(clone, "", "commit", "-q", "--amend", "-m", "Extend the feature, rewritten").Must(t)
	hub.Git(clone, "", "push", "-q", "--force", "origin", "feat").Must(t)
	heads = append(heads, hub.Rev(clone, "HEAD"))
	afterEach(number)
	return number, heads
}

func TestChange_FivePushesGiveFiveVersions(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	number, heads := pushFiveVersions(t, hub, demo, func(number int) {
		t.Helper()
		if got := harness.Num(hub.Get(key, changePath(number, "")).Body["number"]); got != number {
			t.Fatalf("the change read after a push has number %d, want %d", got, number)
		}
	})

	versions := hub.Get(key, changePath(number, "/versions")).Items()
	var numbers []int
	var shas []string
	for _, v := range versions {
		numbers = append(numbers, harness.Num(v["number"]))
		shas = append(shas, harness.Str(v["head_sha"]))
		if v["kind"] != "push" {
			t.Errorf("version %v has kind %v, want push", v["number"], v["kind"])
		}
	}
	if want := []int{1, 2, 3, 4, 5}; !slices.Equal(numbers, want) {
		t.Errorf("the versions are %v, want %v", numbers, want)
	}
	if !slices.Equal(shas, heads) {
		t.Errorf("the versions' heads are %v, want the heads pushed %v", shas, heads)
	}
	if n := len(hub.Get(key, changes).Items()); n != 1 {
		t.Errorf("the workspace has %d changes, want 1", n)
	}
}

func TestChange_EveryVersionHeadIsFetchable(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	number, heads := pushFiveVersions(t, hub, demo, func(int) {})
	versions := hub.Get(key, changePath(number, "/versions")).Items()
	if len(versions) != len(heads) {
		t.Fatalf("the change has %d versions, want %d", len(versions), len(heads))
	}

	fresh := hub.Clone(demo.Remote, key)
	for i, v := range versions {
		ref := fmt.Sprintf("refs/parley/changes/%d/%d", number, i+1)
		hub.Git(fresh, "", "fetch", "-q", "origin", ref).Must(t)
		if got := hub.Rev(fresh, "FETCH_HEAD"); got != harness.Str(v["head_sha"]) || got != heads[i] {
			t.Errorf("%s fetched %s, want the head of version %d %v", ref, got, i+1, v["head_sha"])
		}
		if kind := strings.TrimSpace(hub.Git(fresh, "", "cat-file", "-t", "FETCH_HEAD").Must(t).Stdout); kind != "commit" {
			t.Errorf("%s fetched a %s, want a commit", ref, kind)
		}
	}
}

// changedFiles are the paths of the files of an interdiff that differ
// between its two versions.
func changedFiles(diff map[string]any) []string {
	files, _ := diff["files"].([]any)
	changed := []string{}
	for _, f := range files {
		file, _ := f.(map[string]any)
		if file["changed"] == true {
			changed = append(changed, harness.Str(file["path"]))
		}
	}
	return changed
}

func TestChange_PureRebaseHasEmptyInterdiff(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	clone := hub.Clone(demo.Remote, hub.PAT(demo.Owner, harness.AgentScopes))
	hub.Git(clone, "", "checkout", "-q", "-b", "feat").Must(t)
	hub.CommitFiles(clone, "Add the feature", map[string]string{"feature.txt": "feature\n"})
	hub.PushChange(clone, "", "feat", "add the feature").Must(t)
	number := harness.Num(hub.ChangeOn(key, demo.Slug, "feat")["number"])

	// main moves by a landed change that touches another file.
	hub.Git(clone, "", "checkout", "-q", "-b", "other", "origin/main").Must(t)
	hub.CommitFiles(clone, "Add another file", map[string]string{"other.txt": "other\n"})
	hub.PushChange(clone, "", "other", "add another file").Must(t)
	hub.Land(key, demo.Slug, harness.Num(hub.ChangeOn(key, demo.Slug, "other")["number"]))

	hub.Git(clone, "", "checkout", "-q", "feat").Must(t)
	hub.Git(clone, "", "fetch", "-q", "origin").Must(t)
	hub.Git(clone, "", "rebase", "-q", "origin/main").Must(t)
	hub.Git(clone, "", "push", "-q", "--force", "origin", "feat").Must(t)
	v2 := hub.Get(key, changePath(number, "/versions/latest")).Body
	if harness.Num(v2["number"]) != 2 || harness.Num(v2["patch_equivalent_to"]) != 1 {
		t.Errorf("after the rebase the latest version is %v, patch-equivalent to %v; want version 2, patch-equivalent to 1",
			v2["number"], v2["patch_equivalent_to"])
	}
	d := hub.Get(key, changePath(number, "/diff?version=2&against=1")).Body
	if changed := changedFiles(d); len(changed) != 0 || harness.Num(harness.Field(d, "stats.files_changed")) != 0 {
		t.Errorf("the interdiff of version 2 against 1 changes %v (stats %v), want nothing", changed, d["stats"])
	}

	if err := os.WriteFile(filepath.Join(clone, "feature.txt"), []byte("feature, reworded\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub.Git(clone, "", "commit", "-q", "-a", "--amend", "--no-edit").Must(t)
	hub.Git(clone, "", "push", "-q", "--force", "origin", "feat").Must(t)
	v3 := hub.Get(key, changePath(number, "/versions/latest")).Body
	if harness.Num(v3["number"]) != 3 || v3["patch_equivalent_to"] != nil {
		t.Errorf("after the edit the latest version is %v, patch-equivalent to %v; want version 3, equivalent to none",
			v3["number"], v3["patch_equivalent_to"])
	}
	d = hub.Get(key, changePath(number, "/diff?version=3&against=2")).Body
	if changed := changedFiles(d); !slices.Equal(changed, []string{"feature.txt"}) {
		t.Errorf("the interdiff of version 3 against 2 changes %v, want [feature.txt]", changed)
	}
}

func TestChange_LandedCommitResolvesToChangeVersionAndSession(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	session := harness.Str(hub.CLI(key, "session", "start", demo.Slug).Must(t).JSON()["id"])
	if session == "" {
		t.Fatal("parley session start printed no id")
	}
	clone := hub.Clone(demo.Remote, hub.PAT(demo.Owner, harness.AgentScopes))
	hub.Git(clone, "", "checkout", "-q", "-b", "feat").Must(t)
	hub.CommitFiles(clone, "Add the feature", map[string]string{"feature.txt": "feature\n"})
	hub.PushChange(clone, "", "feat", "add the feature", "parley.session="+session).Must(t)
	number := harness.Num(hub.ChangeOn(key, demo.Slug, "feat")["number"])
	// main has not moved, so the land keeps the pushed commit, version 1.
	hub.Land(key, demo.Slug, number)

	lands := hub.Get(key, changePath(number, "/lands")).Items()
	if len(lands) == 0 {
		t.Fatal("the change lists no land attempt")
	}
	sha := harness.Str(lands[0]["landed_sha"])
	if main := mainHead(t, hub, key); sha == "" || sha != main {
		t.Fatalf("the land attempt landed %q, want the head of main %q", sha, main)
	}

	byCommit := hub.Get(key, changes+"/by-commit/"+sha).Body
	if harness.Num(harness.Field(byCommit, "change.number")) != number || byCommit["landed"] != true {
		t.Errorf("by-commit answers %v, want change %d, landed", byCommit, number)
	}
	commit := hub.Get(key, "/api/v1/workspaces/demo/scm/commits/"+sha).Body
	if harness.Num(harness.Field(commit, "change.number")) != number || harness.Num(harness.Field(commit, "change.version")) != 1 {
		t.Errorf("the commit's change is %v, want change %d version 1", commit["change"], number)
	}
	if commit["session_id"] != session {
		t.Errorf("the commit's session is %v, want %s", commit["session_id"], session)
	}
	found := hub.CLI(key, "change", "find", demo.Slug, sha).Must(t).JSON()
	if harness.Num(harness.Field(found, "change.number")) != number {
		t.Errorf("parley change find prints %v, want change %d", found, number)
	}
}

func TestChange_DirectOwnerPushResolvesToNoChange(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	clone := hub.Clone(demo.Remote, key)
	hub.CommitFiles(clone, "Fix a typo on main", map[string]string{"typo.txt": "fixed\n"})
	hub.Git(clone, "", "push", "-q", "origin", "main").Must(t)
	sha := hub.Rev(clone, "HEAD")

	commit := hub.Get(key, "/api/v1/workspaces/demo/scm/commits/"+sha).Body
	if change, present := commit["change"]; !present || change != nil {
		t.Errorf("the pushed commit's change is %v (present %v), want null", change, present)
	}
	var pushes []map[string]any
	harness.Eventually(t, "the push of "+sha+" in the event log", func() bool {
		pushes = pushesTo(eventLog(t, hub, key, "workspace=demo&types=parley.git.push", 0), sha)
		return len(pushes) > 0
	})
	if len(pushes) != 1 || harness.Field(pushes[0], "metadata.ref") != "refs/heads/main" || harness.Field(pushes[0], "metadata.direct") != true {
		t.Errorf("the log holds the pushes %v to %s, want one, of refs/heads/main, direct", pushes, sha)
	}
	if open := hub.Get(key, changes+"?state=open").Items(); len(open) != 0 {
		t.Errorf("the workspace has open changes %v, want none", open)
	}
}
