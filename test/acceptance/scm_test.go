package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// scm is the path of demo's source control routes.
const scm = "/api/v1/workspaces/demo/scm"

// scmHub starts a hub with alice signed in and the workspace demo she owns,
// registered from an origin whose main got, after its seeding, three more
// commits that touch src/a.txt and docs/b.md. It returns the ids of those
// three commits, oldest first.
func scmHub(t *testing.T) (*harness.Hub, *harness.Workspace, []string) {
	t.Helper()
	hub := harness.Start(t)
	origin := hub.Origin()
	work := filepath.Join(t.TempDir(), "work")
	hub.Git("", "", "clone", "-q", origin.Dir(), work).Must(t)
	var seeded []string
	for _, files := range []map[string]string{
		{"src/a.txt": "alpha\nbeta\ngamma\n", "src/sub/c.txt": "c\n", "docs/b.md": "# B\n"},
		{"src/a.txt": "alpha\nBETA\ngamma\ndelta\n"},
		{"src/a.txt": "alpha\nBETA\ngamma\ndelta\nepsilon\n", "docs/b.md": "# B\n\nMore.\n"},
	} {
		hub.CommitFiles(work, fmt.Sprintf("Seed step %d", len(seeded)+1), files)
		seeded = append(seeded, hub.Rev(work, "HEAD"))
	}
	hub.Git(work, "", "push", "-q", "origin", "main").Must(t)
	origin.Publish()
	origin.Main = seeded[len(seeded)-1]
	return hub, hub.Register(hub.User("alice"), "demo", origin, nil), seeded
}

// decode is raw decoded as a JSON object, failing the test when it is not
// one.
func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not a JSON object (%v): %s", err, raw)
	}
	return m
}

// members are the member name, a dotted path, of every element of the
// array at path of m, in order, as text: a number in decimal, "" for an
// element without the member. With name "" they are the elements
// themselves. They are nil when m holds no array at path.
func members(m map[string]any, path, name string) []string {
	raw, ok := harness.Field(m, path).([]any)
	if !ok {
		return nil
	}
	out := []string{}
	for _, it := range raw {
		v := it
		if name != "" {
			obj, _ := it.(map[string]any)
			v = harness.Field(obj, name)
		}
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case float64:
			out = append(out, strconv.FormatFloat(x, 'f', -1, 64))
		default:
			out = append(out, "")
		}
	}
	return out
}

// sorted is a sorted copy of s.
func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

// blameLines is the commit id of each line of a blame, in line order: the
// hub answers with ranges of lines.
func blameLines(m map[string]any) []string {
	ranges, _ := m["ranges"].([]any)
	out := []string{}
	for _, it := range ranges {
		r, _ := it.(map[string]any)
		for line := harness.Num(r["start_line"]); line <= harness.Num(r["end_line"]); line++ {
			out = append(out, harness.Str(harness.Field(r, "commit.sha")))
		}
	}
	return out
}

func TestSCM_ReadsMatchTheRepository(t *testing.T) {
	t.Parallel()
	hub, demo, seeded := scmHub(t)
	key := demo.Owner.Key
	c1, c3 := seeded[0], seeded[2]
	clone := hub.Clone(demo.Remote, key)

	file, err := os.ReadFile(filepath.Join(clone, "src", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var blamed []string
	for _, line := range hub.Git(clone, "", "blame", "-l", "main", "--", "src/a.txt").Must(t).Lines() {
		sha, _, _ := strings.Cut(line, " ")
		blamed = append(blamed, strings.TrimPrefix(sha, "^"))
	}
	for _, read := range []struct {
		name string
		path string
		cli  []string
		got  func(raw []byte) []string
		want []string
	}{
		{
			"file", scm + "/file?path=src/a.txt&ref=main",
			[]string{"scm", "cat", "demo", "src/a.txt", "--ref", "main"},
			func(raw []byte) []string { return []string{string(raw)} },
			[]string{string(file)},
		},
		{
			"tree", scm + "/tree?ref=main&path=src",
			[]string{"scm", "ls", "demo", "src", "--ref", "main"},
			func(raw []byte) []string { return sorted(members(decode(t, raw), "items", "path")) },
			sorted(hub.Git(clone, "", "ls-tree", "--name-only", "main", "src/").Must(t).Lines()),
		},
		{
			"history", scm + "/commits?path=src/a.txt&ref=main",
			[]string{"scm", "log", "demo", "--ref", "main", "--path", "src/a.txt"},
			func(raw []byte) []string { return members(decode(t, raw), "items", "sha") },
			hub.Git(clone, "", "log", "--format=%H", "main", "--", "src/a.txt").Must(t).Lines(),
		},
		{
			"diff", scm + "/diff?base=" + c1 + "&head=" + c3,
			[]string{"scm", "diff", "demo", c1, c3, "--name-only"},
			func(raw []byte) []string { return sorted(members(decode(t, raw), "files", "path")) },
			sorted(hub.Git(clone, "", "diff", "--name-only", c1, c3).Must(t).Lines()),
		},
		{
			"blame", scm + "/blame?path=src/a.txt&ref=main",
			[]string{"scm", "blame", "demo", "src/a.txt", "--ref", "main"},
			func(raw []byte) []string { return blameLines(decode(t, raw)) },
			blamed,
		},
	} {
		if len(read.want) == 0 || read.want[0] == "" {
			t.Fatalf("stock git reports nothing for the %s", read.name)
		}
		if got := read.got(hub.Get(key, read.path).Raw); !slices.Equal(got, read.want) {
			t.Errorf("the API's %s is %q, stock git's %q", read.name, got, read.want)
		}
		if got := read.got([]byte(hub.CLI(key, read.cli...).Must(t).Stdout)); !slices.Equal(got, read.want) {
			t.Errorf("parley %s prints the %s %q, stock git reports %q", strings.Join(read.cli, " "), read.name, got, read.want)
		}
	}
}

// put is the operation that writes content to path.
func put(path, content string) map[string]any {
	return map[string]any{"op": "put", "path": path, "content": content}
}

func TestSCM_CommitOfSeveralFiles(t *testing.T) {
	t.Parallel()
	hub, demo, _ := scmHub(t)
	key := demo.Owner.Key
	main := mainHead(t, hub, key)

	r := hub.Call(http.MethodPost, scm+"/commits", key, map[string]any{
		"branch": "api-work", "base_ref": "main", "message": "m",
		"operations": []any{
			put("x/1.txt", "one\n"), put("x/2.txt", "two\n"), put("src/a.txt", "rewritten\n"),
			map[string]any{"op": "delete", "path": "docs/b.md"},
		},
	}).Want(t, http.StatusCreated)
	sha := harness.Str(harness.Field(r.Body, "commit.sha"))
	if ref := hub.Get(key, scm+"/refs/api-work").Body["sha"]; sha == "" || ref != sha {
		t.Errorf("api-work points at %v, want the new commit %q", ref, sha)
	}
	commit := hub.Get(key, scm+"/commits/"+sha).Body
	if parents, _ := commit["parents"].([]any); len(parents) != 1 || parents[0] != main {
		t.Errorf("the commit's parents are %v, want [%s], the head of main", commit["parents"], main)
	}
	diff := hub.Get(key, scm+"/diff?base=main&head=api-work").Body
	if got, want := sorted(members(diff, "files", "path")), []string{"docs/b.md", "src/a.txt", "x/1.txt", "x/2.txt"}; !slices.Equal(got, want) {
		t.Errorf("the commit changes %v, want %v", got, want)
	}
}

func TestSCM_StaleExpectedHeadIsRefMoved(t *testing.T) {
	t.Parallel()
	hub, demo, _ := scmHub(t)
	key := demo.Owner.Key
	commit := func(body map[string]any) string {
		t.Helper()
		r := hub.Call(http.MethodPost, scm+"/commits", key, body).Want(t, http.StatusCreated)
		return harness.Str(harness.Field(r.Body, "commit.sha"))
	}
	c1 := commit(map[string]any{"branch": "api-work", "base_ref": "main", "message": "first", "operations": []any{put("w1.txt", "1\n")}})
	c2 := commit(map[string]any{"branch": "api-work", "expected_head_sha": c1, "message": "second", "operations": []any{put("w2.txt", "2\n")}})
	unmoved := func(after string) {
		t.Helper()
		if head := hub.Get(key, scm+"/refs/api-work").Body["sha"]; head != c2 {
			t.Errorf("after %s api-work points at %v, want %s", after, head, c2)
		}
	}

	r := hub.Call(http.MethodPost, scm+"/commits", key, map[string]any{
		"branch": "api-work", "expected_head_sha": c1, "message": "late", "operations": []any{put("y.txt", "y\n")},
	})
	if r.Status != http.StatusConflict || r.ErrorType() != "ref_moved" || harness.Field(r.Body, "error.details.current_sha") != c2 {
		t.Errorf("the commit on a stale head answered %d %s, want 409 ref_moved with the current head %s", r.Status, r.Raw, c2)
	}
	unmoved("the API's refusal")

	local := filepath.Join(t.TempDir(), "y.txt")
	if err := os.WriteFile(local, []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cli := hub.CLI(key, "scm", "commit", "demo", "--branch", "api-work", "--message", "late", "--expected-head", c1, "--put", "y.txt="+local)
	var stderr map[string]any
	_ = json.Unmarshal([]byte(cli.Stderr), &stderr)
	if cli.Code != 1 || harness.Field(stderr, "error.error_type") != "ref_moved" {
		t.Errorf("parley scm commit on a stale head exited %d with %s, want 1 and ref_moved", cli.Code, cli.Stderr)
	}
	unmoved("the CLI's refusal")
}

// landWithoutAClone commits n/1.txt and n/2.txt on the new branch w of
// demo, opens a change of w and lands it, through the REST API alone.
func landWithoutAClone(t *testing.T, hub *harness.Hub, demo *harness.Workspace) {
	t.Helper()
	key := demo.Owner.Key
	hub.Call(http.MethodPost, scm+"/commits", key, map[string]any{
		"branch": "w", "base_ref": "main", "message": "Add n",
		"operations": []any{put("n/1.txt", "one"), put("n/2.txt", "two")},
	}).Want(t, http.StatusCreated)
	c := hub.Call(http.MethodPost, changes, key, map[string]any{
		"source": map[string]any{"branch": "w"}, "title": "t", "intent": "add n without a clone",
	}).Want(t, http.StatusCreated)
	if l := hub.Land(key, demo.Slug, harness.Num(c.Body["number"])); l.Body["status"] != "landed" {
		t.Fatalf("the land answered %s, want the status landed", l.Raw)
	}
}

func TestSCM_OpenAndLandAChangeWithoutAClone(t *testing.T) {
	t.Parallel()
	hub, demo, _ := scmHub(t)
	landWithoutAClone(t, hub, demo)
	for path, want := range map[string]string{"n/1.txt": "one", "n/2.txt": "two"} {
		if got := hub.Get(demo.Owner.Key, scm+"/file?path="+path+"&ref=main").Raw; string(got) != want {
			t.Errorf("%s on main holds %q, want %q", path, got, want)
		}
	}
}

func TestSCM_ResultEqualsEquivalentGitOperations(t *testing.T) {
	t.Parallel()
	hub, demo, _ := scmHub(t)
	landWithoutAClone(t, hub, demo)

	local := filepath.Join(t.TempDir(), "local")
	hub.Git("", "", "clone", "-q", demo.Origin.URL(), local).Must(t)
	hub.CommitFiles(local, "Add n", map[string]string{"n/1.txt": "one", "n/2.txt": "two"})
	want := hub.Git(local, "", "rev-parse", "HEAD^{tree}").Must(t).Lines()

	fresh := hub.Clone(demo.Remote, demo.Owner.Key)
	if got := hub.Git(fresh, "", "rev-parse", "origin/main^{tree}").Must(t).Lines(); len(want) != 1 || !slices.Equal(got, want) {
		t.Errorf("main's tree on the hub is %v, the tree of the same operations committed with git %v", got, want)
	}
	if r := hub.Git(fresh, "", "fsck", "--strict"); r.Code != 0 {
		t.Errorf("git fsck --strict of a fresh clone exited %d: %s", r.Code, r.Stderr)
	}
	if parents := strings.Fields(hub.Git(fresh, "", "log", "--format=%P", "-1", "origin/main").Must(t).Stdout); len(parents) != 1 {
		t.Errorf("main's head has the parents %v, want one", parents)
	}
}
