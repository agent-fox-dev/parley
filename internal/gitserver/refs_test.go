package gitserver_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/hubtest"
)

func TestRefs_ListsOnlyTheRefsOfTheAskedPrefixes(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	w.extendHistory(t)
	body := pkt("command=ls-refs\n") + "0001" + pkt("ref-prefix refs/heads/ma\n") + "0000"
	a := w.send(t, http.MethodPost, uploadPack, basic("x", w.ownerKey), http.Header{"Git-Protocol": {"version=2"}}, body)
	var names []string
	for _, p := range pktlines(a.body) {
		if fields := strings.Fields(p); len(fields) >= 2 {
			names = append(names, fields[1])
		}
	}
	if a.status != http.StatusOK || !slices.Equal(names, []string{"refs/heads/main"}) {
		t.Errorf("ls-refs = %d %q, want 200 listing refs/heads/main alone", a.status, a.body)
	}
}

func TestRefs_SendsWhatARefReachesByNameOrIdAndRefusesTheRest(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	h := w.extendHistory(t)
	remote := w.remote(w.ownerKey, "acme/demo.git")
	for _, c := range []struct {
		name, what, id string
		served         bool
	}{
		{"a hub-owned ref by its name", "refs/parley/changes/1/1", h.change, true},
		{"the id of a commit inside main", h.main[2], h.main[2], true},
		{"the id of a commit no ref reaches", h.dangling, h.dangling, false},
	} {
		for _, version := range []string{"0", "2"} {
			t.Run(c.name+"/protocol "+version, func(t *testing.T) {
				t.Parallel()
				d := t.TempDir()
				runGit(t, d, "init", "--quiet")
				out, err := clientGit(t, d, nil, "-c", "protocol.version="+version, "fetch", remote, c.what)
				switch {
				case c.served && err != nil:
					t.Fatalf("git fetch %s: %v\n%s", c.what, err, out)
				case c.served:
					if got, _ := clientGit(t, d, nil, "rev-parse", "FETCH_HEAD"); strings.TrimSpace(got) != c.id {
						t.Errorf("FETCH_HEAD = %s, want %s", got, c.id)
					}
				case err == nil || !strings.Contains(out, "remote error"):
					t.Errorf("git fetch %s = %v, want a failure with a remote error:\n%s", c.what, err, out)
				}
				if _, err := clientGit(t, d, nil, "cat-file", "-e", c.id); (err == nil) != c.served {
					t.Errorf("the client holds %s = %v, want %v", c.id, err == nil, c.served)
				}
			})
		}
	}
}
