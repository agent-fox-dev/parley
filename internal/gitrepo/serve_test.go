package gitrepo_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// Not parallel: the environment of the test process asks for protocol
// version 2 and hides every branch, for a fetch and a push alike, through a
// system, a global and a home configuration, none of which may reach the git
// the hub runs.
func TestServe_ServesTheRequestAsAskedWhateverTheHostSays(t *testing.T) {
	o := gitrepotest.NewOrigin(t)
	head := o.Commit("main", "first")
	hide := filepath.Join(t.TempDir(), "gitconfig")
	for _, f := range []string{hide, filepath.Join(filepath.Dir(hide), ".gitconfig")} {
		if err := os.WriteFile(f, []byte("[transfer]\n\thideRefs = refs/heads\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GIT_CONFIG_SYSTEM", hide)
	t.Setenv("GIT_CONFIG_GLOBAL", hide)
	t.Setenv("HOME", filepath.Dir(hide))
	t.Setenv("GIT_PROTOCOL", "version=2")
	upload, receive := (gitrepo.Git{}).UploadPack, (gitrepo.Git{}).ReceivePack
	cases := []struct {
		name         string
		run          func(context.Context, gitrepo.ServeRequest) error
		req          gitrepo.ServeRequest
		prefix, want string
		not          string
	}{
		{"version 0 unless asked", upload, gitrepo.ServeRequest{AdvertiseRefs: true}, "", head + " refs/heads/main", "version 2"},
		{"version 2 when asked", upload, gitrepo.ServeRequest{AdvertiseRefs: true, Protocol2: true}, pkt("version 2\n"), "ls-refs", "refs/heads/"},
		{"configuration of the run", upload, gitrepo.ServeRequest{AdvertiseRefs: true, Config: []string{"uploadpack.allowFilter=true"}}, "", " filter", ""},
		{"push never version 2", receive, gitrepo.ServeRequest{AdvertiseRefs: true, Protocol2: true}, "", head + " refs/heads/main\x00", "version"},
	}
	for _, c := range cases {
		var out bytes.Buffer
		c.req.Dir, c.req.Stdout = o.Dir(), &out
		if err := c.run(context.Background(), c.req); err != nil {
			t.Errorf("%s: git = %v", c.name, err)
			continue
		}
		if got := out.String(); !strings.HasPrefix(got, c.prefix) || !strings.Contains(got, c.want) || (c.not != "" && strings.Contains(got, c.not)) {
			t.Errorf("%s: git wrote %q, want it to start with %q, hold %q and not %q", c.name, got, c.prefix, c.want, c.not)
		}
	}
	var out bytes.Buffer
	err := (gitrepo.Git{}).UploadPack(context.Background(), gitrepo.ServeRequest{Dir: o.Dir(), Stdin: strings.NewReader("0009junk"), Stdout: &out})
	var se *gitrepo.ServeError
	if !errors.As(err, &se) || !strings.Contains(se.Stderr, "hung up") {
		t.Errorf("a truncated request: UploadPack = %v, want a *ServeError with git's text", err)
	}
}

// Not parallel: it puts a fake git first on the PATH of the test process,
// which starts a child of its own, answers, and waits on the child until it
// is killed.
func TestUploadPack_StreamsUntilTheContextEndsAndThenKillsGitWithItsChildren(t *testing.T) {
	bin := t.TempDir()
	child := filepath.Join(bin, "child")
	script := "#!/bin/sh\nsleep 30 &\necho $! > '" + child + "'\nprintf '0008NAK\\n'\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- (gitrepo.Git{}).UploadPack(ctx, gitrepo.ServeRequest{Dir: bin, Stdin: strings.NewReader("0000"), Stdout: &signalWriter{first: first}})
	}()
	select {
	case <-first:
	case err := <-done:
		t.Fatalf("UploadPack returned %v before git wrote anything to its writer", err)
	case <-time.After(5 * time.Second):
		t.Fatal("git's first bytes did not reach the writer within 5s")
	}
	select {
	case err := <-done:
		t.Fatalf("UploadPack returned %v while git was still running", err)
	default:
	}
	cancel()
	start := time.Now()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("UploadPack = %v, want an error wrapping context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("UploadPack did not return within 3s of the end of its context")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("returned %v after the end of its context, want within 2s", d)
	}
	pid := readPID(t, child)
	for deadline := time.Now().Add(2 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("git's child %d is still running 2s after the return", pid)
		}
	}
}

func TestUploadPack_GivesUpOnAVersion2RequestThatStopsComingWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	head := o.Commit("main", "first")
	stalled, sender := io.Pipe()
	defer sender.Close()
	go func() { _, _ = io.WriteString(sender, pkt("command=fetch\n")+"0001"+pkt("want "+head+"\n")) }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := (gitrepo.Git{}).UploadPack(ctx, gitrepo.ServeRequest{Dir: o.Dir(), Protocol2: true, Stdin: stalled, Stdout: io.Discard})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Errorf("UploadPack = %v after %v, want an error wrapping context.DeadlineExceeded within 2s", err, time.Since(start))
	}
}

// Every want names a commit by an id git reads as its own: the leading hex
// digits, in either case, whatever follows them.
func TestUploadPack_RefusesAVersion2WantOfACommitNoRefReachesHoweverItIsWritten(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	head := o.Commit("main", "first")
	dangling := gitrepotest.Git(t, o.Dir(), "commit-tree", "-m", "dangling", head+"^{tree}")
	gitrepotest.Git(t, o.Dir(), "tag", "-a", "-m", "loose", "loose", dangling)
	tag := gitrepotest.Git(t, o.Dir(), "rev-parse", "refs/tags/loose")
	gitrepotest.Git(t, o.Dir(), "update-ref", "-d", "refs/tags/loose")
	fetch := func(head, want string) string {
		return head + "0001" + pkt("want "+want+"\n") + pkt("done\n") + "0000"
	}
	command := pkt("command=fetch\n")
	for _, c := range []struct {
		name, request, refused string
		limit                  int64
	}{
		{"a commit main reaches", fetch(command, head), "", 0},
		{"a request of its bound", fetch(command, head), "", int64(len(fetch(command, head)))},
		{"a request over its bound", fetch(command, head), "request too large", int64(len(fetch(command, head))) - 1},
		{"a lowercase id", fetch(command, dangling), "upload-pack: not our ref " + dangling, 0},
		{"an uppercase id", fetch(command, strings.ToUpper(dangling)), "upload-pack: not our ref " + dangling, 0},
		{"an id followed by more", fetch(command, dangling+strings.Repeat("f", 30)+" junk"), "upload-pack: not our ref " + dangling, 0},
		{"a capability before the command", fetch(pkt("agent=git/2.56\n")+command, dangling), "upload-pack: not our ref " + dangling, 0},
		{"an annotated tag of such a commit", fetch(command, tag), "upload-pack: not our ref " + tag, 0},
		{"an id too short", fetch(command, dangling[:12]), "upload-pack: protocol error, expected to get oid", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := (gitrepo.Git{}).UploadPack(context.Background(), gitrepo.ServeRequest{
				Dir: o.Dir(), Protocol2: true, Stdin: strings.NewReader(c.request), Stdout: &out, MaxRequest: c.limit,
			})
			var se *gitrepo.ServeError
			switch {
			case c.refused == "request too large" && (!errors.Is(err, gitrepo.ErrRequestTooLarge) || out.Len() != 0):
				t.Errorf("UploadPack = %v, wrote %q; want ErrRequestTooLarge with git not run", err, out.String())
			case c.refused == "request too large":
			case c.refused == "" && (err != nil || !strings.Contains(out.String(), "PACK")):
				t.Errorf("UploadPack = %v, wrote %q; want a pack", err, out.String())
			case c.refused != "" && (!errors.As(err, &se) || se.Stderr != c.refused || out.Len() != 0):
				t.Errorf("UploadPack = %v, wrote %q; want %q with git not run", err, out.String(), c.refused)
			}
		})
	}
}

// A tree, a blob or an annotated tag is served by its id when a ref of the
// repository served reaches it, deep in history too, and refused when none
// does; a task fork's refs, not its parent's, say what the fork reaches.
func TestUploadPack_ServesATreeBlobOrTagByIdOnlyWhenARefReachesIt(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	o.History()
	git := func(args ...string) string { return gitrepotest.Git(t, o.Dir(), args...) }
	git("tag", "-a", "-m", "release", "release", "main")
	tipTag := git("rev-parse", "refs/tags/release")
	git("tag", "-a", "-m", "gone", "gone", "main")
	goneTag := git("rev-parse", "refs/tags/gone")
	git("update-ref", "-d", "refs/tags/gone")
	looseBlob, err := gitrepotest.Run(o.Dir(), "no ref reaches this\n", "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	looseBlob = strings.TrimSpace(looseBlob)
	looseTree, err := gitrepotest.Run(o.Dir(), "100644 blob "+looseBlob+"\tloose\n", "mktree")
	if err != nil {
		t.Fatal(err)
	}
	looseTree = strings.TrimSpace(looseTree)
	fork := filepath.Join(t.TempDir(), "fork.git")
	if err := (gitrepo.Git{}).CreateFork(context.Background(), o.Dir(), fork); err != nil {
		t.Fatal(err)
	}
	gitrepotest.Git(t, fork, "update-ref", "refs/heads/main", git("rev-parse", "main"))
	for _, c := range []struct {
		name, dir, id string
		served        bool
	}{
		{"a blob of main's first commit", o.Dir(), git("rev-parse", "main~2:README"), true},
		{"the tree of main's first commit", o.Dir(), git("rev-parse", "main~2^{tree}"), true},
		{"an annotated tag a ref points at", o.Dir(), tipTag, true},
		{"a blob no ref reaches", o.Dir(), looseBlob, false},
		{"a tree no ref reaches", o.Dir(), looseTree, false},
		{"an annotated tag no ref points at", o.Dir(), goneTag, false},
		{"a fork's blob its own branch reaches", fork, git("rev-parse", "main:README"), true},
		{"a parent's blob no ref of the fork reaches", fork, git("rev-parse", "dev:README"), false},
	} {
		for _, v2 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/protocol 2 %v", c.name, v2), func(t *testing.T) {
				t.Parallel()
				request := pkt("want "+c.id+"\n") + "0000" + pkt("done\n")
				if v2 {
					request = pkt("command=fetch\n") + "0001" + pkt("want "+c.id+"\n") + pkt("done\n") + "0000"
				}
				var out bytes.Buffer
				err := (gitrepo.Git{}).UploadPack(context.Background(), gitrepo.ServeRequest{
					Dir: c.dir, Protocol2: v2, Config: []string{"uploadpack.allowReachableSHA1InWant=true"},
					Stdin: strings.NewReader(request), Stdout: &out,
				})
				var se *gitrepo.ServeError
				switch {
				case c.served && (err != nil || !strings.Contains(out.String(), "PACK")):
					t.Errorf("UploadPack = %v, wrote %q; want a pack", err, out.String())
				case !c.served && (!errors.As(err, &se) || se.Stderr != "upload-pack: not our ref "+c.id || out.Len() != 0):
					t.Errorf("UploadPack = %v, wrote %q; want %q with git not run", err, out.String(), "upload-pack: not our ref "+c.id)
				}
			})
		}
	}
}

// signalWriter closes first at its first write.
type signalWriter struct {
	first  chan struct{}
	closed bool
}

func (s *signalWriter) Write(p []byte) (int, error) {
	if !s.closed {
		s.closed = true
		close(s.first)
	}
	return len(p), nil
}

// readPID reads the process id written to file.
func readPID(t *testing.T, file string) int {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// pkt is s as one pkt-line.
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
