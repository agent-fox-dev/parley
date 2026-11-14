package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The tests of this file set environment variables, so they must not run in
// parallel.

// syncEventType is the event a completed sync writes.
const syncEventType = "parley.workspace.sync"

// syncToken is the token the test origin of a sync test requires.
const syncToken = "sync-tok-7f3a"

// run runs parley against the hub with the developer's key and returns what
// it printed and its exit status, whatever that is.
func (h *workspaceHub) run(t *testing.T, args ...string) ran {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), append([]string{"--endpoint-url", h.URL(""), "--api-key", h.key}, args...), &stdout, &stderr)
	return ran{code, stdout.String(), stderr.String()}
}

// createReady creates the workspace slug of the developer from url with
// the origin's token and waits until the hub has cloned it.
func (h *workspaceHub) createReady(t *testing.T, slug, url string) {
	t.Helper()
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key,
		map[string]any{"slug": slug, "git_url": url, "git_pat": syncToken})
	if status != http.StatusCreated {
		t.Fatalf("create %s = %d %s, want 201", slug, status, body)
	}
	h.waitStatus(t, slug, workspace.StatusReady)
}

// advance adds a commit on top of branch in the bare repository dir, as a
// landing or a direct push leaves it there, and returns its id.
func advance(t *testing.T, dir, branch, msg string) string {
	t.Helper()
	head := gitrepotest.Git(t, dir, "rev-parse", "refs/heads/"+branch)
	id := gitrepotest.Git(t, dir, "commit-tree", head+"^{tree}", "-p", head, "-m", msg)
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/"+branch, id, head)
	return id
}

// syncResults maps each entry of the branches member of a sync answer to its
// result.
func syncResults(t *testing.T, answer map[string]any) map[string]string {
	t.Helper()
	entries, ok := answer["branches"].([]any)
	if !ok {
		t.Fatalf("the answer's branches is %v, want a list", answer["branches"])
	}
	got := map[string]string{}
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		name, _ := entry["name"].(string)
		got[name], _ = entry["result"].(string)
	}
	return got
}

func TestWorkspaceSync_ADeveloperSyncsFromTheShellAndIsToldWhichBranchDiverged(t *testing.T) {
	origin := newOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", syncToken))
	origin.Branch("release/1.x", "main")
	h := startWorkspaceHub(t, origin, "my-project")
	h.createReady(t, "my-project", origin.URL())
	hub := filepath.Join(h.root, "my-project.git")

	landed := advance(t, hub, "main", "landed in the hub")
	origin.Commit("release/1.x", "a fix on origin")
	origin.Branch("exp", "main")
	origin.Tag("v2", "main")

	first := h.run(t, "workspace", "sync", "my-project")
	if first.code != 0 {
		t.Fatalf("parley workspace sync exited %d; stderr = %q", first.code, first.stderr)
	}
	want := map[string]string{"main": "ahead", "dev": "up_to_date", "exp": "created", "release/1.x": "fast_forward"}
	if got := syncResults(t, first.json(t)); !reflect.DeepEqual(got, want) {
		t.Errorf("the first sync reported %v, want %v", got, want)
	}
	copied := gitrepotest.Refs(t, hub, "")
	for ref, id := range map[string]string{
		"refs/heads/release/1.x": origin.Head("release/1.x"),
		"refs/heads/exp":         origin.Head("exp"),
		"refs/heads/main":        landed,
		"refs/tags/v2":           gitrepotest.Git(t, origin.Dir(), "rev-parse", "v2"),
	} {
		if copied[ref] != id {
			t.Errorf("after the first sync the hub's %s = %q, want %q", ref, copied[ref], id)
		}
	}
	read := h.must(t, "workspace", "get", "my-project").json(t)
	if read["last_sync_at"] == nil || read["sync_error"] != nil || read["head_sha"] != landed {
		t.Errorf("the workspace shows last_sync_at %v, sync_error %v, head_sha %v; want set, null and %s",
			read["last_sync_at"], read["sync_error"], read["head_sha"], landed)
	}
	events := h.events(t, syncEventType, "my-project")
	if len(events) != 1 {
		t.Fatalf("sync events after the first run = %d, want 1", len(events))
	}
	var meta struct {
		Created     []string `json:"created"`
		FastForward []string `json:"fast_forward"`
		Ahead       []string `json:"ahead"`
	}
	if err := json.Unmarshal([]byte(events[0]), &meta); err != nil {
		t.Fatalf("the sync event's metadata %s: %v", events[0], err)
	}
	if !reflect.DeepEqual(meta.Created, []string{"exp"}) || !reflect.DeepEqual(meta.FastForward, []string{"release/1.x"}) || !reflect.DeepEqual(meta.Ahead, []string{"main"}) {
		t.Errorf("the sync event lists created %v, fast_forward %v, ahead %v; want [exp], [release/1.x], [main]",
			meta.Created, meta.FastForward, meta.Ahead)
	}

	origin.Commit("exp", "exp moves on origin")
	hubExp := advance(t, hub, "exp", "exp moves in the hub")
	second := h.run(t, "workspace", "sync", "my-project", "--fail-on-diverged")
	if second.code != 3 || !strings.Contains(second.stderr, "exp") {
		t.Errorf("sync --fail-on-diverged exited %d with stderr %q, want 3 naming exp", second.code, second.stderr)
	}
	if got := syncResults(t, second.json(t))["exp"]; got != "diverged" {
		t.Errorf("sync --fail-on-diverged reported exp %q, want diverged", got)
	}
	if got := gitrepotest.Refs(t, hub, "")["refs/heads/exp"]; got != hubExp {
		t.Errorf("after the diverged sync the hub's exp = %s, want it unchanged at %s", got, hubExp)
	}

	third := h.must(t, "workspace", "sync", "my-project", "--reset-to-origin")
	if got := syncResults(t, third.json(t))["exp"]; got != "reset" {
		t.Errorf("sync --reset-to-origin reported exp %q, want reset", got)
	}
	if got := gitrepotest.Refs(t, hub, "")["refs/heads/exp"]; got != origin.Head("exp") {
		t.Errorf("after the reset the hub's exp = %s, want origin's %s", got, origin.Head("exp"))
	}
}

// switchedProxy forwards TCP connections from its own address to target
// while it is up. Down closes its listener and every connection it holds,
// so that a client finds nothing listening; up listens on the same address
// again.
type switchedProxy struct {
	t      *testing.T
	addr   string
	target string

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]bool
	wg    sync.WaitGroup
}

// newSwitchedProxy starts a proxy to target on a free loopback port and
// stops it when the test ends.
func newSwitchedProxy(t *testing.T, target string) *switchedProxy {
	t.Helper()
	p := &switchedProxy{t: t, target: target, conns: map[net.Conn]bool{}}
	p.up("127.0.0.1:0")
	t.Cleanup(p.down)
	return p
}

// up listens on addr and forwards what it accepts.
func (p *switchedProxy) up(addr string) {
	p.t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		p.t.Fatalf("proxy listen on %s: %v", addr, err)
	}
	p.mu.Lock()
	p.ln, p.addr = ln, ln.Addr().String()
	p.mu.Unlock()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.forward(c)
		}
	}()
}

// forward copies between c and a new connection to the target.
func (p *switchedProxy) forward(c net.Conn) {
	u, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = c.Close()
		return
	}
	p.mu.Lock()
	p.conns[c], p.conns[u] = true, true
	p.mu.Unlock()
	for _, pair := range [][2]net.Conn{{c, u}, {u, c}} {
		p.wg.Add(1)
		go func(dst, src net.Conn) {
			defer p.wg.Done()
			_, _ = io.Copy(dst, src)
			_ = dst.Close()
			_ = src.Close()
		}(pair[0], pair[1])
	}
}

// down stops listening and closes every connection; calling it again does
// nothing more.
func (p *switchedProxy) down() {
	p.mu.Lock()
	if p.ln != nil {
		_ = p.ln.Close()
		p.ln = nil
	}
	for c := range p.conns {
		_ = c.Close()
		delete(p.conns, c)
	}
	p.mu.Unlock()
	p.wg.Wait()
}

func TestWorkspaceSync_AnAgentsSyncFailsWhileOriginIsUnreachableAndSucceedsWhenItIsBack(t *testing.T) {
	origin := newOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", syncToken))
	h := startWorkspaceHub(t, origin, "my-project")
	originAddr := strings.TrimPrefix(strings.TrimSuffix(origin.URL(), "/origin.git"), "https://")
	proxy := newSwitchedProxy(t, originAddr)
	h.createReady(t, "my-project", "https://"+proxy.addr+"/origin.git")
	pat := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"workspaces:write"}})

	proxy.down()
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/my-project/sync", pat, nil)
	var refusal struct {
		Error struct {
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil || status != http.StatusBadGateway || refusal.Error.ErrorType != "upstream_error" {
		t.Fatalf("sync while origin is unreachable = %d %s, want 502 upstream_error", status, body)
	}
	status, body = h.Call(t, http.MethodGet, "/api/v1/workspaces/my-project", pat, nil)
	read := decoded(t, body)
	syncErr, _ := read["sync_error"].(string)
	if status != http.StatusOK || syncErr == "" || strings.Contains(syncErr, syncToken) || read["last_sync_at"] != nil {
		t.Errorf("after the failed sync the workspace shows sync_error %q and last_sync_at %v, want a text without the token and null", syncErr, read["last_sync_at"])
	}
	if events := h.events(t, syncEventType, "my-project"); len(events) != 0 {
		t.Errorf("sync events after the failed sync = %d, want 0", len(events))
	}

	head := origin.Commit("main", "origin is back with a new commit")
	proxy.up(proxy.addr)
	status, body = h.Call(t, http.MethodPost, "/api/v1/workspaces/my-project/sync", pat, nil)
	if status != http.StatusOK {
		t.Fatalf("sync once origin is back = %d %s, want 200", status, body)
	}
	if got := syncResults(t, decoded(t, body))["main"]; got != "fast_forward" {
		t.Errorf("the sync reported main %q, want fast_forward", got)
	}
	status, body = h.Call(t, http.MethodGet, "/api/v1/workspaces/my-project", pat, nil)
	read = decoded(t, body)
	if status != http.StatusOK || read["sync_error"] != nil || read["last_sync_at"] == nil || read["head_sha"] != head || read["origin_head_sha"] != head {
		t.Errorf("after the sync the workspace shows sync_error %v, last_sync_at %v, head_sha %v, origin_head_sha %v; want null, set, %s, %s",
			read["sync_error"], read["last_sync_at"], read["head_sha"], read["origin_head_sha"], head, head)
	}
	if events := h.events(t, syncEventType, "my-project"); len(events) != 1 {
		t.Errorf("sync events = %d, want 1", len(events))
	}
}
