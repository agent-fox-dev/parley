package gitserver_test

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The push side is version 0 whatever the client asks for: git receive-pack
// speaks no version 2.
func TestPush_AdvertisesEveryRefWithThePushCapabilitiesUnderVersion0(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	w.extendHistory(t)
	refs := strings.Fields(gitrepotest.Git(t, w.repo, "for-each-ref", "--format=%(refname)"))
	for _, protocol := range []string{"", "version=2"} {
		t.Run("Git-Protocol "+protocol, func(t *testing.T) {
			header := http.Header{}
			if protocol != "" {
				header.Set("Git-Protocol", protocol)
			}
			a := w.send(t, http.MethodGet, pushRefs, basic("x", w.ownerKey), header, "")
			if a.status != http.StatusOK || a.header.Get("Content-Type") != "application/x-git-receive-pack-advertisement" ||
				a.header.Get("Cache-Control") != "no-cache" {
				t.Fatalf("answer = %d Content-Type %q Cache-Control %q %q, want 200 application/x-git-receive-pack-advertisement no-cache",
					a.status, a.header.Get("Content-Type"), a.header.Get("Cache-Control"), a.body)
			}
			p := pktlines(a.body)
			if len(p) < 3 || p[0] != "# service=git-receive-pack\n" || p[1] != flushPkt {
				t.Fatalf("pkt-lines = %q, want the service line, a flush packet and git's advertisement", p)
			}
			_, caps, _ := strings.Cut(p[2], "\x00")
			for _, c := range []string{"report-status", "side-band-64k", "delete-refs", "ofs-delta", "atomic", "push-options"} {
				if !slices.Contains(strings.Fields(caps), c) {
					t.Errorf("capabilities %q lack %s", caps, c)
				}
			}
			for _, ref := range refs {
				if !strings.Contains(a.body, " "+ref) {
					t.Errorf("the advertisement does not list %s", ref)
				}
			}
		})
	}
}

// pushed is what the fake processor was handed for one push.
type pushed struct {
	caller    gitserver.PushCaller
	workspace workspace.Workspace
	body      []byte
}

// fakeProcessor is the push processor in the tests: it records what it was
// handed, reads the whole body, answers 200 with the body fake and reports
// the result report gave it, ok until then.
type fakeProcessor struct {
	mu     sync.Mutex
	calls  []pushed
	result gitserver.Result
}

// report makes the processor report res for the pushes to come.
func (f *fakeProcessor) report(res gitserver.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.result = res
}

func (f *fakeProcessor) ProcessPush(w http.ResponseWriter, r *http.Request, c gitserver.PushCaller, ws workspace.Workspace) gitserver.Result {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		body = append(body, []byte(" read: "+err.Error())...)
	}
	f.mu.Lock()
	f.calls = append(f.calls, pushed{c, ws, body})
	res := cmp.Or(f.result, gitserver.ResultOK)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "fake")
	return res
}

// The REST body limit is 1KB on this hub, and the push 64 kB. The processor
// answers every push 200, and the result it reports is what the push is
// counted as.
func TestPush_HandsTheCheckedPushToTheProcessorWithTheCallerAndTheWholeBody(t *testing.T) {
	t.Parallel()
	processor := &fakeProcessor{}
	w := newGitWorld(t, hubtest.Config{Server: `max_body_size = "1KB"`, Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) { d.Push = processor })
	}})
	pat, patID := w.pat(t, w.owner, "", "git:write")
	bound, boundID := w.pat(t, w.owner, "demo", "git:write")
	paths, pathsID := w.IssuePATID(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:write"}, Paths: []string{"docs/**"}, CreatedAt: w.clock()})
	cases := []struct {
		name       string
		credential string
		want       gitserver.PushCaller
		paths      []string
		result     gitserver.Result
	}{
		{"owner API key", w.ownerKey, gitserver.PushCaller{CredentialType: "api_key", CredentialID: strings.Split(w.ownerKey, "_")[1], UserID: w.owner, Level: access.LevelOwner}, nil, gitserver.ResultOK},
		{"owner PAT", pat, gitserver.PushCaller{CredentialType: "pat", CredentialID: patID, UserID: w.owner, Level: access.LevelOwner}, nil, gitserver.ResultRefused},
		{"owner PAT with paths", paths, gitserver.PushCaller{CredentialType: "pat", CredentialID: pathsID, UserID: w.owner, Level: access.LevelOwner}, []string{"docs/**"}, gitserver.ResultOK},
		{"bound PAT", bound, gitserver.PushCaller{CredentialType: "pat", CredentialID: boundID, UserID: w.owner, Level: access.LevelOwner,
			Binding: &access.Binding{TokenID: boundID, Workspace: "demo", Relation: access.RelationBound}}, nil, gitserver.ResultError},
		{"admin token", w.Token, gitserver.PushCaller{CredentialType: "admin_token", CredentialID: "admin", Level: access.LevelAdmin}, nil, gitserver.ResultOK},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sent := make([]byte, 64<<10)
			_, _ = rand.Read(sent)
			processor.report(c.result)
			before := w.git.Requests()
			a := w.send(t, http.MethodPost, receivePack, basic("x", c.credential),
				http.Header{"Content-Type": {"application/x-git-receive-pack-request"}}, string(sent))
			if a.status != http.StatusOK || a.body != "fake" || a.header.Get("Content-Type") != "application/x-git-receive-pack-result" {
				t.Errorf("answer = %d Content-Type %q %q, want the processor's 200 application/x-git-receive-pack-result \"fake\"",
					a.status, a.header.Get("Content-Type"), a.body)
			}
			w.counted(t, before, gitserver.LabelReceivePack, c.result)
			processor.mu.Lock()
			defer processor.mu.Unlock()
			if len(processor.calls) != i+1 {
				t.Fatalf("the processor was called %d times in all, want %d", len(processor.calls), i+1)
			}
			got := processor.calls[i]
			pat := got.caller.PAT
			got.caller.PAT = nil
			if !reflect.DeepEqual(got.caller, c.want) {
				t.Errorf("caller = %+v binding %+v, want %+v binding %+v", got.caller, got.caller.Binding, c.want, c.want.Binding)
			}
			if wantPAT := c.want.CredentialType == "pat"; (pat != nil) != wantPAT || (pat != nil && (pat.CredentialID != c.want.CredentialID || !slices.Equal(pat.Paths, c.paths))) {
				t.Errorf("PAT record = %+v, want the PAT %s with paths %q, or none for another credential", pat, c.want.CredentialID, c.paths)
			}
			if got.workspace.Slug != "demo" || got.workspace.OrgID != w.acme {
				t.Errorf("workspace = %q of org %q, want demo of acme", got.workspace.Slug, got.workspace.OrgID)
			}
			if !bytes.Equal(got.body, sent) {
				t.Errorf("the processor read %d bytes, want the %d bytes sent", len(got.body), len(sent))
			}
		})
	}
}

func TestPush_RefusesAndChangesNothingWithoutAProcessor(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) { d.Push = nil })
	}})
	before := gitrepotest.Git(t, w.repo, "for-each-ref")
	dir := t.TempDir()
	remote := w.remote(w.ownerKey, "acme/demo.git")
	runGit(t, dir, "clone", "--quiet", remote, "c")
	clone := dir + "/c"
	runGit(t, clone, "-c", "user.name=P", "-c", "user.email=p@example.invalid", "commit", "--allow-empty", "-m", "new")
	if out, err := clientGit(t, clone, nil, "push", remote, "HEAD:refs/heads/new"); err == nil {
		t.Errorf("git push = success, want a failure\n%s", out)
	}
	if after := gitrepotest.Git(t, w.repo, "for-each-ref"); after != before {
		t.Errorf("the hub's refs are\n%s\nwant them unchanged\n%s", after, before)
	}
	runs := w.runs.n.Load()
	if a := w.send(t, http.MethodPost, receivePack, basic("x", w.ownerKey), nil, ""); a.status != http.StatusInternalServerError || a.body != "internal error\n" {
		t.Errorf("push = %d %q, want 500 \"internal error\\n\"", a.status, a.body)
	}
	// A push that announces 1 MB and sends 4 bytes is answered at once
	// only when nothing waits for the rest of its body.
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
	_, err = io.WriteString(conn, "POST "+receivePack+" HTTP/1.1\r\nHost: "+u.Host+"\r\nAuthorization: "+basic("x", w.ownerKey)+
		"\r\nContent-Type: application/x-git-receive-pack-request\r\nContent-Length: 1048576\r\n\r\n0000")
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("a push whose body never ends got no answer: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("a push whose body never ends = %d, want 500", res.StatusCode)
	}
	if n := w.runs.n.Load(); n != runs {
		t.Errorf("git ran %d times for refused pushes, want never", n-runs)
	}
}
