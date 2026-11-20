package acceptance

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// layer is one branch of a stack: its name and the files its one commit
// writes.
type layer struct {
	branch string
	files  map[string]string
}

// pushStack builds each layer's branch on the one before, the first on
// main, in a clone made with the API key of demo's owner, and pushes them
// all in one git push that asks for changes. It returns the clone and the
// changes' numbers, bottom first, after checking that each depends on the
// one below.
func pushStack(t *testing.T, hub *harness.Hub, demo *harness.Workspace, layers ...layer) (string, []int) {
	t.Helper()
	clone := hub.Clone(demo.Remote, demo.Owner.Key)
	args := []string{"push", "-q", "-o", "parley.change", "-o", "parley.intent=build the stack", "origin"}
	for _, l := range layers {
		hub.Git(clone, "", "checkout", "-q", "-b", l.branch).Must(t)
		hub.CommitFiles(clone, "Add "+l.branch, l.files)
		args = append(args, l.branch)
	}
	hub.Git(clone, "", args...).Must(t)
	var numbers []int
	for i, l := range layers {
		c := hub.ChangeOn(demo.Owner.Key, demo.Slug, l.branch)
		numbers = append(numbers, harness.Num(c["number"]))
		if i > 0 && harness.Num(c["depends_on"]) != numbers[i-1] {
			t.Fatalf("the change of %s depends on %v, want %d", l.branch, c["depends_on"], numbers[i-1])
		}
	}
	return clone, numbers
}

// amend rewrites the head commit of branch in the clone with the files and
// force-pushes it, and returns the new head.
func amend(t *testing.T, hub *harness.Hub, clone, branch string, files map[string]string) string {
	t.Helper()
	hub.Git(clone, "", "checkout", "-q", branch).Must(t)
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(clone, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hub.Git(clone, "", "commit", "-q", "-a", "--amend", "--no-edit").Must(t)
	hub.Git(clone, "", "push", "-q", "--force", "origin", branch).Must(t)
	return hub.Rev(clone, "HEAD")
}

func TestStack_NewVersionBelowRestacksDependants(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	clone, n := pushStack(t, hub, demo,
		layer{"a", map[string]string{"a.txt": "a\n"}},
		layer{"b", map[string]string{"b.txt": "b\n"}},
		layer{"c", map[string]string{"c.txt": "c\n"}})
	var stack []int
	members, _ := hub.Get(key, changePath(n[0], "/stack")).Body["changes"].([]any)
	for _, c := range members {
		stack = append(stack, harness.Num(c.(map[string]any)["number"]))
	}
	if !slices.Equal(stack, n) {
		t.Fatalf("the stack of %d is %v, want %v", n[0], stack, n)
	}

	headA := amend(t, hub, clone, "a", map[string]string{"a.txt": "a, amended\n"})
	// From here on the scenario only reads: the restacks are the hub's own.
	harness.Eventually(t, "the top change to be restacked", func() bool {
		return hub.Get(key, changePath(n[2], "/versions/latest")).Body["kind"] == "restack"
	})
	vb := hub.Get(key, changePath(n[1], "/versions/latest")).Body
	vc := hub.Get(key, changePath(n[2], "/versions/latest")).Body
	for _, v := range []map[string]any{vb, vc} {
		if v["kind"] != "restack" || harness.Field(v, "created_by.actor_type") != "system" {
			t.Errorf("a dependant's latest version is %v, want a restack by the system", v)
		}
	}
	hub.Git(clone, "", "fetch", "-q", "origin").Must(t)
	hub.Git(clone, "", "merge-base", "--is-ancestor", headA, harness.Str(vb["head_sha"])).Must(t)
	hub.Git(clone, "", "merge-base", "--is-ancestor", harness.Str(vb["head_sha"]), harness.Str(vc["head_sha"])).Must(t)
	for i := 1; i < 3; i++ {
		if got := harness.Num(hub.Get(key, changePath(n[i], "")).Body["depends_on"]); got != n[i-1] {
			t.Errorf("change %d depends on %d after the restack, want %d", n[i], got, n[i-1])
		}
	}
}

func TestStack_LandingBottomRebasesNextOntoTarget(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	clone, n := pushStack(t, hub, demo,
		layer{"a", map[string]string{"a.txt": "a\n"}},
		layer{"b", map[string]string{"b.txt": "b\n"}},
		layer{"c", map[string]string{"c.txt": "c\n"}})
	// The owner moves main past the stack's base, so that the land rewrites
	// the bottom change and the next one has something to be rebased onto.
	hub.Git(clone, "", "checkout", "-q", "main").Must(t)
	hub.CommitFiles(clone, "Add an unrelated file", map[string]string{"unrelated.txt": "unrelated\n"})
	hub.Git(clone, "", "push", "-q", "origin", "main").Must(t)

	if r := hub.Land(key, demo.Slug, n[0]); r.Body["status"] != "landed" {
		t.Fatalf("the land answered %s, want landed", r.Raw)
	}
	main := harness.Str(hub.Get(key, "/api/v1/workspaces/demo/scm/refs/main").Body["sha"])
	harness.Eventually(t, "the next change to be rebased onto main", func() bool {
		b := hub.Get(key, changePath(n[1], "")).Body
		return b["depends_on"] == nil && hub.Get(key, changePath(n[1], "/versions/latest")).Body["kind"] == "restack"
	})
	if b := hub.Get(key, changePath(n[1], "")).Body; b["needs_restack"] != false {
		t.Errorf("change %d needs a restack: %v", n[1], b)
	}
	if v := hub.Get(key, changePath(n[1], "/versions/latest")).Body; v["base_sha"] != main {
		t.Errorf("change %d's restack is based on %v, want the head of main %s", n[1], v["base_sha"], main)
	}
	if got := harness.Num(hub.Get(key, changePath(n[2], "")).Body["depends_on"]); got != n[1] {
		t.Errorf("change %d depends on %d, want %d", n[2], got, n[1])
	}
}

func TestStack_ConflictingRestackMovesNothing(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	clone, n := pushStack(t, hub, demo,
		layer{"a", map[string]string{"shared.txt": "line 1\nline 2\n"}},
		layer{"b", map[string]string{"shared.txt": "line 1, by b\nline 2\n"}})
	headB := func() string {
		return harness.Str(hub.Get(key, "/api/v1/workspaces/demo/scm/refs/b").Body["sha"])
	}
	beforeHead, beforeVersion := headB(), harness.Num(hub.Get(key, changePath(n[1], "")).Body["version"])

	amend(t, hub, clone, "a", map[string]string{"shared.txt": "line 1, by a\nline 2\n"})
	harness.Eventually(t, "the dependant to need a restack", func() bool {
		return hub.Get(key, changePath(n[1], "")).Body["needs_restack"] == true
	})
	b := hub.Get(key, changePath(n[1], "")).Body
	if b["restack_reason"] != "conflict" || b["mergeable"] != "conflict" {
		t.Errorf("the dependant has restack_reason %v and mergeable %v, want conflict and conflict", b["restack_reason"], b["mergeable"])
	}
	report := hub.Get(key, changePath(n[1], "/conflict")).Body
	var paths []string
	files, _ := harness.Field(report, "conflict.files").([]any)
	for _, f := range files {
		paths = append(paths, harness.Str(f.(map[string]any)["path"]))
	}
	if report["mergeable"] != "conflict" || !slices.Contains(paths, "shared.txt") {
		t.Errorf("the conflict report is %v, want mergeable conflict on shared.txt", report)
	}
	if head, version := headB(), harness.Num(b["version"]); head != beforeHead || version != beforeVersion {
		t.Errorf("the dependant's branch is at %s, version %d; want them unmoved at %s, version %d", head, version, beforeHead, beforeVersion)
	}
}
