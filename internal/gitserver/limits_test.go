package gitserver_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// limitStage is an options stage that refuses a push with the push option
// refuse and holds one with the option wait until its context ends, or
// five seconds have passed on a context that never ends.
type limitStage struct{}

func (limitStage) CheckOptions(ctx context.Context, in gitserver.OptionsInput) (gitserver.OptionsAnswer, error) {
	switch {
	case slices.Contains(in.Options, "wait"):
		select {
		case <-ctx.Done():
			return gitserver.OptionsAnswer{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	case slices.Contains(in.Options, "refuse"):
		return gitserver.OptionsAnswer{Refusal: "refused by the options stage"}, nil
	}
	return gitserver.OptionsAnswer{}, nil
}

// bigPack is a root commit whose one file holds 4 KiB that do not compress,
// made in a repository of its own, and its pack.
func bigPack(t *testing.T) (id, pack string) {
	t.Helper()
	dir := t.TempDir() + "/big.git"
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	content := make([]byte, 4<<10)
	_, _ = rand.Read(content)
	stream := fmt.Sprintf("commit refs/heads/big\ncommitter P <p@example.invalid> 0 +0000\ndata 3\nbig\nM 100644 inline big.bin\ndata %d\n%s\n", len(content), content)
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet", "--date-format=raw"); err != nil {
		t.Fatal(err)
	}
	id = gitrepotest.Refs(t, dir, "refs/heads/")["big"]
	return id, gitrepotest.Pack(t, dir, id)
}

// The REST body limit of the last case is 1KB, and the pack is over 4 KiB.
func TestReceive_FailsAPushWhosePackIsLargerThanTheMaximumPushSize(t *testing.T) {
	t.Parallel()
	id, pack := bigPack(t)
	cmds := []command{{zero, id, "refs/heads/feat"}}
	limit := func(n int) string { return fmt.Sprintf("[git]\nmax_push_size = \"%d\"\n", n) }
	accepted := []string{"unpack ok", "ok refs/heads/feat"}
	for _, c := range []struct {
		name string
		cfg  hubtest.Config
		want []string
	}{
		{"a pack of exactly the limit", hubtest.Config{TOML: limit(len(pack))}, accepted},
		{"a pack one byte over the limit", hubtest.Config{TOML: limit(len(pack) - 1)},
			[]string{"unpack pack exceeds the maximum push size", "ng refs/heads/feat unpacker error"}},
		{"no limit and a pack over the REST body limit", hubtest.Config{TOML: limit(0), Server: `max_body_size = "1KB"`}, accepted},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newGitWorld(t, c.cfg)
			refs, seq := gitrepotest.Git(t, w.repo, "for-each-ref"), w.lastSeq(t)
			a := w.receive(t, "acme/demo.git", w.ownerKey, receiveRequest(fullCaps, cmds, nil, pack))
			if got := parseReport(a.body).lines; a.status != http.StatusOK || !slices.Equal(got, c.want) {
				t.Errorf("answer = %d %q, want 200 %q", a.status, got, c.want)
			}
			applied := c.want[0] == "unpack ok"
			moved := gitrepotest.Git(t, w.repo, "for-each-ref") != refs
			if q := gitrepotest.Quarantines(w.repo); moved != applied || readable(w.repo, id) != applied || (w.lastSeq(t) != seq) != applied || len(q) != 0 {
				t.Errorf("refs moved %t, commit readable %t, events %d -> %d, quarantines %v: want the push applied %t and no quarantine left",
					moved, readable(w.repo, id), seq, w.lastSeq(t), q, applied)
			}
		})
	}
}

// The push's [git].timeout is 100 ms: an options stage that holds the push,
// and a client that sends half its pack and then nothing, outlast it.
func TestReceive_AbandonsAPushWhoseTimeoutPassesBeforeItsRefsAreWritten(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) { d.Timeout = 100 * time.Millisecond })
		c.Receiver().RegisterOptionsStage(limitStage{})
	}})
	caps := append(slices.Clone(fullCaps), "push-options")
	for _, c := range []struct {
		name    string
		options []string
		stall   bool
	}{
		{"an options stage that waits for its context", []string{"wait"}, false},
		{"a body that stops halfway through the pack", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			feat := w.commit(t, w.head, c.name)
			cmds := []command{{zero, feat, "refs/heads/feat"}}
			pack := w.packFor(t, cmds)
			body := receiveRequest(caps, cmds, c.options, pack)
			refs, seq := gitrepotest.Git(t, w.repo, "for-each-ref"), w.lastSeq(t)
			start := time.Now()
			var got string
			if c.stall {
				got = w.stalled(t, body, len(body)-len(pack)/2)
			} else {
				got = w.receive(t, "acme/demo.git", w.ownerKey, body).body
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("the push was answered after %v, want within 2s", d)
			}
			if lines, want := parseReport(got).lines, []string{"unpack push timed out", "ng refs/heads/feat unpacker error"}; !slices.Equal(lines, want) {
				t.Errorf("report = %q, want %q", lines, want)
			}
			if gitrepotest.Git(t, w.repo, "for-each-ref") != refs || readable(w.repo, feat) || len(gitrepotest.Quarantines(w.repo)) != 0 || w.lastSeq(t) != seq {
				t.Error("a push that timed out moved a ref, stored an object, left a quarantine or wrote an event")
			}
		})
	}
}

// stalled sends the push body to demo's receive-pack route with the
// owner's API key and a Content-Length of all of it, but only its first
// sent bytes, and returns the body of the answer.
func (w *world) stalled(t *testing.T, body string, sent int) string {
	t.Helper()
	u, err := url.Parse(w.URL("/"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	head := "POST " + receivePack + " HTTP/1.1\r\nHost: " + u.Host + "\r\nAuthorization: " + basic("x", w.ownerKey) +
		"\r\nContent-Type: application/x-git-receive-pack-request\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
	if _, err := io.WriteString(conn, head+body[:sent]); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("a push whose body stopped got no answer: %v", err)
	}
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("answer = %d (%v), want 200", res.StatusCode, err)
	}
	return string(got)
}

// Every push is counted once, by the git server, with the result the
// processor returned. The hub's limit is 1KB, which only the big pack
// exceeds, and its timeout 2s.
func TestReceive_CountsEveryPushOnceByHowItEnded(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{TOML: "[git]\nmax_push_size = \"1KB\"\n", Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) { d.Timeout = 2 * time.Second })
		c.Receiver().RegisterOptionsStage(limitStage{})
	}})
	caps := append(slices.Clone(fullCaps), "push-options")
	request := func(cmd command, cut bool, options ...string) string {
		pack := w.packFor(t, []command{cmd})
		if cut {
			pack = pack[:len(pack)/2]
		}
		return receiveRequest(caps, []command{cmd}, options, pack)
	}
	create := func(ref string) command { return command{zero, w.commit(t, w.head, ref), ref} }
	big, pack := bigPack(t)
	for _, c := range []struct {
		name, settings, body string
		want                 gitserver.Result
	}{
		{"create refs/heads/feat", "{}", request(create("refs/heads/feat"), false), gitserver.ResultOK},
		{"flush only", "{}", "0000", gitserver.ResultOK},
		{"create refs/parley/x", "{}", request(create("refs/parley/x"), false), gitserver.ResultRefused},
		{"options stage refuses", "{}", request(create("refs/heads/opt"), false, "refuse"), gitserver.ResultRefused},
		{"stale before", "{}", request(command{w.head, w.commit(t, w.head, "stale"), "refs/heads/absent"}, false), gitserver.ResultRefused},
		{"truncated pack", "{}", request(create("refs/heads/cut"), true), gitserver.ResultError},
		{"pack over max_push_size", "{}", receiveRequest(caps, []command{{zero, big, "refs/heads/big"}}, nil, pack), gitserver.ResultError},
		{"timeout", "{}", request(create("refs/heads/slow"), false, "wait"), gitserver.ResultError},
		{"settings reader fails", `{"protected_branches":"main"}`, request(create("refs/heads/set"), false), gitserver.ResultError},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.set(t, "demo", "settings", c.settings)
			before := w.git.Requests()
			if a := w.receive(t, "acme/demo.git", w.ownerKey, c.body); a.status != http.StatusOK {
				t.Errorf("answer = %d %q, want 200", a.status, a.body)
			}
			w.counted(t, before, gitserver.LabelReceivePack, c.want)
		})
	}
}
