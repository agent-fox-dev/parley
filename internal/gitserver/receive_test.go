package gitserver_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

const resultType = "application/x-git-receive-pack-result"

// stages are the options stage, the mapping stage and a ref-updated stage
// a test registers on its hub. They note in seen what they find when they
// are called, answer what the test set (the ref-updated stage fails with
// fail when it is set), and the mapping stage of holdRef tells held and
// waits for release.
type stages struct {
	t *testing.T
	w *world

	mu      sync.Mutex
	refusal string
	session *string
	lines   []string
	fail    error
	mapped  int
	seen    []string

	holdRef       string
	held, release chan struct{}
}

func (s *stages) CheckOptions(_ context.Context, in gitserver.OptionsInput) (gitserver.OptionsAnswer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := len(gitrepotest.Quarantines(s.w.repo)) > 0 || readable(s.w.repo, in.Updates[0].After)
	s.seen = append(s.seen, fmt.Sprintf("options %q, pack stored %t", in.Options, stored))
	return gitserver.OptionsAnswer{Refusal: s.refusal, SessionID: s.session}, nil
}

func (s *stages) MapPush(_ context.Context, in gitserver.MappingInput) []string {
	s.mu.Lock()
	s.mapped++
	u := in.Updates[0]
	at, _ := gitrepotest.Run(s.w.repo, "", "rev-parse", "--verify", "--quiet", u.Ref)
	s.seen = append(s.seen, fmt.Sprintf("mapping %s at %s, direct %t, version %+v, events at %d", u.Ref, at, u.Direct, u.Version, s.w.lastSeq(s.t)))
	lines, hold := s.lines, u.Ref == s.holdRef
	s.mu.Unlock()
	if hold {
		s.held <- struct{}{}
		<-s.release
	}
	return lines
}

func (s *stages) refUpdated(_ context.Context, _ *sql.Tx, in pushrule.StageInput) ([]*pushrule.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := "none"
	if in.SessionID != nil {
		session = *in.SessionID
	}
	s.seen = append(s.seen, fmt.Sprintf("ref-updated, session %s, pushed commit readable %t", session, readable(s.w.repo, in.Updates[0].After)))
	return []*pushrule.Version{{WorkspaceSlug: "demo", Number: 1, Version: 1}}, s.fail
}

// take returns what the stages saw since the last take and forgets it.
func (s *stages) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := s.seen
	s.seen = nil
	return seen
}

// newStagedWorld is newGitWorld with the stages registered on its hub.
func newStagedWorld(t *testing.T) (*world, *stages) {
	t.Helper()
	s := &stages{t: t, held: make(chan struct{}), release: make(chan struct{})}
	s.w = newGitWorld(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.Receiver().RegisterOptionsStage(s)
		c.Receiver().RegisterMappingStage(s)
		c.RefWriter().RegisterRefUpdatedStage(s.refUpdated)
	}})
	return s.w, s
}

func TestReceive_UpdatesEveryRefOfAnAcceptedPushTogether(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	for _, ref := range []string{"refs/heads/x", "refs/heads/y", "refs/heads/old"} {
		gitrepotest.Git(t, w.repo, "update-ref", ref, w.head)
	}
	c1 := w.commit(t, w.head, "one")
	c2 := w.commit(t, c1, "two")
	c3 := w.commit(t, w.head, "three")
	tag, err := gitrepotest.Run(w.client, "object "+c1+"\ntype commit\ntag v1\ntagger P <p@example.invalid> 0 +0000\n\nv1\n", "mktag")
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := gitrepotest.Run(w.client, "note", "hash-object", "-w", "--stdin")
	tree, _ := gitrepotest.Run(w.client, "100644 blob "+blob+"\t"+w.head+"\n", "mktree")
	notes := gitrepotest.Git(t, w.client, "commit-tree", "-m", "notes", tree)
	caps := append(slices.Clone(fullCaps), "push-options")
	for _, c := range []struct {
		name    string
		cmds    []command
		options []string
		gzip    bool
	}{
		{"create", []command{{zero, c1, "refs/heads/feat"}}, nil, false},
		{"fast-forward", []command{{c1, c2, "refs/heads/feat"}}, nil, false},
		{"forced update", []command{{c2, c3, "refs/heads/feat"}}, nil, false},
		{"two deletes without a pack", []command{{w.head, zero, "refs/heads/x"}, {w.head, zero, "refs/heads/y"}}, nil, false},
		{"annotated tag", []command{{zero, tag, "refs/tags/v1"}}, nil, false},
		{"notes", []command{{zero, notes, "refs/notes/commits"}}, nil, false},
		{"two creates and a delete", []command{
			{zero, w.commit(t, w.head, "a"), "refs/heads/a"}, {zero, w.commit(t, w.head, "b"), "refs/heads/b"}, {w.head, zero, "refs/heads/old"},
		}, nil, false},
		{"a push option with no stage registered", []command{{zero, w.commit(t, w.head, "opt"), "refs/heads/opt"}}, []string{"parley.anything"}, false},
		{"gzip-encoded request", []command{{zero, w.commit(t, w.head, "gz"), "refs/heads/gz"}}, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			body, header := receiveRequest(caps, c.cmds, c.options, w.packFor(t, c.cmds)), http.Header{}
			if c.gzip {
				var b bytes.Buffer
				zw := gzip.NewWriter(&b)
				_, _ = zw.Write([]byte(body))
				_ = zw.Close()
				body = b.String()
				header.Set("Content-Encoding", "gzip")
			}
			a := w.send(t, http.MethodPost, receivePack, basic("x", w.ownerKey), header, body)
			if a.status != http.StatusOK || a.header.Get("Content-Type") != resultType {
				t.Fatalf("answer = %d %q, want 200 %s", a.status, a.header.Get("Content-Type"), resultType)
			}
			want := []string{"unpack ok"}
			for _, cmd := range c.cmds {
				want = append(want, "ok "+cmd.ref)
			}
			if got := parseReport(a.body); !slices.Equal(got.lines, want) || !got.framed {
				t.Errorf("report = %q framed %t, want %q in side band 1", got.lines, got.framed, want)
			}
			refs := gitrepotest.Refs(t, w.repo, "")
			for _, cmd := range c.cmds {
				if at, exists := refs[cmd.ref]; cmd.new == zero && exists || cmd.new != zero && (at != cmd.new || !readable(w.repo, cmd.new)) {
					t.Errorf("%s is at %q, want %s, readable", cmd.ref, at, cmd.new)
				}
			}
			if q := gitrepotest.Quarantines(w.repo); len(q) != 0 {
				t.Errorf("objects/ holds %v, want no quarantine left", q)
			}
		})
	}
}

// A refused push writes nothing, keeps none of its objects and is reported
// ref by ref, the refused ref also on the side band.
func TestReceive_RefusesTheWholePushAndKeepsNoObjectWhenOneRefIsRefused(t *testing.T) {
	t.Parallel()
	w, s := newStagedWorld(t)
	gitrepotest.Git(t, w.repo, "update-ref", "refs/heads/feat", w.head)
	feat := w.commitOn(t, "feat")
	mainParent := gitrepotest.Git(t, w.repo, "rev-parse", "main~1")
	for i, c := range []struct {
		name, credential, settings string
		offending                  command
		message                    string
	}{
		{"hub-owned ref", w.ownerKey, "{}", command{zero, w.commit(t, w.head, "change"), "refs/parley/changes/1/1"},
			"refs/parley/ is maintained by the hub and cannot be pushed to"},
		{"forced update of a protected branch by the admin token", w.Token, "{}", command{w.head, w.commit(t, mainParent, "forced"), "refs/heads/main"},
			"main is protected and cannot be force-pushed or deleted"},
		{"delete of the unprotected default branch", w.ownerKey, `{"protected_branches":[]}`, command{w.head, zero, "refs/heads/main"},
			"main is the default branch and cannot be deleted"},
		{"fast-forward from a stale before", w.ownerKey, "{}", command{w.head, w.commit(t, feat, "ahead"), "refs/heads/feat"},
			"ref changed since it was read; fetch and try again"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.set(t, "demo", "settings", c.settings)
			ok := command{zero, w.commit(t, w.head, fmt.Sprint("ok ", i)), "refs/heads/ok"}
			cmds := []command{c.offending, ok}
			refs, seq := gitrepotest.Git(t, w.repo, "for-each-ref"), w.lastSeq(t)
			a := w.receive(t, "acme/demo.git", c.credential, receiveRequest(fullCaps, cmds, nil, w.packFor(t, cmds)))
			got := parseReport(a.body)
			want := []string{"unpack ok", "ng " + c.offending.ref + " " + c.message, "ng refs/heads/ok not updated: another ref in this push was refused"}
			if a.status != http.StatusOK || !slices.Equal(got.lines, want) {
				t.Errorf("answer = %d %q, want 200 %q", a.status, got.lines, want)
			}
			if line := "error: " + c.offending.ref + ": " + c.message + "\n"; strings.Count(got.text, line) != 1 || strings.Count(got.text, "error:") != 1 {
				t.Errorf("side band = %q, want the one line %q", got.text, line)
			}
			if after := gitrepotest.Git(t, w.repo, "for-each-ref"); after != refs {
				t.Errorf("refs are\n%s\nwant them unchanged\n%s", after, refs)
			}
			for _, cmd := range cmds {
				if cmd.new != zero && readable(w.repo, cmd.new) {
					t.Errorf("the pushed commit %s is readable in the repository", cmd.new)
				}
			}
			if q, seen := gitrepotest.Quarantines(w.repo), s.take(); len(q) != 0 || w.lastSeq(t) != seq || len(seen) != 1 {
				t.Errorf("quarantines %v, events %d -> %d, stages saw %q: want none left, no event, only the options stage", q, seq, w.lastSeq(t), seen)
			}
		})
	}
}

// A push stopped after its objects moved into the repository, where the
// ref-updated stages read them, takes them out again.
func TestReceive_KeepsNoObjectOfAPushStoppedAfterItsObjectsMovedIn(t *testing.T) {
	t.Parallel()
	w, s := newStagedWorld(t)
	s.fail = errors.New("stage failed")
	pushed := w.commit(t, w.head, "stopped")
	cmds := []command{{zero, pushed, "refs/heads/stopped"}}
	refs, seq := gitrepotest.Git(t, w.repo, "for-each-ref"), w.lastSeq(t)
	got := parseReport(w.receive(t, "acme/demo.git", w.ownerKey, receiveRequest(fullCaps, cmds, nil, w.packFor(t, cmds))).body)
	if want := []string{"unpack ok", "ng refs/heads/stopped internal error; try again"}; !slices.Equal(got.lines, want) {
		t.Errorf("report = %q, want %q", got.lines, want)
	}
	if seen, want := s.take(), "ref-updated, session none, pushed commit readable true"; len(seen) != 2 || seen[1] != want {
		t.Errorf("the stages saw %q, want the options stage, then %q", seen, want)
	}
	if readable(w.repo, pushed) || len(gitrepotest.Quarantines(w.repo)) != 0 || gitrepotest.Git(t, w.repo, "for-each-ref") != refs || w.lastSeq(t) != seq {
		t.Error("a push stopped by its ref-updated stage left its commit readable or a quarantine, moved a ref or wrote an event")
	}
}

func TestReceive_RunsTheOptionsStageBeforeThePackAndTheMappingStageAfterTheRefs(t *testing.T) {
	t.Parallel()
	w, s := newStagedWorld(t)
	session, line := "6f1c2a4e-8d3b-4c5a-9e7f-0a1b2c3d4e5f", "change demo#1 version 1 (opened)"
	s.session, s.lines = &session, []string{line}
	caps := append(slices.Clone(fullCaps), "push-options")
	feat := w.commit(t, w.head, "feat")
	cmds := []command{{zero, feat, "refs/heads/feat"}}
	seq := w.lastSeq(t)
	a := w.receive(t, "acme/demo.git", w.ownerKey, receiveRequest(caps, cmds, []string{"parley.change", "x=y"}, w.packFor(t, cmds)))
	got := parseReport(a.body)
	if want := []string{"unpack ok", "ok refs/heads/feat"}; !slices.Equal(got.lines, want) || !strings.Contains(got.text, line+"\n") {
		t.Errorf("report = %q side band %q, want %q and the mapping stage's line", got.lines, got.text, want)
	}
	want := []string{
		`options ["parley.change" "x=y"], pack stored false`,
		"ref-updated, session " + session + ", pushed commit readable true",
		fmt.Sprintf("mapping refs/heads/feat at %s, direct false, version &{WorkspaceSlug:demo Number:1 Version:1}, events at %d", feat, seq),
	}
	if seen := s.take(); !slices.Equal(seen, want) {
		t.Errorf("the stages saw\n%q\nwant\n%q", seen, want)
	}

	const refusal = "invalid push option parley.nope: unknown option"
	s.refusal = refusal
	refused := w.commit(t, w.head, "refused")
	cmds = []command{{zero, refused, "refs/heads/refused"}}
	refs, seq := gitrepotest.Git(t, w.repo, "for-each-ref"), w.lastSeq(t)
	got = parseReport(w.receive(t, "acme/demo.git", w.ownerKey, receiveRequest(caps, cmds, []string{"parley.nope"}, w.packFor(t, cmds))).body)
	if want := []string{"unpack ok", "ng refs/heads/refused " + refusal}; !slices.Equal(got.lines, want) || got.text != "error: "+refusal+"\n" {
		t.Errorf("report = %q side band %q, want %q and the one line error: %s", got.lines, got.text, want, refusal)
	}
	if readable(w.repo, refused) || len(gitrepotest.Quarantines(w.repo)) != 0 || gitrepotest.Git(t, w.repo, "for-each-ref") != refs || w.lastSeq(t) != seq {
		t.Error("a push the options stage refused stored an object, left a quarantine, moved a ref or wrote an event")
	}
	if seen := s.take(); len(seen) != 1 {
		t.Errorf("the stages saw %q, want only the options stage", seen)
	}
}

// The pack is checked whole before any rule runs: a pack git cannot index,
// or one without the object a command names, fails the push.
func TestReceive_FailsAPushWhosePackCannotBeRead(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	feat, other := w.commit(t, w.head, "feat"), w.commit(t, w.head, "other")
	cmds := []command{{zero, feat, "refs/heads/feat"}}
	pack := w.packFor(t, cmds)
	for _, c := range []struct{ name, pack string }{
		{"truncated pack", pack[:len(pack)/2]},
		{"twenty bytes of no pack", "these are no pack!!\n"},
		{"pack without the new commit", w.packFor(t, []command{{zero, other, "refs/heads/other"}})},
		{"no pack at all", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			refs, seq := gitrepotest.Git(t, w.repo, "for-each-ref"), w.lastSeq(t)
			a := w.receive(t, "acme/demo.git", w.ownerKey, receiveRequest(fullCaps, cmds, nil, c.pack))
			if got, want := parseReport(a.body).lines, []string{"unpack invalid pack", "ng refs/heads/feat unpacker error"}; a.status != http.StatusOK || !slices.Equal(got, want) {
				t.Errorf("answer = %d %q, want 200 %q", a.status, got, want)
			}
			if gitrepotest.Git(t, w.repo, "for-each-ref") != refs || readable(w.repo, feat) || readable(w.repo, other) ||
				len(gitrepotest.Quarantines(w.repo)) != 0 || w.lastSeq(t) != seq {
				t.Error("a push whose pack failed moved a ref, stored an object, left a quarantine or wrote an event")
			}
		})
	}
}

func TestReceive_AnswersAPushWithoutCommandsWithAnEmptyBody(t *testing.T) {
	t.Parallel()
	w, s := newStagedWorld(t)
	refs, seq := gitrepotest.Git(t, w.repo, "for-each-ref"), w.lastSeq(t)
	a := w.receive(t, "acme/demo.git", w.ownerKey, "0000")
	if a.status != http.StatusOK || a.header.Get("Content-Type") != resultType || a.body != "" {
		t.Errorf("answer = %d %q %q, want 200 %s and no body", a.status, a.header.Get("Content-Type"), a.body, resultType)
	}
	if gitrepotest.Git(t, w.repo, "for-each-ref") != refs || w.lastSeq(t) != seq || len(s.take()) != 0 {
		t.Error("a push without commands moved a ref, wrote an event or ran a stage")
	}
}

// head_sha follows a repository's default branch, and is on the record by
// the time the client has read the answer.
func TestReceive_RecordsTheNewHeadOfTheDefaultBranchBeforeAnswering(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	w.addForkTracking(t, nil, nil)
	next := w.commit(t, w.head, "next")
	gitrepotest.Git(t, w.workspaces.RepoPath("demo--t1"), "update-ref", "refs/heads/main", w.head)
	for _, c := range []struct {
		name, repo, credential string
		cmd                    command
		demo                   string
	}{
		{"fast-forward of demo's main", "acme/demo.git", w.ownerKey, command{w.head, next, "refs/heads/main"}, next},
		{"create of another branch", "acme/demo.git", w.ownerKey, command{zero, w.commit(t, w.head, "feat"), "refs/heads/feat"}, next},
		{"fast-forward of the fork's main", "acme/demo--t1.git", w.memberKey, command{w.head, w.commit(t, w.head, "fork"), "refs/heads/main"}, next},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmds := []command{c.cmd}
			got := parseReport(w.receive(t, c.repo, c.credential, receiveRequest(fullCaps, cmds, nil, w.packFor(t, cmds))).body).lines
			if want := []string{"unpack ok", "ok " + c.cmd.ref}; !slices.Equal(got, want) {
				t.Fatalf("report = %q, want %q", got, want)
			}
			heads := map[string]string{}
			for _, slug := range []string{"demo", "demo--t1"} {
				rec, _, err := workspace.BySlug(context.Background(), w.Store.Main(), slug)
				if err != nil {
					t.Fatal(err)
				}
				heads[slug] = deref(rec.HeadSHA)
			}
			if want := map[string]string{"demo": c.demo, "demo--t1": ""}; !maps.Equal(heads, want) {
				t.Errorf("head_sha = %v, want %v", heads, want)
			}
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestReceive_SendsOnlyWhatTheClientAskedFor(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	feat := w.commit(t, w.head, "feat")
	forced := w.commit(t, gitrepotest.Git(t, w.repo, "rev-parse", "main~1"), "forced")
	for _, c := range []struct {
		name     string
		caps     []string
		cmd      command
		accepted bool
		want     []string // the answer's pkt-lines
	}{
		{"nothing asked, refused", nil, command{zero, feat, "refs/parley/x"}, false, nil},
		{"nothing asked, accepted", nil, command{zero, feat, "refs/heads/feat"}, true, nil},
		{"report-status alone, refused", []string{"report-status"}, command{w.head, forced, "refs/heads/main"}, false,
			[]string{"unpack ok\n", "ng refs/heads/main main is protected and cannot be force-pushed or deleted\n", flushPkt}},
		{"report-status-v2 alone, accepted", []string{"report-status-v2"}, command{zero, feat, "refs/heads/v2"}, true,
			[]string{"unpack ok\n", "ok refs/heads/v2\n", flushPkt}},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := gitrepotest.Refs(t, w.repo, "")
			cmds := []command{c.cmd}
			a := w.receive(t, "acme/demo.git", w.ownerKey, receiveRequest(c.caps, cmds, nil, w.packFor(t, cmds)))
			if a.status != http.StatusOK || !slices.Equal(pktlines(a.body), c.want) || strings.Contains(a.body, "error:") {
				t.Errorf("answer = %d %q, want 200 with the pkt-lines %q and no error line", a.status, a.body, c.want)
			}
			want := maps.Clone(before)
			if c.accepted {
				want[c.cmd.ref] = c.cmd.new
			}
			if got := gitrepotest.Refs(t, w.repo, ""); !maps.Equal(got, want) {
				t.Errorf("refs = %v, want %v", got, want)
			}
		})
	}
}

// Of concurrent pushes of one ref from one before, the compare-and-swap
// lets one through, and the others' objects never reach the repository.
func TestReceive_AppliesExactlyOneOfConcurrentPushesOfOneRef(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	gitrepotest.Git(t, w.repo, "update-ref", "refs/heads/feat", w.head)
	const n = 8
	afters, bodies := make([]string, n), make([]string, n)
	for i := range n {
		afters[i] = w.commit(t, w.head, fmt.Sprint("push ", i))
		cmds := []command{{w.head, afters[i], "refs/heads/feat"}}
		bodies[i] = receiveRequest(fullCaps, cmds, nil, w.packFor(t, cmds))
	}
	reports := make([][]string, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			a, err := w.receiveErr("acme/demo.git", w.ownerKey, bodies[i])
			reports[i] = parseReport(a.body).lines
			if err != nil {
				reports[i] = []string{err.Error()}
			}
		})
	}
	close(start)
	wg.Wait()
	winner := -1
	for i, r := range reports {
		switch {
		case slices.Equal(r, []string{"unpack ok", "ok refs/heads/feat"}) && winner == -1:
			winner = i
		case slices.Equal(r, []string{"unpack ok", "ng refs/heads/feat ref changed since it was read; fetch and try again"}):
		default:
			t.Errorf("push %d answered %q", i, r)
		}
	}
	if winner == -1 {
		t.Fatal("no push went through")
	}
	if at := gitrepotest.Refs(t, w.repo, "refs/heads/")["feat"]; at != afters[winner] {
		t.Errorf("feat is at %s, want the winner's %s", at, afters[winner])
	}
	for i, id := range afters {
		if i != winner && readable(w.repo, id) {
			t.Errorf("the refused push %d left its commit in the repository", i)
		}
	}
}

// No lock spans a push: one held in its mapping stage keeps no other push
// of the workspace waiting.
func TestReceive_AppliesPushesOfDifferentRefsWithoutWaitingForEachOther(t *testing.T) {
	t.Parallel()
	w, s := newStagedWorld(t)
	s.holdRef = "refs/heads/a"
	request := func(ref string) string {
		cmds := []command{{zero, w.commit(t, w.head, ref), ref}}
		return receiveRequest(fullCaps, cmds, nil, w.packFor(t, cmds))
	}
	reqA, reqB := request("refs/heads/a"), request("refs/heads/b")
	bodyA := make(chan []string, 1)
	go func() {
		a, err := w.receiveErr("acme/demo.git", w.ownerKey, reqA)
		if err != nil {
			bodyA <- []string{err.Error()}
			return
		}
		bodyA <- parseReport(a.body).lines
	}()
	select {
	case <-s.held:
	case got := <-bodyA:
		t.Fatalf("A answered %q without being held in its mapping stage", got)
	}
	if got := parseReport(w.receive(t, "acme/demo.git", w.ownerKey, reqB).body).lines; !slices.Equal(got, []string{"unpack ok", "ok refs/heads/b"}) {
		t.Errorf("B answered %q while A was held, want ok refs/heads/b", got)
	}
	select {
	case got := <-bodyA:
		t.Fatalf("A answered %q before it was released", got)
	default:
	}
	close(s.release)
	if got := <-bodyA; !slices.Equal(got, []string{"unpack ok", "ok refs/heads/a"}) {
		t.Errorf("A answered %q, want ok refs/heads/a", got)
	}
	if refs := gitrepotest.Refs(t, w.repo, "refs/heads/"); refs["a"] == "" || refs["b"] == "" {
		t.Errorf("branches = %v, want a and b", refs)
	}
}

// postPush is a post-push listener that sends what it is told to called and
// then waits for release.
type postPush struct {
	called  chan pushrule.PostPush
	release chan struct{}
}

func (l *postPush) listen(_ context.Context, p pushrule.PostPush) error {
	l.called <- p
	<-l.release
	return nil
}

// next is the next call of l, failing the test when none comes within 5 s.
func (l *postPush) next(t *testing.T) pushrule.PostPush {
	t.Helper()
	select {
	case p := <-l.called:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("the post-push listener was not called")
		return pushrule.PostPush{}
	}
}

// The answer to an accepted push does not wait for the post-push listeners,
// and a push that moves no ref tells them nothing.
func TestReceive_TellsThePostPushListenersOfAnAcceptedPushWithoutWaitingForThem(t *testing.T) {
	t.Parallel()
	l := &postPush{called: make(chan pushrule.PostPush, 8), release: make(chan struct{})}
	w := newGitWorld(t, hubtest.Config{Compose: func(c *hub.Composition) { c.RefWriter().RegisterPostPushListener(l.listen) }})
	t.Cleanup(func() { close(l.release) })
	push := func(ref string) []string {
		cmds := []command{{zero, w.commit(t, w.head, ref), ref}}
		return parseReport(w.receive(t, "acme/demo.git", w.ownerKey, receiveRequest(fullCaps, cmds, nil, w.packFor(t, cmds))).body).lines
	}
	if got := push("refs/heads/feat"); !slices.Equal(got, []string{"unpack ok", "ok refs/heads/feat"}) {
		t.Fatalf("report = %q while the listener holds, want ok refs/heads/feat", got)
	}
	if p := l.next(t); p.Workspace != "demo" || p.Via != "git" || p.SessionID != nil || len(p.Updates) != 1 || p.Updates[0].Ref != "refs/heads/feat" || !p.Updates[0].Created {
		t.Errorf("the listener was told %+v, want demo, the create of refs/heads/feat, via git and no session", p)
	}
	w.receive(t, "acme/demo.git", w.ownerKey, "0000")
	push("refs/parley/x")
	push("refs/heads/after")
	if p := l.next(t); p.Updates[0].Ref != "refs/heads/after" || len(l.called) != 0 {
		t.Errorf("the listener was told of %s with %d calls pending, want only the accepted refs/heads/after", p.Updates[0].Ref, len(l.called))
	}
}
