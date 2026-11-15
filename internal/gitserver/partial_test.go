package gitserver_test

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

func TestPartial_LeavesOutWhatTheFilterExcludesAndSendsItOnDemand(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	h := w.extendHistory(t)
	types := map[string]string{}
	for line := range strings.Lines(gitrepotest.Git(t, w.repo, "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype)")) {
		id, typ, _ := strings.Cut(strings.TrimSpace(line), " ")
		types[id] = typ
	}
	for _, c := range []struct {
		filter string
		// missing reports whether the clone is to lack the object id of
		// type typ.
		missing func(id, typ string) bool
	}{
		{"blob:none", func(_, typ string) bool { return typ == "blob" }},
		{"blob:limit=1k", func(id, _ string) bool { return id == h.big }},
		{"tree:0", func(_, typ string) bool { return typ == "tree" }},
	} {
		for _, version := range []string{"0", "2"} {
			t.Run(c.filter+"/protocol "+version, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				d := filepath.Join(dir, "d")
				v := []string{"-c", "protocol.version=" + version}
				runGit(t, dir, append(v, "clone", "--no-checkout", "--filter="+c.filter, w.remote(w.ownerKey, "acme/demo.git"), "d")...)
				out := runGit(t, d, "rev-list", "--objects", "--missing=print", "HEAD")
				var lacks int
				for line := range strings.Lines(out) {
					id, _, _ := strings.Cut(strings.TrimSpace(line), " ")
					id, missing := strings.CutPrefix(id, "?")
					if missing {
						lacks++
					}
					if missing != c.missing(id, types[id]) {
						t.Errorf("the clone lacks the %s %s = %v, want %v", types[id], id, missing, !missing)
					}
				}
				if lacks == 0 {
					t.Errorf("the clone lacks no object:\n%s", out)
				}
				runGit(t, d, append(v, "checkout", "--quiet", "main")...)
				if got, err := os.ReadFile(filepath.Join(d, "big.bin")); err != nil || sha256.Sum256(got) != h.bigSum {
					t.Errorf("big.bin of the checkout (%v) is not the content committed", err)
				}
				if out, err := clientGit(t, d, nil, "cat-file", "-e", h.big); err != nil {
					t.Errorf("the clone lacks big.bin after the checkout: %v\n%s", err, out)
				}
			})
		}
	}
}

// git shows a remote error only for an ERR packet in an answer of 200: it
// reports any other status as the status.
func TestPartial_RefusesAnotherFilterKindInBand(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	readme := gitrepotest.Git(t, w.repo, "rev-parse", "main:README")
	for _, c := range []struct{ filter, kind string }{
		{"object:type=commit", "object:type"},
		{"sparse:oid=" + readme, "sparse:oid"},
		{"combine:blob:none+tree:0", "combine"},
	} {
		for _, version := range []string{"0", "2"} {
			t.Run(c.kind+"/protocol "+version, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				out, err := clientGit(t, dir, nil, "-c", "protocol.version="+version, "clone", "--no-checkout", "--filter="+c.filter, w.remote(w.ownerKey, "acme/demo.git"), "d")
				if err == nil || !strings.Contains(out, "remote error") || !strings.Contains(out, "'"+c.kind+"'") {
					t.Errorf("git clone = %v, want a failure with a remote error naming %s:\n%s", err, c.kind, out)
				}
				if _, err := os.Stat(filepath.Join(dir, "d")); !os.IsNotExist(err) {
					t.Errorf("the failed clone left its destination behind (%v)", err)
				}
			})
		}
	}
}
