package gitserver_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestRoutes_RefusesRefDiscoveryWithoutAValidService(t *testing.T) {
	t.Parallel()
	w := newWorld(t, hubtest.Config{})
	for _, q := range []string{"", "service=", "service=git-archive", "service=upload-pack", "svc=git-upload-pack"} {
		t.Run(q, func(t *testing.T) {
			a := w.call(t, http.MethodGet, "/git/acme/demo.git/info/refs?"+q, basic("x", w.ownerKey))
			if a.status != http.StatusBadRequest || a.body != "invalid or missing service parameter\n" {
				t.Errorf("answer = %d %q, want 400 \"invalid or missing service parameter\\n\"", a.status, a.body)
			}
		})
	}
	if n := w.service.served(); n != 0 {
		t.Errorf("%d refused requests reached the service, want none", n)
	}
	a := w.call(t, http.MethodGet, "/git/acme/demo.git/info/refs?service=git-upload-pack&service=git-receive-pack", basic("x", w.ownerKey))
	if a.status != http.StatusOK {
		t.Fatalf("two service values = %d %q, want 200", a.status, a.body)
	}
	if x := w.service.last(t); x.Service != "git-upload-pack" || x.Need != "read" {
		t.Errorf("exchange = service %q need %q, want git-upload-pack read", x.Service, x.Need)
	}
}

// The hub admits one request a minute per key, and each row starts a new
// window, so that the row over the limit, which sends its request twice,
// is the only one refused for it. The workspace empty is ready and has no
// repository on disk.
func TestRoutes_AnswersEveryEarlyRefusalWithOneLineOfText(t *testing.T) {
	t.Parallel()
	w := newWorld(t, hubtest.Config{TOML: "[limits]\nrequests_per_minute = 1"})
	w.addFork(t)
	w.AddWorkspace(t, access.Record{Slug: "empty", OwnerID: w.owner, OrgID: w.acme, Kind: access.KindRepository, Status: access.StatusReady})
	blocked := w.CreateUser(t, "blocked")
	blockedKey := w.IssueKey(t, blocked)
	if status, body := w.Call(t, http.MethodPost, "/api/v1/users/"+blocked+"/block", w.Token, nil); status != http.StatusOK {
		t.Fatalf("block the user = %d %s, want 200", status, body)
	}
	noGit, _ := w.pat(t, w.owner, "", "changes:read")
	rows := []struct {
		name, auth string
		request    gitRequest
		repo       string
		parent     string
		twice      bool
		status     int
		message    string
	}{
		{"no credential", "", gitRequests[0], "acme/demo.git", "", false, http.StatusUnauthorized, "authentication required"},
		{"blocked user", basic("x", blockedKey), gitRequests[0], "acme/demo.git", "", false, http.StatusForbidden, "user is blocked"},
		{"over the limit", basic("x", w.ownerKey), gitRequests[0], "acme/demo.git", "", true, http.StatusTooManyRequests, "rate limit exceeded"},
		{"unknown repository", basic("x", w.ownerKey), gitRequests[0], "acme/nosuch.git", "", false, http.StatusNotFound, "repository not found"},
		{"pat without git:read", basic("x", noGit), gitRequests[1], "acme/demo.git", "", false, http.StatusForbidden, "token lacks scope git:read"},
		{"pat without git:write", basic("x", w.ownerPAT), gitRequests[3], "acme/demo.git", "", false, http.StatusForbidden, "token lacks scope git:write"},
		{"member pushes", basic("x", w.memberKey), gitRequests[3], "acme/demo.git", "", false, http.StatusForbidden, msgReadOnly[:len(msgReadOnly)-1]},
		{"invalid service", basic("x", w.ownerKey), gitRequest{"refs", http.MethodGet, "/info/refs?service=git-archive", false}, "acme/demo.git", "", false,
			http.StatusBadRequest, "invalid or missing service parameter"},
		{"workspace not ready", basic("x", w.ownerKey), gitRequests[0], "acme/demo.git", access.StatusFailed, false, http.StatusConflict,
			"repository not available: workspace is failed"},
		{"parent busy", basic("x", w.memberKey), gitRequests[1], "acme/demo--t1.git", access.StatusCloning, false, http.StatusConflict,
			"repository not available: parent workspace is busy"},
		{"parent archived", basic("x", w.memberKey), gitRequests[3], "acme/demo--t1.git", access.StatusArchived, false, http.StatusConflict,
			"repository is read-only: parent workspace is archived"},
		{"no repository on disk", basic("x", w.ownerKey), gitRequests[0], "acme/empty.git", "", false, http.StatusInternalServerError, "internal error"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			w.advance(time.Minute)
			w.setStatus(t, "demo", cmp.Or(row.parent, access.StatusReady))
			if row.twice {
				w.call(t, row.request.method, row.request.to(row.repo), row.auth)
			}
			a := w.call(t, row.request.method, row.request.to(row.repo), row.auth)
			if a.status != row.status || a.body != row.message+"\n" || a.header.Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Errorf("answer = %d %q Content-Type %q, want %d %q text/plain; charset=utf-8",
					a.status, a.body, a.header.Get("Content-Type"), row.status, row.message+"\n")
			}
			if json.Valid([]byte(a.body)) {
				t.Errorf("body %q is JSON, want one line of text", a.body)
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

func TestRoutes_HidesEveryFailureBeforeTheExchangeBehindInternalError(t *testing.T) {
	t.Parallel()
	f := &faulty{}
	f.fail.Store("")
	w := newWorld(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) {
			f.Access, f.Records = d.Access, d.Records
			d.Access, d.Records = f, f
		})
	}})
	w.AddWorkspace(t, access.Record{Slug: "empty", OwnerID: w.owner, OrgID: w.acme, Kind: access.KindRepository, Status: access.StatusReady})
	failures := []struct{ name, fail, slug string }{
		{"workspace lookup", "workspace", "demo"},
		{"access decision", "decide", "demo"},
		{"repository directory missing", "", "empty"},
	}
	for _, c := range failures {
		t.Run(c.name, func(t *testing.T) {
			f.fail.Store(c.fail)
			defer f.fail.Store("")
			before := strings.Count(w.Log.String(), `"msg":"git request failed"`)
			a := w.call(t, http.MethodGet, "/git/acme/"+c.slug+".git/info/refs?service=git-upload-pack", basic("x", w.ownerKey))
			if a.status != http.StatusInternalServerError || a.body != "internal error\n" {
				t.Errorf("answer = %d %q, want 500 \"internal error\\n\"", a.status, a.body)
			}
			log := w.Log.String()
			if n := strings.Count(log, `"msg":"git request failed"`) - before; n != 1 {
				t.Fatalf("%d error records logged, want 1; log:\n%s", n, log)
			}
			record := log[strings.LastIndex(log, `"msg":"git request failed"`):]
			record = record[:strings.IndexByte(record, '\n')]
			for _, want := range []string{`"route":"GET /git/{org}/{repo}/info/refs"`, `"repository":"` + c.slug + `.git"`, `"error":`} {
				if !strings.Contains(record, want) {
					t.Errorf("error record %s lacks %s", record, want)
				}
			}
		})
	}
	if n := w.service.served(); n != 0 {
		t.Errorf("%d failed requests reached the service, want none", n)
	}
}
