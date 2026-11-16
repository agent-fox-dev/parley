package land_test

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/land"
)

func TestLand_SquashesTheChangeIntoOneCommitOnTheTargetHead(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.changes.RegisterSessionLinker(anySession{})
	w.configure(t, `{"changes.auto_restack":false,"land.strategy":"squash"}`)
	ann, bob := "Ann <ann@example.org> 1600000000 +0200", "Bob <bob@example.org> 1600000300 -0500"
	for i, c := range []struct {
		name string
		// authors are the authors of the change's commits, oldest first.
		authors []string
		session bool
		// trailers are the message's lines after Parley-Change.
		trailers string
	}{
		{"by two authors, in a session", []string{ann, bob, ann}, true,
			"Parley-Session: %s\nCo-authored-by: Bob <bob@example.org>\n"},
		{"by two authors, in no session", []string{ann, bob, ann}, false, "Co-authored-by: Bob <bob@example.org>\n"},
		{"by one author", []string{ann, ann}, true, "Parley-Session: %s\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			branch, from := "sq"+strconv.Itoa(i), w.base
			var files []string
			for j, author := range c.authors {
				file := branch + "-" + strconv.Itoa(j) + ".txt"
				from = commitAs(t, w.api, branch, from, author, "commit "+strconv.Itoa(j), map[string]string{file: file + "\n"})
				files = append(files, file)
			}
			in := change.OpenInput{Workspace: "api", SourceBranch: branch, Title: new("Add search"), Intent: new("Why."),
				Kind: change.KindRef, Caller: w.caller()}
			session := strings.ToUpper(uuid.New().String())
			if c.session {
				in.SessionID = &session
			}
			ch := w.opened(t, in)
			moved := commit(t, w.api, "main", head(t, w.api, "main"), "unrelated", map[string]string{branch + "-main.txt": "m\n"})

			status, a := w.landCall(t, w.ownerKey, ch.Number, "", nil)

			if status != 200 || a["status"] != land.StatusLanded {
				t.Fatalf("land = %d %v, want 200 landed", status, a)
			}
			landed := head(t, w.api, "main")
			if gained := strings.Fields(gitrepotest.Git(t, w.api, "rev-list", "--parents", moved+"..main")); !reflect.DeepEqual(gained, []string{landed, moved}) {
				t.Fatalf("main gained %v, want one commit whose parent is %s", gained, moved)
			}
			repo, err := gitrepo.Git{}.Open(context.Background(), w.api)
			if err != nil {
				t.Fatal(err)
			}
			read, err := repo.ReadCommits(context.Background(), []string{landed})
			if err != nil {
				t.Fatal(err)
			}
			got := read[0]
			if got.Author.Name != "Ann" || got.Author.Email != "ann@example.org" || got.Committer.Name != "Parley" ||
				got.Committer.Email != "parley@localhost" {
				t.Errorf("author %+v, committer %+v; want Ann and the landing identity", got.Author, got.Committer)
			}
			if author := strings.TrimSpace(gitrepotest.Git(t, w.api, "show", "-s", "--date=raw", "--format=%an <%ae> %ad", landed)); author != ann {
				t.Errorf("author %q, want %q, the author, time and zone of the version's head", author, ann)
			}
			message := got.Message
			trailers := c.trailers
			if c.session {
				trailers = strings.ReplaceAll(trailers, "%s", strings.ToLower(session))
			}
			if want := "Add search\n\nWhy.\n\nParley-Change: " + ch.Ref + "\n" + trailers; message != want {
				t.Errorf("message %q, want %q", message, want)
			}
			if changed := strings.Fields(gitrepotest.Git(t, w.api, "diff", "--name-only", moved, landed)); !reflect.DeepEqual(changed, files) {
				t.Errorf("the commit changes %v, want the change's %v", changed, files)
			}
			want := []any{map[string]any{"change": float64(ch.Number), "version_sha": ch.HeadSHA, "landed_sha": landed}}
			if !reflect.DeepEqual(a["commits"], any(want)) || a["landed_sha"] != landed {
				t.Errorf("commits %v, landed_sha %v; want %v and %s", a["commits"], a["landed_sha"], want, landed)
			}
		})
	}
}
