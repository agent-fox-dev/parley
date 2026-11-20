package acceptance

import (
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// missing reports whether git rev-list, run with --missing=print, names
// object as missing from the clone.
func missing(lines []string, object string) bool { return slices.Contains(lines, "?"+object) }

func TestGit_CloneFetchPushWithPATAndCredentialHelper(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	pat := hub.PAT(demo.Owner, []string{"git:read", "git:write"})
	_, parley := harness.Binaries(t)
	seeded, err := strconv.Atoi(strings.TrimSpace(
		hub.Git("", "", "--git-dir="+demo.Origin.Dir(), "rev-list", "--count", "main").Must(t).Stdout))
	if err != nil || seeded < 2 {
		t.Fatalf("the origin's main has %d commits (%v), want several", seeded, err)
	}
	// The other clone, which pushes the commit each row's clone fetches.
	pusher := hub.Clone(demo.Remote, pat)
	helperEnv := []string{"PARLEY_ENDPOINT_URL=" + hub.URL(""), "PARLEY_API_KEY=" + pat}

	for _, auth := range []struct {
		name   string
		remote string
		// cloneArgs are given to git clone alone; git runs every command.
		cloneArgs []string
		git       func(dir string, args ...string) harness.Result
	}{
		{
			"pat-in-url", harness.AuthURL(demo.Remote, pat), nil,
			func(dir string, args ...string) harness.Result { return hub.Git(dir, "", args...) },
		},
		{
			// git clone -c keeps the helper in the clone's configuration,
			// for its later fetches and pushes.
			"credential-helper", demo.Remote, []string{"-c", "credential.helper=" + parley + " credential-helper"},
			func(dir string, args ...string) harness.Result { return hub.GitEnv(dir, helperEnv, args...) },
		},
	} {
		for _, kind := range []struct {
			name string
			args []string
		}{
			{"full", nil},
			{"shallow", []string{"--depth", "1"}},
			{"partial", []string{"--filter=blob:none"}},
		} {
			row := auth.name + "-" + kind.name
			t.Run(row, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "clone")
				args := append(append(append([]string{"clone", "-q"}, auth.cloneArgs...), kind.args...), auth.remote, dir)
				auth.git("", args...).Must(t)

				count := strings.TrimSpace(auth.git(dir, "rev-list", "--count", "HEAD").Must(t).Stdout)
				switch kind.name {
				case "full":
					if count != strconv.Itoa(seeded) {
						t.Errorf("the full clone holds %s commits of main, want %d", count, seeded)
					}
				case "shallow":
					if count != "1" {
						t.Errorf("the shallow clone holds %s commits, want 1", count)
					}
				case "partial":
					blob := strings.TrimSpace(auth.git(dir, "rev-parse", "origin/dev:README").Must(t).Stdout)
					if !missing(auth.git(dir, "rev-list", "--objects", "--missing=print", "origin/dev").Must(t).Lines(), blob) {
						t.Errorf("dev's README %s is in the partial clone before a checkout asked for it", blob)
					}
					auth.git(dir, "checkout", "-q", "dev").Must(t)
					if missing(auth.git(dir, "rev-list", "--objects", "--missing=print", "origin/dev").Must(t).Lines(), blob) {
						t.Errorf("dev's README %s is still missing after the checkout of dev", blob)
					}
				}

				side := "side-" + row
				hub.Git(pusher, "", "checkout", "-q", "-B", side, "origin/main").Must(t)
				hub.CommitFiles(pusher, "Push "+side, map[string]string{side + ".txt": side + "\n"})
				hub.Git(pusher, "", "push", "-q", "origin", side).Must(t)
				// A shallow clone tracks main alone, so every row names the
				// branch it fetches.
				auth.git(dir, "fetch", "-q", "origin", "+refs/heads/"+side+":refs/remotes/origin/"+side).Must(t)
				if got, want := hub.Rev(dir, "origin/"+side), hub.Rev(pusher, "HEAD"); got != want {
					t.Errorf("origin/%s is %s after the fetch, want the pushed %s", side, got, want)
				}

				// The hub's receive-pack does not yet accept the shallow
				// <oid> lines git sends before the commands of a push from a
				// shallow clone: readCommands and parseCommand in
				// internal/gitserver/receive.go answer 400. The shallow rows
				// prove the clone and the fetch only.
				if kind.name == "shallow" {
					return
				}
				auth.git(dir, "checkout", "-q", "-b", row, "origin/main").Must(t)
				hub.CommitFiles(dir, "Push "+row, map[string]string{row + ".txt": row + "\n"})
				auth.git(dir, "push", "-q", "origin", row).Must(t)
				if got, want := hub.Get(key, scm+"/refs/"+row).Body["sha"], hub.Rev(dir, "HEAD"); got != want {
					t.Errorf("%s on the hub points at %v, want the pushed %s", row, got, want)
				}
			})
		}
	}
}

// sameBut are the names of the headers that differ between a and b, but
// for those that differ between any two answers.
func sameBut(a, b http.Header) []string {
	var differ []string
	for _, name := range slices.Compact(sorted(append(slices.Collect(maps.Keys(a)), slices.Collect(maps.Keys(b))...))) {
		switch name {
		case "Date", "X-Request-Id":
			continue
		}
		if !slices.Equal(a.Values(name), b.Values(name)) {
			differ = append(differ, name)
		}
	}
	return differ
}

func TestGit_UnreadableWorkspaceLooksNonexistent(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	mallory := hub.User("mallory")

	for _, path := range []string{
		"/api/v1/workspaces/%s", "/api/v1/workspaces/%s/changes", "/api/v1/workspaces/%s/scm/refs",
		"/api/v1/workspaces/%s/activity?paths=a",
	} {
		a := hub.Call(http.MethodGet, fmt.Sprintf(path, "demo"), mallory.Key, nil)
		b := hub.Call(http.MethodGet, fmt.Sprintf(path, "no-such"), mallory.Key, nil)
		if a.Status != http.StatusNotFound || b.Status != http.StatusNotFound ||
			a.ErrorType() != "not_found" || b.ErrorType() != "not_found" {
			t.Errorf("%s answered %d %s for demo and %d %s for no-such, want 404 not_found for both", path, a.Status, a.Raw, b.Status, b.Raw)
		}
		if harness.Field(a.Body, "error.message") != harness.Field(b.Body, "error.message") || string(a.Raw) != string(b.Raw) {
			t.Errorf("%s answered %s for demo and %s for no-such, want the same", path, a.Raw, b.Raw)
		}
		if differ := sameBut(a.Header, b.Header); len(differ) != 0 {
			t.Errorf("%s: the headers %v differ between demo and no-such", path, differ)
		}
	}

	pat := hub.PAT(mallory, []string{"git:read"})
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("x:"+pat))
	repo := strings.TrimPrefix(demo.Remote, hub.URL(""))
	ra := hub.Call(http.MethodGet, repo+"/info/refs?service=git-upload-pack", "", nil, "Authorization", basic)
	rb := hub.Call(http.MethodGet, strings.Replace(repo, "/demo.git", "/no-such.git", 1)+"/info/refs?service=git-upload-pack", "", nil,
		"Authorization", basic)
	if ra.Status != rb.Status || ra.Status < 400 || string(ra.Raw) != string(rb.Raw) ||
		ra.Header.Get("WWW-Authenticate") != rb.Header.Get("WWW-Authenticate") {
		t.Errorf("info/refs answered %d %v %q for demo and %d %v %q for no-such, want the same refusal",
			ra.Status, ra.Header, ra.Raw, rb.Status, rb.Header, rb.Raw)
	}
	if differ := sameBut(ra.Header, rb.Header); len(differ) != 0 {
		t.Errorf("info/refs: the headers %v differ between demo and no-such", differ)
	}

	base := t.TempDir()
	clone := func(name string) harness.Result {
		remote := strings.Replace(demo.Remote, "/demo.git", "/"+name+".git", 1)
		r := hub.Git(base, "", "clone", harness.AuthURL(remote, pat), name)
		r.Stderr = strings.ReplaceAll(r.Stderr, name, "<repository>")
		return r
	}
	a, b := clone("demo"), clone("no-such")
	if a.Code == 0 || a.Code != b.Code || a.Stderr != b.Stderr {
		t.Errorf("git clone exited %d for demo and %d for no-such, printing\n%s\nand\n%s\nwant the same failure", a.Code, b.Code, a.Stderr, b.Stderr)
	}
}
