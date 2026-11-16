package land_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/land"
)

// What a commit of a change becomes on the target: replayed onto it,
// kept as it is, or dropped since the target holds its content.
const (
	replayed = "replayed"
	kept     = "kept"
	dropped  = "dropped"
)

// ownCommits are a change's commits, oldest first: c1 by Ann adds c1.txt,
// c2 by Bob adds c2.txt.
var ownCommits = []struct {
	author, message string
	files           map[string]string
}{
	{"Ann <ann@example.org> 1600000000 +0200", "c1", map[string]string{"c1.txt": "c1\n"}},
	{"Bob <bob@example.org> 1600000300 -0500", "c2", map[string]string{"c2.txt": "c2\n"}},
}

// replays are lands of the own commits onto main.
var replays = []struct {
	name string
	// identity is the landing identity the engine is given, the default
	// for the zero value, and committer what each replayed commit then
	// names.
	identity  land.Committer
	committer string
	// target is what main's one new commit sets on top of base; the
	// change's branch is made on that commit with onTarget, else on base.
	target   map[string]string
	onTarget bool
	// became is what each commit of the change becomes.
	became []string
}{
	{"on a moved target, by the default identity", land.Committer{}, "Parley <parley@localhost>",
		map[string]string{"b.txt": "b\n"}, false, []string{replayed, replayed}},
	{"on a moved target, by the configured identity", land.Committer{Name: "Hub", Email: "hub@example.org"},
		"Hub <hub@example.org>", map[string]string{"b.txt": "b\n"}, false, []string{replayed, replayed}},
	{"sitting on the target head", land.Committer{}, "", map[string]string{"b.txt": "b\n"}, true,
		[]string{kept, kept}},
	{"whose first commit's content is in the target", land.Committer{}, "Parley <parley@localhost>",
		map[string]string{"c1.txt": "c1\n"}, false, []string{dropped, replayed}},
	{"whose every commit's content is in the target", land.Committer{}, "",
		map[string]string{"c1.txt": "c1\n", "c2.txt": "c2\n"}, false, []string{dropped, dropped}},
}

func TestRun_LandsTheChangesOwnCommitsOntoTheTargetHead(t *testing.T) {
	t.Parallel()
	for _, c := range replays {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, func(d *land.Deps) { d.Committer = c.identity })
			target := commit(t, w.api, "main", w.base, "target", c.target)
			from, versionSHAs := w.base, []string{}
			if c.onTarget {
				from = target
			}
			for _, cm := range ownCommits {
				from = commitAs(t, w.api, "feature", from, cm.author, cm.message, cm.files)
				versionSHAs = append(versionSHAs, from)
			}
			ch := w.openBranch(t, "feature", w.caller())

			a := w.landNow(t, ch, w.caller(), nil)

			noop := !slices.Contains(c.became, replayed) && !slices.Contains(c.became, kept)
			if a.Status != land.StatusLanded || a.Attempts != 1 || a.Noop != noop || a.OntoSHA == nil || *a.OntoSHA != target ||
				a.LandedSHA == nil {
				t.Fatalf("attempt = %s after %d tries, noop %v, onto %v, landed %v; want landed after 1, noop %v, onto %s",
					a.Status, a.Attempts, a.Noop, a.OntoSHA, a.LandedSHA, noop, target)
			}
			if got := head(t, w.api, "main"); got != *a.LandedSHA {
				t.Errorf("main is at %s, want the landed %s", got, *a.LandedSHA)
			}
			checkBecame(t, w, a, ch.Number, target, c.committer, versionSHAs, c.became)
			want := []string{"a.txt", "c1.txt", "c2.txt"}
			for path := range c.target {
				if !slices.Contains(want, path) {
					want = append(want, path)
				}
			}
			slices.Sort(want)
			if files := strings.Fields(gitrepotest.Git(t, w.api, "ls-tree", "-r", "--name-only", "main")); !slices.Equal(files, want) {
				t.Errorf("main holds %v, want %v", files, want)
			}

			got, err := w.changes.ReadChange(context.Background(), nil, "api", ch.Number)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != change.StateLanded || got.Landed == nil || got.Landed.SHA != *a.LandedSHA || got.Landed.LandID != a.ID {
				t.Errorf("the change is %s, landed %+v; want landed at %s by %s", got.State, got.Landed, *a.LandedSHA, a.ID)
			}
			var lands []bool
			for _, e := range w.eventsOf(t, "parley.change.land") {
				if e.ResourceID == strconv.FormatInt(ch.Number, 10) {
					lands = append(lands, metadata(t, e)["noop"] == true)
				}
			}
			if !slices.Equal(lands, []bool{noop}) {
				t.Errorf("parley.change.land events with noop %v, want one with noop %v", lands, noop)
			}
			if n := len(w.landPushes(t)); n != map[bool]int{true: 0, false: 1}[noop] {
				t.Errorf("%d parley.git.push events via land, want %d", n, map[bool]int{true: 0, false: 1}[noop])
			}
		})
	}
}

// checkBecame checks that each of the own commits, versionSHAs, of the
// change number that the landed attempt a replayed onto target became
// what became says, one landed commit on the one before, with its author,
// date and message and committer as committer, or itself when kept, and
// that a maps each to the change.
func checkBecame(t *testing.T, w *world, a land.Attempt, number int64, target, committer string, versionSHAs, became []string) {
	t.Helper()
	prev, entries := target, a.Commits
	for i, b := range became {
		if b == dropped {
			continue
		}
		if len(entries) == 0 || entries[0].Change != number || entries[0].VersionSHA != versionSHAs[i] {
			t.Fatalf("commits = %+v; want the next entry for %s of change %d", a.Commits, versionSHAs[i], number)
		}
		landed := entries[0].LandedSHA
		entries = entries[1:]
		// author <date>|subject|committer|parent
		got := strings.Split(gitrepotest.Git(t, w.api, "log", "-1", "--date=raw",
			"--format=%an <%ae> %ad|%s|%cn <%ce>|%P", landed), "|")
		want := []string{ownCommits[i].author, ownCommits[i].message, committer, prev}
		if b == kept {
			want[2] = "T <t@example.invalid>"
			if landed != versionSHAs[i] {
				t.Errorf("%s landed as %s, want itself", versionSHAs[i], landed)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s landed as %s with author, message, committer and parent %q, want %q", versionSHAs[i], landed, got, want)
		}
		prev = landed
	}
	if len(entries) != 0 || *a.LandedSHA != prev {
		t.Errorf("commits = %+v, landed_sha %s; want one entry per commit not dropped, the last %s", a.Commits, *a.LandedSHA, prev)
	}
}
