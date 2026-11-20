package acceptance

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// claims is the path of demo's claims.
const claims = "/api/v1/workspaces/demo/claims"

// claimEvents is the stream query of demo's claim events: each claim
// creation writes exactly one, and nothing the hub does in the background
// writes one, so they are the scenarios' event source.
const claimEvents = "workspace=demo&types=parley.claim.*"

// newClaim creates a claim on demo's docs with credential and returns its
// id.
func newClaim(t *testing.T, hub *harness.Hub, credential string) string {
	t.Helper()
	r := hub.Call(http.MethodPost, claims, credential, map[string]any{"paths": []string{"docs/**"}, "intent": "write the docs"}).
		Want(t, http.StatusCreated)
	id := harness.Str(harness.Field(r.Body, "claim.id"))
	if id == "" {
		t.Fatalf("the claim's answer holds no id: %s", r.Raw)
	}
	return id
}

// walk is every item of the list at path, with its query, read with
// credential 200 at a time by following next_cursor to its end.
func walk(t *testing.T, hub *harness.Hub, credential, path string) []map[string]any {
	t.Helper()
	u, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("limit", "200")
	var all []map[string]any
	for {
		u.RawQuery = q.Encode()
		page := hub.Get(credential, u.String())
		all = append(all, page.Items()...)
		next := harness.Str(page.Body["next_cursor"])
		if next == "" {
			return all
		}
		q.Set("cursor", next)
	}
}

// eventLog is every event of the event log that credential sees after the
// seq after, oldest first, with the filters of query (such as
// "workspace=demo", "" for none). The log's cursor is its position, so
// the walk starts with it.
func eventLog(t *testing.T, hub *harness.Hub, credential, query string, after int64) []map[string]any {
	t.Helper()
	return walk(t, hub, credential, "/api/v1/events/log?cursor="+strconv.FormatInt(after, 10)+"&"+query)
}

// seqOf is the seq of the event record e.
func seqOf(e map[string]any) int64 { return int64(harness.Num(e["seq"])) }

// ascending reports whether seqs are strictly ascending.
func ascending(seqs []int64) bool {
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			return false
		}
	}
	return true
}

func TestEvents_ResumeDeliversExactlyTheMissedFiftyThenLive(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	reader := hub.Events(key, claimEvents)
	newClaim(t, hub, key)
	s0 := reader.Next(1, 10*time.Second)[0].Seq
	reader.Close()

	var ids []string
	for range 50 {
		ids = append(ids, newClaim(t, hub, key))
	}
	reader2 := hub.Events(key, claimEvents, "Last-Event-ID", strconv.FormatInt(s0, 10))
	got := reader2.Next(50, 10*time.Second)
	var gotIDs []string
	seqs := []int64{s0}
	for _, e := range got {
		gotIDs = append(gotIDs, harness.Str(e.Record["resource_id"]))
		seqs = append(seqs, e.Seq)
	}
	if !slices.Equal(gotIDs, ids) {
		t.Errorf("the resumed stream replayed the claims %v, want the fifty missed %v", gotIDs, ids)
	}
	if !ascending(seqs) {
		t.Errorf("the resumed stream replayed the seqs %v after %d, want them strictly ascending above it", seqs[1:], s0)
	}
	live := newClaim(t, hub, key)
	e := reader2.Next(1, 5*time.Second)[0]
	if harness.Str(e.Record["resource_id"]) != live || e.Seq <= got[49].Seq {
		t.Errorf("the event after the replay names %v with seq %d, want the live claim %s above %d", e.Record["resource_id"], e.Seq, live, got[49].Seq)
	}

	lines := hub.Tail(key, 51, 10*time.Second, "--workspace", "demo", "--types", "parley.claim.*", "--after", strconv.FormatInt(s0, 10))
	var tailed []string
	for _, line := range lines {
		var rec struct {
			ResourceID string `json:"resource_id"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("parley events tail printed %q, want an event record: %v", line, err)
		}
		tailed = append(tailed, rec.ResourceID)
	}
	if want := append(slices.Clone(ids), live); !slices.Equal(tailed, want) {
		t.Errorf("parley events tail --after %d printed the claims %v, want %v", s0, tailed, want)
	}
}

func TestLong_CursorOlderThanRetentionIsExpired(t *testing.T) {
	harness.Require(t, harness.Long)
	t.Parallel()
	hub := harness.Start(t, harness.Options{TOML: "[events]\nretention = \"1h\"\n"})
	key := hub.Workspace(hub.User("alice"), "demo").Owner.Key
	newClaim(t, hub, key)
	created := eventLog(t, hub, key, "types=parley.claim.create", 0)
	if len(created) == 0 {
		t.Fatal("the log holds no parley.claim.create of the first claim")
	}
	s0 := seqOf(created[0])
	for range 5 {
		newClaim(t, hub, key)
	}
	// The retention is at least an hour and the hub's clock is its own:
	// only a real wait takes the early events past it.
	time.Sleep(65 * time.Minute)
	newClaim(t, hub, key)
	hub.Stop()
	hub.Start()

	after := "/api/v1/events/log?after=" + strconv.FormatInt(s0, 10)
	var r harness.Response
	// The retention runs in the maintenance pass the start begins in the
	// background.
	harness.Eventually(t, "the position "+strconv.FormatInt(s0, 10)+" to expire", func() bool {
		r = hub.Call(http.MethodGet, after, key, nil)
		return r.Status == http.StatusGone
	})
	oldest := int64(harness.Num(harness.Field(r.Body, "error.oldest_seq")))
	if r.ErrorType() != "cursor_expired" || oldest <= s0 {
		t.Errorf("GET %s answered %s, want cursor_expired with an oldest_seq above %d", after, r.Raw, s0)
	}
	stream := hub.EventsRefused(key, "", "Last-Event-ID", strconv.FormatInt(s0, 10))
	if stream.Status != http.StatusGone || stream.ErrorType() != "cursor_expired" ||
		int64(harness.Num(harness.Field(stream.Body, "error.oldest_seq"))) <= s0 {
		t.Errorf("the stream resumed from %d answered %d %s, want 410 cursor_expired with an oldest_seq above it", s0, stream.Status, stream.Raw)
	}
	hub.Get(key, "/api/v1/events/log?after="+strconv.FormatInt(oldest-1, 10))
}

func TestEvents_CarryTheSessionOfTheOperation(t *testing.T) {
	t.Parallel()
	hub, demo := demoHub(t)
	key := demo.Owner.Key
	seedFiveLines(t, hub, key)
	session := harness.Str(hub.CLI(key, "session", "start", demo.Slug).Must(t).JSON()["id"])
	if session == "" {
		t.Fatal("parley session start printed no id")
	}
	fork := hub.Fork(key, demo.Slug, forkPermissions, map[string]any{"session_id": session})

	clone := hub.Clone(fork.Remote, fork.Token)
	hub.Git(clone, "", "checkout", "-q", "-b", "w1").Must(t)
	hub.CommitFiles(clone, "Change line 3 in w1", map[string]string{"src/a.txt": lineThree("line 3 from w1")})
	w1 := changeOfPush(t, hub.PushChange(clone, "", "w1", "change line 3", "parley.session="+session).Must(t))
	hub.Git(clone, "", "checkout", "-q", "-b", "w2", "main").Must(t)
	hub.CommitFiles(clone, "Change line 3 in w2", map[string]string{"src/a.txt": lineThree("line 3 from w2")})
	w2 := changeOfPush(t, hub.PushChange(clone, "", "w2", "change line 3 otherwise", "parley.session="+session).Must(t))

	hub.Call(http.MethodPost, landPath(w1), key, map[string]any{"session_id": session}).Want(t, http.StatusOK)
	harness.Eventually(t, "change "+strconv.Itoa(w2)+" to be mergeable conflict", func() bool {
		return hub.Get(key, changePath(w2, "")).Body["mergeable"] == "conflict"
	})

	events := eventLog(t, hub, key, "session_id="+session, 0)
	types := map[string]bool{}
	for _, e := range events {
		types[harness.Str(e["type"])] = true
		if e["session_id"] != session {
			t.Errorf("the session's log holds %v %v with the session %v, want %s", e["type"], e["resource_id"], e["session_id"], session)
		}
	}
	for _, want := range []string{"parley.workspace.fork", "parley.git.push", "parley.change.version", "parley.change.land", "parley.change.conflict"} {
		if !types[want] {
			t.Errorf("the log of the session %s holds no %s; it holds %v", session, want, types)
		}
	}
}

func TestEvents_NoneMissingAfterKillUnderLoad(t *testing.T) {
	t.Parallel()
	// Eight writers reach the default limit of 1000 active claims of a
	// workspace within the two seconds, and every claim after it is
	// refused; the scenario is about the writes, not the limit.
	hub := harness.Start(t, harness.Options{TOML: "[awareness]\nmax_claims_per_workspace = 1000000\n"})
	key := hub.Workspace(hub.User("alice"), "demo").Owner.Key
	reader := hub.Events(key, "workspace=demo")

	var mu sync.Mutex
	acknowledged := map[string]bool{}
	var stop atomic.Bool
	wait := harness.Release(8, func(int) {
		for !stop.Load() {
			r := hub.Call(http.MethodPost, claims, key, map[string]any{"paths": []string{"docs/**"}, "intent": "write the docs"})
			if r.Err != nil {
				return // the hub is gone
			}
			if r.Status == http.StatusCreated {
				mu.Lock()
				acknowledged[harness.Str(harness.Field(r.Body, "claim.id"))] = true
				mu.Unlock()
			}
		}
	})
	// The kill has to come while the writers are writing: it is the
	// scenario's real time, not a wait for the hub.
	time.Sleep(2 * time.Second)
	hub.Kill()
	stop.Store(true)
	wait()
	maxSeen := reader.MaxSeq()
	hub.Start()

	active := map[string]bool{}
	for _, c := range walk(t, hub, key, claims+"?state=active") {
		active[harness.Str(c["id"])] = true
	}
	created := eventLog(t, hub, key, "workspace=demo&types=parley.claim.create", 0)
	named := map[string]bool{}
	var maxCreated int64
	for _, e := range created {
		id := harness.Str(e["resource_id"])
		if named[id] {
			t.Errorf("the log holds two parley.claim.create events of the claim %s", id)
		}
		named[id] = true
		maxCreated = max(maxCreated, seqOf(e))
		if !active[id] {
			t.Errorf("the log's parley.claim.create of %s names no active claim", id)
		}
	}
	for id := range active {
		if !named[id] {
			t.Errorf("the active claim %s has no parley.claim.create in the log", id)
		}
	}
	mu.Lock()
	for id := range acknowledged {
		if !active[id] {
			t.Errorf("the claim %s was answered 201 before the kill and is not there after the restart", id)
		}
	}
	t.Logf("%d claims answered 201, %d active after the restart, highest seq streamed before the kill %d", len(acknowledged), len(active), maxSeen)
	mu.Unlock()
	if len(acknowledged) == 0 {
		t.Fatal("no claim was answered 201 before the kill")
	}
	var seqs []int64
	for _, e := range eventLog(t, hub, key, "", 0) {
		seqs = append(seqs, seqOf(e))
	}
	if !ascending(seqs) {
		t.Errorf("the log's seqs are not strictly ascending across the restart: %v", seqs)
	}

	n := newClaim(t, hub, key)
	var e map[string]any
	for _, c := range eventLog(t, hub, key, "workspace=demo&types=parley.claim.create", maxCreated) {
		if c["resource_id"] == n {
			e = c
		}
	}
	if e == nil {
		t.Fatalf("the claim %s created after the restart has no parley.claim.create after the seq %d", n, maxCreated)
	}
	if s := seqOf(e); s <= maxSeen || s <= maxCreated {
		t.Errorf("the first event after the restart has the seq %d, want it above %d, streamed before the kill, and %d, logged before it", s, maxSeen, maxCreated)
	}
}
