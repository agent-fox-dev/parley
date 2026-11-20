package acceptance

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// conflictMarkers are the lines git writes around and between the two
// sides of a conflict.
var conflictMarkers = []string{"<<<<<<<", "=======", ">>>>>>>"}

// resolve submits content as src/a.txt of change number of demo with
// parley change resolve, run with credential, and fails the test unless it
// exits 0.
func resolve(t *testing.T, hub *harness.Hub, credential string, number int, content string) {
	t.Helper()
	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hub.CLI(credential, "change", "resolve", "demo", strconv.Itoa(number), "--file", "src/a.txt="+local).Must(t)
}

// landWithCLI lands change number of demo with parley change land, run
// with credential, and fails the test unless it lands.
func landWithCLI(t *testing.T, hub *harness.Hub, credential string, number int) {
	t.Helper()
	l := hub.CLI(credential, "change", "land", "demo", strconv.Itoa(number))
	if l.Code != 0 || l.JSON()["status"] != "landed" {
		t.Fatalf("parley change land %d exited %d with %s%s, want 0 and the status landed", number, l.Code, l.Stdout, l.Stderr)
	}
}

// Every step goes through the REST API or the parley binary: resolving a
// conflict needs no clone.
func TestConflict_ResolveWithoutACloneThenLand(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	seedFiveLines(t, hub, key)
	p := openChange(t, hub, key, "p", map[string]string{"src/a.txt": lineThree("line 3 from p")})
	q := openChange(t, hub, key, "q", map[string]string{"src/a.txt": lineThree("line 3 from q")})
	n := raceToLand(t, hub, p, key, q, key).loser

	f := hub.Get(key, changePath(n, "/conflict/file?path=src/a.txt"))
	if ct := f.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("the conflicted file is served as %q, want text/plain", ct)
	}
	for _, marker := range conflictMarkers {
		if !strings.Contains(string(f.Raw), marker) {
			t.Errorf("the conflicted file %q lacks the marker %s", f.Raw, marker)
		}
	}

	previous := harness.Num(hub.Get(key, changePath(n, "")).Body["version"])
	resolved := lineThree("line 3 from p and q")
	resolve(t, hub, key, n, resolved)
	c := hub.Get(key, changePath(n, "")).Body
	if harness.Num(c["version"]) != previous+1 || c["needs_restack"] != false {
		t.Errorf("after the resolution the change has version %v and needs_restack %v, want %d and false", c["version"], c["needs_restack"], previous+1)
	}
	harness.Eventually(t, "the resolved change to be mergeable clean", func() bool {
		return hub.Get(key, changePath(n, "")).Body["mergeable"] == "clean"
	})
	landWithCLI(t, hub, key, n)
	if body := string(hub.Get(key, scm+"/file?path=src/a.txt&ref=main").Raw); body != resolved {
		t.Errorf("src/a.txt on main holds %q, want the resolution %q", body, resolved)
	}
}
