package policy_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/policy"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// world is a hub with the policy composed: the org o1, whose members are
// dana and mel, and the outsider oz, each with an API key; dana's ready
// workspaces api and web of o1, whose default branch is main, holding one
// commit (base) with a.txt, and whose changes are never restacked
// automatically; dana's task fork api--f1 and mel's task fork api--m1 of
// api, made from base, whose main is at base, each with a PAT of its
// owner bound to it with changes:read. The mergeability checks of the
// changes are not spaced, and the change component writes its events
// through readyGate.
type world struct {
	*hubtest.Hub
	events     *event.Component
	changes    *change.Component
	policy     *policy.Component
	workspaces *workspace.Service
	// readyGate is the change component's event writer.
	readyGate              *readyGate
	org                    string
	dana, mel, oz          string
	danaKey, melKey, ozKey string
	// danaF1 and melM1 are the PATs bound to api--f1 and api--m1.
	danaF1, melM1 string
	// api is the git directory of api's repository, and base the commit
	// its main is at.
	api, base string
}

// newWorld starts the world's hub with adjust run on the policy
// component's dependencies.
func newWorld(t *testing.T, adjust ...func(*policy.Deps)) *world {
	t.Helper()
	w := &world{}
	w.Hub = hubtest.StartWith(t, hubtest.Config{TOML: "[changes]\nmergeable_debounce = \"0s\"\n", Compose: func(c *hub.Composition) {
		w.events, w.changes, w.policy, w.workspaces = c.Events(), c.Changes(), c.Policy(), c.Workspaces()
		c.AdjustChange(func(d *change.Deps) {
			w.readyGate = &readyGate{EventWriter: d.Events}
			d.Events = w.readyGate
		})
		for _, fn := range adjust {
			c.AdjustPolicy(fn)
		}
	}})
	w.org = w.CreateOrg(t, "o1")
	for _, u := range []struct {
		name    string
		id, key *string
		member  bool
	}{{"dana", &w.dana, &w.danaKey, true}, {"mel", &w.mel, &w.melKey, true}, {"oz", &w.oz, &w.ozKey, false}} {
		*u.id = w.CreateUser(t, u.name)
		*u.key = w.IssueKey(t, *u.id)
		if u.member {
			w.AddMember(t, w.org, *u.id)
		}
	}
	w.api = w.addWorkspace(t, "api", "")
	w.base = gitrepotest.Refs(t, w.api, "refs/heads/")["main"]
	w.addWorkspace(t, "web", "")
	w.addWorkspace(t, "api--f1", w.dana)
	w.addWorkspace(t, "api--m1", w.mel)
	w.danaF1 = w.IssuePAT(t, identity.PATSpec{UserID: w.dana, Permissions: []string{"changes:read"}, Workspace: "api--f1"})
	w.melM1 = w.IssuePAT(t, identity.PATSpec{UserID: w.mel, Permissions: []string{"changes:read"}, Workspace: "api--m1"})
	return w
}

// addWorkspace stores the ready record of slug in o1, dana's repository
// workspace for owner "", else a task fork of api owned by owner, and
// returns its git directory.
func (w *world) addWorkspace(t *testing.T, slug, owner string) string {
	t.Helper()
	ctx, now, dir := context.Background(), time.Now().UTC(), w.workspaces.RepoPath(slug)
	rec := workspace.Workspace{
		Slug: slug, Kind: access.KindRepository, Mode: workspace.ModeStandard, DefaultBranch: new("main"),
		OwnerID: w.dana, OrgID: w.org, Status: workspace.StatusReady, DisplayName: slug,
		Settings:  map[string]json.RawMessage{"changes.auto_restack": json.RawMessage("false")},
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
	}
	if owner == "" {
		gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
		rec.HeadSHA = new(commit(t, dir, "main", "", map[string]string{"a.txt": "a\n"}))
	} else {
		if err := (gitrepo.Git{}).CreateFork(ctx, w.api, dir); err != nil {
			t.Fatal(err)
		}
		gitrepotest.Git(t, dir, "update-ref", "refs/heads/main", w.base)
	}
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		if owner == "" {
			rec.GitURL = new("https://origin.test/" + slug + ".git")
			return workspace.Insert(ctx, tx, rec)
		}
		rec.Kind, rec.OwnerID, rec.ParentSlug, rec.Settings = access.KindFork, owner, new("api"), nil
		rec.ForkPoint = &workspace.ForkPoint{Ref: "main", SHA: w.base}
		rec.ExpiresAt, rec.AutoChange = new(now.Add(7*24*time.Hour)), new(false)
		return fork.Insert(ctx, tx, fork.Fork{Workspace: rec})
	})
	if err != nil {
		t.Fatalf("store the workspace %s: %v", slug, err)
	}
	return dir
}

// commit writes to the repository at dir a commit on top of from (a
// commit id or a branch), or a root commit for from "", that sets each
// file of files to its content and then runs the fast-import file
// commands of more ("R old new" renames a file), points branch at it and
// returns its id.
func commit(t *testing.T, dir, branch, from string, files map[string]string, more ...string) string {
	t.Helper()
	const committer = "committer T <t@example.invalid> 1700000000 +0000\n"
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	var s strings.Builder
	s.WriteString("commit refs/heads/" + branch + "\n" + committer + data(branch))
	if from != "" {
		s.WriteString("from " + from + "\n")
	}
	for _, path := range slices.Sorted(maps.Keys(files)) {
		s.WriteString("M 100644 inline " + path + "\n" + data(files[path]))
	}
	for _, cmd := range more {
		s.WriteString(cmd + "\n")
	}
	if _, err := gitrepotest.Run(dir, s.String()+"\n", "fast-import", "--quiet", "--force"); err != nil {
		t.Fatal(err)
	}
	return gitrepotest.Refs(t, dir, "refs/heads/")[branch]
}

// danaCaller is dana with an API key of hers, as the change component
// names a caller.
func (w *world) danaCaller() change.Caller {
	return change.Caller{UserID: w.dana, Username: "dana", Credential: change.Credential{Type: change.CredentialAPIKey, ID: "k1"}}
}

// open opens, through the change component, by dana, a change of the
// workspace slug from its branch, whose commits the test made, with adjust
// run on the input, and returns it.
func (w *world) open(t *testing.T, slug, branch string, adjust ...func(*change.OpenInput)) change.Change {
	t.Helper()
	in := change.OpenInput{Workspace: slug, SourceWorkspace: slug, SourceBranch: branch, Intent: new("why"),
		Kind: change.KindRef, Caller: w.danaCaller()}
	for _, fn := range adjust {
		fn(&in)
	}
	var ch change.Change
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		ch, err = w.changes.Open(ctx, tx, in)
		return err
	}); err != nil {
		t.Fatalf("open %s of %s: %v", branch, slug, err)
	}
	return ch
}

// inTx runs fn in a store transaction and fails the test on its error.
func (w *world) inTx(t *testing.T, fn func(ctx context.Context, tx *sql.Tx) error) {
	t.Helper()
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return fn(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
}

// readChange is the change number of the workspace slug as it is stored.
func (w *world) readChange(t *testing.T, slug string, number int64) change.Change {
	t.Helper()
	ch, err := w.changes.ReadChange(context.Background(), nil, slug, number)
	if err != nil {
		t.Fatalf("read %s: %v", change.Ref(slug, number), err)
	}
	return ch
}

// checked waits until the mergeability check of the change number of the
// workspace slug has computed its mergeable, at most 30 s, and returns
// the change as it is stored then; until another trigger nothing writes
// it again.
func (w *world) checked(t *testing.T, slug string, number int64) change.Change {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if ch := w.readChange(t, slug, number); ch.Mergeable != change.MergeableUnknown {
			return ch
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mergeable of %s stayed unknown", change.Ref(slug, number))
		}
	}
}

// seq is the sequence number of the last hub event.
func (w *world) seq(t *testing.T) int64 {
	t.Helper()
	recs, _, err := w.events.ReadAfter(context.Background(), 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		return 0
	}
	return recs[len(recs)-1].Seq
}

// eventsAfter are the hub events after seq, oldest first, of the type typ,
// any type for "".
func (w *world) eventsAfter(t *testing.T, seq int64, typ string) []event.Record {
	t.Helper()
	recs, _, err := w.events.ReadAfter(context.Background(), seq, 100000)
	if err != nil {
		t.Fatal(err)
	}
	var out []event.Record
	for _, r := range recs {
		if typ == "" || r.Type == typ {
			out = append(out, r)
		}
	}
	return out
}

// readyEvents are the parley.change.ready events of the change number of
// api written after seq.
func (w *world) readyEvents(t *testing.T, seq, number int64) []event.Record {
	t.Helper()
	var out []event.Record
	for _, r := range w.eventsAfter(t, seq, change.EventReady) {
		if r.ResourceID == strconv.FormatInt(number, 10) && r.Workspace != nil && *r.Workspace == "api" {
			out = append(out, r)
		}
	}
	return out
}

// readyGate passes the change component's events on, and refuses
// parley.change.ready while fail is set, as a log that cannot be written.
type readyGate struct {
	change.EventWriter
	fail atomic.Bool
}

func (g *readyGate) Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error) {
	if in.Type == change.EventReady && g.fail.Load() {
		return event.Record{}, errors.New("the event log is full")
	}
	return g.EventWriter.Emit(ctx, tx, in)
}

// evidence is an evidence source for the changes of api: for the change
// number at version version (0 for any) it counts approvals approvals and,
// with pass set, has every check it is asked for passed; it fails for the
// change fail, and has nothing for any other. It records every query.
type evidence struct {
	mu                    sync.Mutex
	number, version, fail int64
	approvals             int
	pass                  bool
	asked                 []policy.EvidenceQuery
}

// count sets the approvals the source counts.
func (e *evidence) count(n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.approvals = n
}

func (e *evidence) Evidence(_ context.Context, _ policy.Querier, in policy.EvidenceQuery) (policy.EvidenceAnswer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.asked = append(e.asked, in)
	switch {
	case in.Number == e.fail:
		return policy.EvidenceAnswer{}, errors.New("the evidence cannot be read")
	case e.number != 0 && in.Number != e.number, e.version != 0 && in.Version != e.version:
		return policy.EvidenceAnswer{}, nil
	}
	answer := policy.EvidenceAnswer{Counted: e.approvals}
	if e.pass {
		answer.Checks = map[string]string{}
		for _, c := range in.Checks {
			answer.Checks[c] = "passed"
		}
	}
	return answer, nil
}

// setStatus sets the status of slug, which no clone worker acts on once the
// hub has started.
func (w *world) setStatus(t *testing.T, slug string, status workspace.Status) {
	t.Helper()
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET status = ? WHERE slug = ?`, status, slug)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// byDana and byAdmin are the writers of a revision dana's API key and the
// admin token write.
func (w *world) byDana() policy.Writer {
	return policy.Writer{ActorType: "api_key", ActorID: new(w.dana), Username: new("dana")}
}

var byAdmin = policy.Writer{ActorType: "admin_token"}

// setPolicy stores raw, validated, as the next revision of slug, written by
// by at at, through the store function the replacement writes with.
func (w *world) setPolicy(t *testing.T, slug, raw string, by policy.Writer, at time.Time) {
	t.Helper()
	doc, err := validate(t, raw)
	if err != nil {
		t.Fatalf("validate %s: %v", raw, err)
	}
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := policy.Replace(ctx, tx, slug, *doc, nil, by, at)
		return err
	}); err != nil {
		t.Fatalf("store a revision of %s: %v", slug, err)
	}
}

// answer is a hub's answer: its status, its headers, its body as sent and
// as decoded JSON.
type answer struct {
	status int
	header http.Header
	body   []byte
	json   map[string]any
}

// errorType is the error_type of an error envelope, "" for any other body.
func (a answer) errorType() string {
	e, _ := a.json["error"].(map[string]any)
	s, _ := e["error_type"].(string)
	return s
}

// get sends GET path to the hub with credential.
func (w *world) get(t *testing.T, path, credential string) answer {
	t.Helper()
	return w.send(t, http.MethodGet, path, credential, nil, nil)
}

// put sends PUT path to the hub with credential and the document doc,
// with If-Match set to ifMatch unless it is "".
func (w *world) put(t *testing.T, path, credential, doc, ifMatch string) answer {
	t.Helper()
	var header http.Header
	if ifMatch != "" {
		header = http.Header{"If-Match": {ifMatch}}
	}
	return w.send(t, http.MethodPut, path, credential, doc, header)
}

// send sends the request to the hub; t.Errorf, not t.Fatalf, reports an
// answer that is not JSON, so that it may run outside the test's goroutine.
func (w *world) send(t *testing.T, method, path, credential string, body any, header http.Header) answer {
	t.Helper()
	status, h, raw := w.Exchange(t, method, path, credential, body, header)
	a := answer{status: status, header: h, body: raw}
	if err := json.Unmarshal(raw, &a.json); err != nil {
		t.Errorf("%s %s = %d %s: not a JSON object: %v", method, path, status, raw, err)
	}
	return a
}

// asJSON is v encoded and decoded as JSON, for comparing with a decoded
// body.
func asJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

// pushed is a change of api opened by dana with her PAT patPush, which
// holds git:write, changes:read, changes:write and changes:approve, so that
// version 1 is created by that credential; patPushID is the PAT's id.
type pushed struct {
	number             int64
	patPush, patPushID string
}

// openPushed opens a change of api from a new branch of base that adds the
// file <branch>.txt, by dana with a new PAT patPush, with adjust run on the
// input.
func (w *world) openPushed(t *testing.T, branch string, adjust ...func(*change.OpenInput)) pushed {
	t.Helper()
	var p pushed
	p.patPush, p.patPushID = w.IssuePATID(t, identity.PATSpec{UserID: w.dana,
		Permissions: []string{"git:write", "changes:read", "changes:write", "changes:approve"}})
	commit(t, w.api, branch, w.base, map[string]string{branch + ".txt": branch + "\n"})
	byPAT := func(in *change.OpenInput) {
		in.Caller = change.Caller{UserID: w.dana, Username: "dana", Credential: change.Credential{Type: change.CredentialPAT, ID: p.patPushID}}
	}
	p.number = w.open(t, "api", branch, append([]func(*change.OpenInput){byPAT}, adjust...)...).Number
	return p
}

// approvalsPath is the path of the verdicts of the change number of api.
func approvalsPath(number int64) string {
	return "/api/v1/workspaces/api/changes/" + strconv.FormatInt(number, 10) + "/approvals"
}

// postVerdict sends body as a verdict on the change number of api with
// credential.
func (w *world) postVerdict(t *testing.T, number int64, credential string, body any) answer {
	t.Helper()
	return w.send(t, http.MethodPost, approvalsPath(number), credential, body, nil)
}

// count is the number of rows of the store table table.
func (w *world) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := w.Store.Main().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// verdictIDs are the ids of the verdicts stored of the user on version
// version of the change number of api.
func (w *world) verdictIDs(t *testing.T, number, version int64, user string) []string {
	t.Helper()
	rows, err := w.Store.Main().Query(`SELECT id FROM change_approvals
WHERE workspace_slug = 'api' AND number = ? AND version = ? AND user_id = ? ORDER BY id`, number, version, user)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// withdraw sends DELETE of the verdict id of the change number of api with
// credential. A withdrawal answers 204 with no body, so the body is decoded
// only when there is one.
func (w *world) withdraw(t *testing.T, number int64, id, credential string) answer {
	t.Helper()
	status, h, raw := w.Exchange(t, http.MethodDelete, approvalsPath(number)+"/"+id, credential, nil, nil)
	a := answer{status: status, header: h, body: raw}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a.json); err != nil {
			t.Errorf("DELETE %s = %d %s: not a JSON object: %v", id, status, raw, err)
		}
	}
	return a
}

// listedIDs are the ids of the verdicts of the change number of api that
// the list answers credential, newest first.
func (w *world) listedIDs(t *testing.T, number int64, credential string) []string {
	t.Helper()
	r := w.get(t, approvalsPath(number), credential)
	if r.status != http.StatusOK {
		t.Fatalf("list the verdicts of %d = %d %s, want 200", number, r.status, r.body)
	}
	var ids []string
	for _, item := range r.json["items"].([]any) {
		ids = append(ids, item.(map[string]any)["id"].(string))
	}
	return ids
}

// stepClock is a hub clock that stands still until the test moves it on.
type stepClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// tick moves the clock on by one second.
func (c *stepClock) tick() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(time.Second)
}
