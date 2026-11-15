package gitserver_test

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// DELETE gets the server's own answer to a method no route of the path has,
// which HEAD, a method the router serves with a GET route, is to match.
func TestRoutes_AnswersHEADAsAnyOtherMethodBeforeAnyCheck(t *testing.T) {
	t.Parallel()
	w := newGitWorld(t, hubtest.Config{})
	before := w.git.Requests()
	for _, auth := range []string{"", basic("x", w.ownerKey)} {
		other := w.call(t, http.MethodDelete, demoRefs, auth)
		a := w.call(t, http.MethodHead, demoRefs, auth)
		if a.status != http.StatusMethodNotAllowed || a.header.Get("Allow") != "GET" || a.dump() != strings.TrimSuffix(other.dump(), other.body) {
			t.Errorf("HEAD =\n%s\nwant DELETE's answer without its body:\n%s", a.dump(), other.dump())
		}
	}
	if got := grown(before, w.git.Requests()); len(got) != 0 || w.runs.n.Load() != 0 {
		t.Errorf("HEAD was counted %v and ran git %d times, want neither", got, w.runs.n.Load())
	}
}

// refusal is a request and the answer the checks are to give it: g on repo
// (acme/demo.git when "") with the Authorization header auth, sent twice
// when twice, so that the second is judged, while demo has the status demo
// and the marker marker and the fork demo--t1 the status fork (ready when
// "") and the access rule or the records fail as fail says. A status of
// 200 is a request the checks let through to the service; any other is
// answered with msg as one line of text.
type refusal struct {
	name, auth, repo, fail string
	g                      gitRequest
	demo, fork             string
	marker                 workspace.Marker
	twice                  bool
	status                 int
	msg                    string
}

// The hub admits one request a minute per key, and each row starts a new
// window, so that a row sent twice is the only one over the limit. The
// workspace empty is ready and has no repository on disk. Each request is
// counted once, by its route and its result: a refusal of authentication or
// access (401, 403, 404) as refused, any other as an error. Every answer to
// a request without a credential that passes (401), and every answer to one
// for a repository the caller cannot see (404), is the same bytes. The cause
// of every 500 is logged, with the route and the repository, and hidden
// from the answer.
func TestRoutes_AnswersEachRequestByTheFirstCheckItFails(t *testing.T) {
	t.Parallel()
	f := &faulty{}
	f.fail.Store("")
	w := newWorld(t, hubtest.Config{TOML: "[limits]\nrequests_per_minute = 1", Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) {
			f.Access, f.Records = d.Access, d.Records
			d.Access, d.Records = f, f
		})
	}})
	w.addFork(t)
	w.AddWorkspace(t, access.Record{Slug: "empty", OwnerID: w.owner, OrgID: w.acme, Kind: access.KindRepository, Status: access.StatusReady})
	blocked := w.CreateUser(t, "blocked")
	blockedKey, revoked := w.IssueKey(t, blocked), w.IssueKey(t, w.owner)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/users/" + blocked + "/block"},
		{http.MethodDelete, "/api/v1/users/" + w.owner + "/keys/" + strings.Split(revoked, "_")[1]},
	} {
		w.advance(time.Minute)
		if status, body := w.Call(t, c.method, c.path, w.Token, nil); status != http.StatusOK {
			t.Fatalf("%s %s = %d %s, want 200", c.method, c.path, status, body)
		}
	}
	expired := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:read"}, ExpiresAt: new(w.clock().Add(-1)), CreatedAt: w.clock().Add(-2)})
	noGit, _ := w.pat(t, w.owner, "", "changes:read")
	boundReader, _ := w.pat(t, w.owner, "demo", "git:read")
	boundNoGit, _ := w.pat(t, w.owner, "demo", "changes:read")
	memberPAT, _ := w.pat(t, w.member, "", "git:write")
	owner, member := basic("x", w.ownerKey), basic("x", w.memberKey)
	const (
		ready, archived, failed       = access.StatusReady, access.StatusArchived, access.StatusFailed
		pending, cloning              = access.StatusPending, access.StatusCloning
		notAvailable, busy, readOnly  = "repository not available: workspace is ", "repository not available: parent workspace is busy", "repository is read-only: parent workspace is archived"
		notFound, scope, invalidQuery = "repository not found", "token lacks scope ", "invalid or missing service parameter"
	)
	refs := func(query string) gitRequest {
		return gitRequest{"refs?" + query, http.MethodGet, "/info/refs?" + query, false, gitserver.LabelInfoRefs}
	}
	other := "beta/other.git"
	rows := []refusal{
		{name: "blocked user", auth: basic("x", blockedKey), g: reqFetchRefs, status: http.StatusForbidden, msg: "user is blocked"},
		{name: "blocked user over the limit", auth: basic("x", blockedKey), g: reqFetchRefs, twice: true, status: http.StatusForbidden, msg: "user is blocked"},
		{name: "over the limit", auth: owner, g: reqFetchRefs, twice: true, status: http.StatusTooManyRequests, msg: "rate limit exceeded"},
		{name: "outside the binding over the limit", auth: basic("x", boundReader), g: reqFetchRefs, repo: other, twice: true, status: http.StatusTooManyRequests, msg: "rate limit exceeded"},
		{name: "outside the binding without a git scope", auth: basic("x", boundNoGit), g: reqFetchRefs, repo: other, status: http.StatusNotFound, msg: notFound},
		{name: "stranger with an invalid service", auth: basic("x", w.strangerKey), g: refs("service=git-archive"), status: http.StatusNotFound, msg: notFound},
		{name: "invalid service on a workspace not ready", auth: owner, g: refs("service=git-archive"), demo: archived, status: http.StatusBadRequest, msg: invalidQuery},
		{name: "no repository on disk", auth: owner, g: reqFetchRefs, repo: "acme/empty.git", status: http.StatusInternalServerError, msg: "internal error"},
		{name: "the workspace lookup fails", auth: owner, g: reqFetchRefs, fail: "workspace", status: http.StatusInternalServerError, msg: "internal error"},
		{name: "the access decision fails", auth: owner, g: reqFetch, fail: "decide", status: http.StatusInternalServerError, msg: "internal error"},
	}
	for _, q := range []string{"", "service=", "service=git-archive", "service=upload-pack", "svc=git-upload-pack"} {
		rows = append(rows, refusal{name: "service query " + q, auth: owner, g: refs(q), status: http.StatusBadRequest, msg: invalidQuery})
	}
	for _, auth := range []string{"", "Bearer " + w.ownerKey, "Basic !!!", "Basic " + base64.StdEncoding.EncodeToString([]byte("nocolon")), basic("x", ""),
		basic("x", "parley_abcdefgh_"+strings.Repeat("x", 32)), basic("x", revoked), basic("x", expired)} {
		for _, at := range []struct {
			g    gitRequest
			repo string
		}{{reqFetchRefs, ""}, {reqPush, "acme/nosuch.git"}} {
			rows = append(rows, refusal{name: "credential " + auth + "/" + at.g.name, auth: auth, g: at.g, repo: at.repo, status: http.StatusUnauthorized, msg: "authentication required"})
		}
	}
	for _, repo := range []string{"", "acme/nosuch.git", other} {
		for _, g := range gitRequests {
			auth, need := noGit, "git:read"
			if g.push {
				auth, need = w.ownerPAT, "git:write"
			}
			rows = append(rows, refusal{name: "no " + need + "/" + repo + "/" + g.name, auth: basic("x", auth), g: g, repo: repo, status: http.StatusForbidden, msg: scope + need})
		}
	}
	for _, status := range []string{ready, archived} {
		for _, auth := range []string{w.memberKey, memberPAT} {
			for _, g := range []gitRequest{reqPushRefs, reqPush} {
				rows = append(rows, refusal{name: "member pushes/" + status + "/" + g.name, auth: basic("x", auth), g: g, demo: status, status: http.StatusForbidden, msg: msgReadOnly[:len(msgReadOnly)-1]})
			}
		}
	}
	for _, g := range gitRequests {
		for _, repo := range []string{"acme/demo", "acme/nosuch.git", "acme/solo.git", "beta/demo.git", "ACME/demo.git"} {
			rows = append(rows, refusal{name: "unseen " + repo + "/" + g.name, auth: owner, g: g, repo: repo, status: http.StatusNotFound, msg: notFound})
		}
		for _, status := range []string{pending, cloning, ready, failed, archived} {
			rows = append(rows, refusal{name: "stranger/" + status + "/" + g.name, auth: basic("x", w.strangerKey), g: g, demo: status, status: http.StatusNotFound, msg: notFound})
		}
		for _, s := range []struct {
			status string
			marker workspace.Marker
			word   string
		}{{pending, "", "pending"}, {cloning, "", "cloning"}, {failed, "", "failed"}, {archived, "", "archived"}, {ready, workspace.MarkerReclone, "busy"}} {
			rows = append(rows, refusal{name: "workspace " + s.word + "/" + g.name, auth: owner, g: g, demo: s.status, marker: s.marker, status: http.StatusConflict, msg: notAvailable + s.word})
		}
		// A fork is judged by its own status, then by its parent's.
		for _, c := range []struct {
			name, fork, parent string
			marker             workspace.Marker
			fetch, push        string
		}{
			{"parent cloning", ready, cloning, "", busy, busy},
			{"parent pending", ready, pending, "", busy, busy},
			{"parent failed", ready, failed, "", busy, busy},
			{"marker on a ready parent", ready, ready, workspace.MarkerReclone, busy, busy},
			{"marker on an archived parent", ready, archived, workspace.MarkerDelete, busy, busy},
			{"parent archived", ready, archived, "", "", readOnly},
			{"fork failed with its parent cloning", failed, cloning, "", notAvailable + "failed", notAvailable + "failed"},
			{"parent ready", ready, ready, "", "", ""},
		} {
			r := refusal{name: "fork/" + c.name + "/" + g.name, auth: member, g: g, repo: "acme/demo--t1.git", demo: c.parent, fork: c.fork, marker: c.marker,
				status: http.StatusConflict, msg: c.fetch}
			if g.push {
				r.msg = c.push
			}
			if r.msg == "" {
				r.status = http.StatusOK
			}
			rows = append(rows, r)
		}
	}
	first := map[int]string{}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			w.advance(time.Minute)
			w.set(t, "demo", "status", cmp.Or(r.demo, ready))
			w.set(t, "demo", "maintenance", string(r.marker))
			w.set(t, "demo--t1", "status", cmp.Or(r.fork, ready))
			f.fail.Store(r.fail)
			defer f.fail.Store("")
			repo := cmp.Or(r.repo, "acme/demo.git")
			to, logged := r.g.to(repo), strings.Count(w.Log.String(), `"msg":"git request failed"`)
			if r.twice {
				w.call(t, r.g.method, to, r.auth)
			}
			before, served := w.git.Requests(), w.service.served()
			a := w.call(t, r.g.method, to, r.auth)
			result := gitserver.ResultError
			switch r.status {
			case http.StatusOK:
				result = gitserver.ResultOK
				if a.status != http.StatusOK || w.service.served() != served+1 {
					t.Errorf("answer = %d %q, want the request served", a.status, a.body)
				}
			case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
				result = gitserver.ResultRefused
			}
			if r.status != http.StatusOK {
				if w.service.served() != served {
					t.Error("the refused request reached the service")
				}
				if a.status != r.status || a.body != r.msg+"\n" || a.header.Get("Content-Type") != "text/plain; charset=utf-8" {
					t.Errorf("answer = %d %q Content-Type %q, want %d %q text/plain; charset=utf-8", a.status, a.body, a.header.Get("Content-Type"), r.status, r.msg+"\n")
				}
				challenge := ""
				if r.status == http.StatusUnauthorized {
					challenge = `Basic realm="parley"`
				}
				if got := a.header.Get("WWW-Authenticate"); got != challenge {
					t.Errorf("WWW-Authenticate = %q, want %q", got, challenge)
				}
			}
			if r.status == http.StatusUnauthorized || r.status == http.StatusNotFound {
				if first[r.status] == "" {
					first[r.status] = a.dump()
				} else if got := a.dump(); got != first[r.status] {
					t.Errorf("answer =\n%s\nwant the same as the first %d:\n%s", got, r.status, first[r.status])
				}
			}
			w.counted(t, before, r.g.label, result)
			log := w.Log.String()
			n := strings.Count(log, `"msg":"git request failed"`) - logged
			if want := r.status == http.StatusInternalServerError; n != 0 && !want || n != 1 && want {
				t.Fatalf("%d error records logged for a %d, want one for a 500 alone; log:\n%s", n, r.status, log)
			} else if want {
				route, _, _ := strings.Cut(r.g.suffix, "?")
				record := log[strings.LastIndex(log, `"msg":"git request failed"`):]
				record = record[:strings.IndexByte(record, '\n')]
				for _, want := range []string{`"route":"` + r.g.method + ` /git/{org}/{repo}` + route + `"`, `"repository":"` + path.Base(repo) + `"`, `"error":`} {
					if !strings.Contains(record, want) {
						t.Errorf("error record %s lacks %s", record, want)
					}
				}
			}
		})
	}
}

// faulty is the hub's access rule and records, failing the call fail names:
// "decide" or "workspace"; "" fails none.
type faulty struct {
	gitserver.Access
	gitserver.Records
	fail atomic.Value
}

var errFault = errors.New("SELECT * FROM workspaces: disk I/O error at /var/lib/parley/data/parley.db")

func (f *faulty) Decide(ctx context.Context, c access.Caller, slug string, need access.Need) (access.Decision, error) {
	if f.fail.Load() == "decide" {
		return access.Decision{}, errFault
	}
	return f.Access.Decide(ctx, c, slug, need)
}

func (f *faulty) Workspace(ctx context.Context, slug string) (workspace.Workspace, bool, error) {
	if f.fail.Load() == "workspace" {
		return workspace.Workspace{}, false, errFault
	}
	return f.Records.Workspace(ctx, slug)
}
