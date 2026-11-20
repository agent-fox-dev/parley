package perf_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/test/perf"
)

// stubToken is the credential the harness sends to the stub; a test looks
// for it in everything the harness writes.
const stubToken = "parley_perf_stub_token_5f0c2a9e"

// stubVersion is the document the stub answers GET /version with.
const stubVersion = `{"version":"1.4.0","commit":"3f2a1bc","build_time":"2026-11-20T00:00:00Z"}`

// stubDefaultBranch is the default branch of every workspace of the stub.
const stubDefaultBranch = "main"

// scenarioNames are the fourteen scenarios in the order of the report.
var scenarioNames = []string{
	"fork_create", "file_read", "commit_create", "land", "land_throughput", "land_concurrency", "mergeability",
	"event_latency", "active_forks", "open_changes", "event_streams", "sandboxes_running", "sandbox_ready", "sandbox_exec",
}

// stubOptions change how the stub answers; the zero value answers every
// request at once with a success.
type stubOptions struct {
	// forkDelay is how long the n-th fork creation, counting from 1, waits
	// before it answers.
	forkDelay func(n int) time.Duration
	// failForkDeletes answers the first so many fork deletions with 500.
	failForkDeletes int
	// failCommitsFrom answers the commit route with 500 from its request
	// of that number on, 0 for never.
	failCommitsFrom int
	// landDelay is how long a land request waits before it answers.
	landDelay time.Duration
	// landFails ends the n-th land attempt of the scenario, counting from
	// 1, as failed.
	landFails func(scenario string, n int) bool
	// mergeDelay is how long after the land of the mergeability scenario's
	// extra change its other changes stay mergeable unknown.
	mergeDelay time.Duration
	// eventDelay is how long after a commit's answer its event is sent on
	// the open event streams.
	eventDelay time.Duration
	// withholdEvent is the commit of event_latency, counting from 1, whose
	// event is never sent, 0 for none.
	withholdEvent int
	// forkLimit is how many forks of one workspace may exist at once: the
	// next creation is answered with 409 and stubForkLimit. 0 is no limit.
	forkLimit int
	// sandboxLimit is how many sandboxes may exist at once: the next
	// creation is answered with 429 sandbox_capacity. 0 is no limit.
	sandboxLimit int
	// provisioning is how long a sandbox shows state provisioning after it
	// was created, before it shows ready.
	provisioning time.Duration
	// failSandbox is the sandbox creation, counting from 1, whose sandbox
	// shows state failed with state_reason image_pull, 0 for none.
	failSandbox int
}

// stubForkLimit is the error_type of the stub's refusal of a fork beyond
// forkLimit, one the hub does not use.
const stubForkLimit = "stub_fork_limit"

// stub stands in for a hub: it answers GET /version and the routes the
// scenarios call, each with a success unless the test overrides it, and
// records what it was sent.
type stub struct {
	*httptest.Server
	opts stubOptions
	mu   sync.Mutex
	// seen is "METHOD path" of every request, in the order they came.
	seen []string
	// unauthorized is "METHOD path" of every request that did not carry
	// the bearer token.
	unauthorized []string
	workspaces   map[string]*stubWorkspace
	// forks are the forks the stub created and nobody deleted yet, each
	// with the workspace it was forked from.
	forks        map[string]string
	nextFork     int
	forkRequests int
	forkDeletes  int
	nextSHA      int
	// refs are the names of the branches created through the refs route.
	refs           []string
	commits        []stubCommit
	commitRequests int
	eventCommits   int
	changes        map[int64]*stubChange
	nextChange     int64
	lands          map[string]*landStats
	// mergeLandedAt is when the mergeability scenario's extra change
	// landed, and mergePolls when each list of its unknown changes came.
	mergeLandedAt time.Time
	mergePolls    []time.Time
	// streams are the open event streams; streamsOpened counts every one
	// and streamsMax is the most that were open at once.
	streams       map[chan string]bool
	streamsOpened int
	streamsMax    int
	nextSeq       int
	sandboxes     map[string]*stubSandbox
	nextSandbox   int
	// profiles are the profiles the sandbox creations named.
	profiles []string
}

type stubWorkspace struct {
	// branches are the heads of the branches, by name without refs/heads/.
	branches map[string]string
}

// stubCommit is a commit request the stub was sent.
type stubCommit struct {
	workspace, branch string
	// paths are the paths of its file operations.
	paths []string
}

type stubChange struct {
	number                                     int64
	workspace, source, target, scenario, state string
}

// landStats are the land requests of one scenario.
type landStats struct {
	inFlight, max, count int
	// first is when the first came, last when the last was answered.
	first, last time.Time
}

type stubSandbox struct {
	id, workspace, profile string
	created                time.Time
	// failed shows the sandbox as failed from its creation on.
	failed bool
}

// newStub serves a stub until the test ends. override replaces the
// default handler of a route, keyed by its http.ServeMux pattern.
func newStub(t *testing.T, override map[string]http.HandlerFunc) *stub {
	t.Helper()
	return newStubWith(t, stubOptions{}, override)
}

// newStubWith is newStub answering as opts say.
func newStubWith(t *testing.T, opts stubOptions, override map[string]http.HandlerFunc) *stub {
	t.Helper()
	s := &stub{opts: opts, workspaces: map[string]*stubWorkspace{}, forks: map[string]string{},
		changes: map[int64]*stubChange{}, lands: map[string]*landStats{}, streams: map[chan string]bool{},
		sandboxes: map[string]*stubSandbox{}}
	routes := map[string]http.HandlerFunc{
		"GET /version": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, stubVersion)
		},
		"GET /api/v1/workspaces/{slug}":        s.getWorkspace,
		"POST /api/v1/workspaces/{slug}/forks": s.createFork,
		"DELETE /api/v1/workspaces/{slug}":     s.deleteWorkspace,
		"GET /api/v1/workspaces/{slug}/scm/tree": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"items": []map[string]any{
				{"path": "src", "name": "src", "type": "tree", "mode": "040000", "sha": strings.Repeat("a", 40)},
				{"path": "README.md", "name": "README.md", "type": "blob", "mode": "100644", "sha": strings.Repeat("b", 40), "size": 6},
			}, "next_cursor": nil})
		},
		"GET /api/v1/workspaces/{slug}/scm/file": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "hello\n")
		},
		"POST /api/v1/workspaces/{slug}/scm/refs":                 s.createRef,
		"POST /api/v1/workspaces/{slug}/scm/commits":              s.createCommit,
		"POST /api/v1/workspaces/{slug}/changes":                  s.openChange,
		"GET /api/v1/workspaces/{slug}/changes":                   s.listChanges,
		"POST /api/v1/workspaces/{slug}/changes/{number}/land":    s.land,
		"POST /api/v1/workspaces/{slug}/changes/{number}/abandon": s.abandon,
		"GET /api/v1/events":                                      s.events,
		"POST /api/v1/workspaces/{slug}/sandboxes":                s.createSandbox,
		"GET /api/v1/workspaces/{slug}/sandboxes/{id}":            s.getSandbox,
		"DELETE /api/v1/workspaces/{slug}/sandboxes/{id}":         s.deleteSandbox,
		"POST /api/v1/workspaces/{slug}/sandboxes/{id}/exec":      s.exec,
	}
	for pattern, h := range override {
		if _, ok := routes[pattern]; !ok {
			t.Fatalf("the stub has no route %q to override", pattern)
		}
		routes[pattern] = h
	}
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+stubToken {
			s.unauthorized = append(s.unauthorized, r.Method+" "+r.URL.Path)
		}
		s.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		// A stream the harness left open would hold Close forever.
		s.CloseClientConnections()
		s.Close()
	})
	return s
}

// workspace is the workspace slug, made with its default branch when the
// stub does not know it yet. The caller holds s.mu.
func (s *stub) workspace(slug string) *stubWorkspace {
	ws, ok := s.workspaces[slug]
	if !ok {
		ws = &stubWorkspace{branches: map[string]string{stubDefaultBranch: s.newSHA()}}
		s.workspaces[slug] = ws
	}
	return ws
}

// newSHA is a commit id the stub has not handed out. The caller holds s.mu.
func (s *stub) newSHA() string {
	s.nextSHA++
	return fmt.Sprintf("%040x", s.nextSHA)
}

func (s *stub) getWorkspace(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slug := r.PathValue("slug")
	ws := s.workspace(slug)
	writeJSON(w, http.StatusOK, map[string]any{"slug": slug, "kind": "registered",
		"default_branch": stubDefaultBranch, "head_sha": ws.branches[stubDefaultBranch]})
}

func (s *stub) createFork(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.forkRequests++
	n := s.forkRequests
	s.mu.Unlock()
	if s.opts.forkDelay != nil {
		time.Sleep(s.opts.forkDelay(n))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	parent := r.PathValue("slug")
	if s.opts.forkLimit > 0 {
		live := 0
		for _, p := range s.forks {
			if p == parent {
				live++
			}
		}
		if live >= s.opts.forkLimit {
			writeError(w, http.StatusConflict, stubForkLimit, fmt.Sprintf("%s has %d forks", parent, live))
			return
		}
	}
	s.nextFork++
	slug := fmt.Sprintf("%s-f%d", parent, s.nextFork)
	head := s.workspace(parent).branches[stubDefaultBranch]
	s.workspaces[slug] = &stubWorkspace{branches: map[string]string{stubDefaultBranch: head}}
	s.forks[slug] = parent
	writeJSON(w, http.StatusCreated, map[string]any{"workspace": map[string]any{"slug": slug, "kind": "fork",
		"default_branch": stubDefaultBranch, "head_sha": head}, "remote": nil})
}

func (s *stub) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slug := r.PathValue("slug")
	if _, ok := s.forks[slug]; !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such workspace")
		return
	}
	s.forkDeletes++
	if s.forkDeletes <= s.opts.failForkDeletes {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	delete(s.forks, slug)
	delete(s.workspaces, slug)
	w.WriteHeader(http.StatusNoContent)
}

func (s *stub) createRef(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		Ref  string `json:"ref"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.Name == "" || body.Ref == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name and ref are required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs = append(s.refs, body.Name)
	ws := s.workspace(r.PathValue("slug"))
	if _, ok := ws.branches[body.Name]; ok {
		writeError(w, http.StatusConflict, "ref_exists", "ref refs/heads/"+body.Name+" already exists")
		return
	}
	sha, ok := ws.branches[body.Ref]
	if !ok {
		writeError(w, http.StatusNotFound, "ref_not_found", "revision "+body.Ref+" names no commit")
		return
	}
	ws.branches[body.Name] = sha
	writeJSON(w, http.StatusCreated, map[string]any{"name": "refs/heads/" + body.Name, "kind": "branch", "sha": sha})
}

// createCommit moves the branch as the hub does: an existing branch only
// from expected_head_sha, a new one from base_ref.
func (s *stub) createCommit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Branch          string `json:"branch"`
		BaseRef         string `json:"base_ref"`
		ExpectedHeadSHA string `json:"expected_head_sha"`
		Message         string `json:"message"`
		Operations      []struct {
			Op      string `json:"op"`
			Path    string `json:"path"`
			Content string `json:"content"`
		} `json:"operations"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.Branch == "" || body.Message == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "branch and message are required")
		return
	}
	slug := r.PathValue("slug")
	c := stubCommit{workspace: slug, branch: body.Branch}
	for _, op := range body.Operations {
		if op.Op != "put" || op.Path == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "an operation is no put of a path")
			return
		}
		c.paths = append(c.paths, op.Path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits = append(s.commits, c)
	s.commitRequests++
	if s.opts.failCommitsFrom > 0 && s.commitRequests >= s.opts.failCommitsFrom {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	ws := s.workspace(slug)
	parent, exists := ws.branches[body.Branch]
	switch {
	case exists && body.ExpectedHeadSHA != parent:
		writeError(w, http.StatusConflict, "ref_moved", "refs/heads/"+body.Branch+" is at "+parent)
		return
	case !exists:
		base, ok := ws.branches[body.BaseRef]
		if !ok {
			writeError(w, http.StatusNotFound, "ref_not_found", "revision "+body.BaseRef+" names no commit")
			return
		}
		parent = base
	}
	sha := s.newSHA()
	ws.branches[body.Branch] = sha
	writeJSON(w, http.StatusCreated, map[string]any{
		"commit": map[string]any{"sha": sha, "parents": []string{parent}, "message": body.Message},
		"branch": map[string]any{"name": body.Branch, "kind": "branch", "sha": sha}, "change": nil, "change_error": nil,
	})
	if scenarioOf(body.Branch) == "event_latency" {
		s.eventCommits++
		if s.eventCommits == s.opts.withholdEvent {
			return
		}
	}
	time.AfterFunc(s.opts.eventDelay, func() { s.publish(slug, sha) })
}

// publish sends the event of the commit sha on every open stream.
func (s *stub) publish(slug, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSeq++
	data, _ := json.Marshal(map[string]any{"seq": s.nextSeq, "type": "parley.scm.commit", "workspace": slug,
		"resource_type": "commit", "resource_id": sha, "metadata": map[string]any{"sha": sha}})
	frame := fmt.Sprintf("id: %d\nevent: hub_event\ndata: %s\n\n", s.nextSeq, data)
	for ch := range s.streams {
		select {
		case ch <- frame:
		default:
		}
	}
}

func (s *stub) events(w http.ResponseWriter, r *http.Request) {
	ch := make(chan string, 1024)
	s.mu.Lock()
	s.streams[ch] = true
	s.streamsOpened++
	s.streamsMax = max(s.streamsMax, len(s.streams))
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams, ch)
		s.mu.Unlock()
	}()
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "id: 0\nevent: heartbeat\ndata: {\"seq\":0}\n\n")
	_ = rc.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case frame := <-ch:
			fmt.Fprint(w, frame)
			_ = rc.Flush()
		}
	}
}

func (s *stub) openChange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source struct {
			Branch string `json:"branch"`
		} `json:"source"`
		TargetBranch string `json:"target_branch"`
		Title        string `json:"title"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.Source.Branch == "" || body.TargetBranch == "" || body.Title == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "source.branch, target_branch and title are required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	slug := r.PathValue("slug")
	ws := s.workspace(slug)
	_, source := ws.branches[body.Source.Branch]
	_, target := ws.branches[body.TargetBranch]
	if !source || !target {
		writeError(w, http.StatusNotFound, "ref_not_found", "the source or the target branch does not exist")
		return
	}
	s.nextChange++
	ch := &stubChange{number: s.nextChange, workspace: slug, source: body.Source.Branch, target: body.TargetBranch,
		scenario: scenarioOf(body.Source.Branch), state: "open"}
	s.changes[ch.number] = ch
	writeJSON(w, http.StatusCreated, s.changeObject(ch, time.Now()))
}

// changeObject is ch as the hub shows it at now. The caller holds s.mu.
func (s *stub) changeObject(ch *stubChange, now time.Time) map[string]any {
	return map[string]any{"workspace_slug": ch.workspace, "number": ch.number,
		"ref": fmt.Sprintf("%s#%d", ch.workspace, ch.number), "state": ch.state, "target_branch": ch.target,
		"source": map[string]any{"workspace_slug": ch.workspace, "branch": ch.source}, "version": 1,
		"mergeable": s.mergeable(ch, now)}
}

// mergeable is the mergeability of ch at now: the changes of the
// mergeability scenario are unknown until mergeDelay after its extra
// change landed, the others are clean. The caller holds s.mu.
func (s *stub) mergeable(ch *stubChange, now time.Time) string {
	if ch.scenario == "mergeability" && ch.state == "open" &&
		(s.mergeLandedAt.IsZero() || now.Before(s.mergeLandedAt.Add(s.opts.mergeDelay))) {
		return "unknown"
	}
	return "clean"
}

// listChanges lists the open changes, narrowed by target_branch and
// mergeable, on one page.
func (s *stub) listChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if q.Get("mergeable") == "unknown" {
		s.mergePolls = append(s.mergePolls, now)
	}
	items := []map[string]any{}
	for _, n := range slices.Sorted(maps.Keys(s.changes)) {
		ch := s.changes[n]
		if ch.workspace != r.PathValue("slug") || ch.state != "open" ||
			(q.Has("target_branch") && ch.target != q.Get("target_branch")) ||
			(q.Has("mergeable") && s.mergeable(ch, now) != q.Get("mergeable")) {
			continue
		}
		items = append(items, s.changeObject(ch, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nil})
}

// abandon abandons an open change.
func (s *stub) abandon(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.ParseInt(r.PathValue("number"), 10, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.changes[number]
	switch {
	case !ok || ch.workspace != r.PathValue("slug"):
		writeError(w, http.StatusNotFound, "change_not_found", "no such change")
	case ch.state != "open":
		writeError(w, http.StatusConflict, "change_closed", "the change is "+ch.state)
	default:
		ch.state = "abandoned"
		writeJSON(w, http.StatusOK, s.changeObject(ch, time.Now()))
	}
}

// land answers after landDelay with the attempt, landed unless landFails
// says otherwise. The attempts of land_concurrency are also held until all
// sixteen are in flight, or two seconds have passed, so that whether they
// were sent at once does not hang on the scheduler.
func (s *stub) land(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.ParseInt(r.PathValue("number"), 10, 64)
	s.mu.Lock()
	ch, ok := s.changes[number]
	if !ok || ch.workspace != r.PathValue("slug") {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "change_not_found", "no such change")
		return
	}
	st := s.lands[ch.scenario]
	if st == nil {
		st = &landStats{first: time.Now()}
		s.lands[ch.scenario] = st
	}
	st.inFlight++
	st.max = max(st.max, st.inFlight)
	st.count++
	n := st.count
	s.mu.Unlock()
	if ch.scenario == "land_concurrency" {
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			s.mu.Lock()
			all := st.max >= 16
			s.mu.Unlock()
			if all {
				break
			}
		}
	}
	time.Sleep(s.opts.landDelay)
	s.mu.Lock()
	defer s.mu.Unlock()
	st.inFlight--
	attempt := map[string]any{"id": fmt.Sprintf("00000000-0000-4000-8000-%012d", n), "workspace_slug": ch.workspace,
		"target_branch": ch.target, "changes": []int64{ch.number}, "status": "landed"}
	if s.opts.landFails != nil && s.opts.landFails(ch.scenario, n) {
		attempt["status"], attempt["error"] = "failed", "internal"
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"code": 503,
			"error_type": "land_failed", "message": "land attempt failed: internal", "details": attempt}})
		st.last = time.Now()
		return
	}
	ch.state = "landed"
	s.workspace(ch.workspace).branches[ch.target] = s.newSHA()
	if ch.scenario == "mergeability" {
		s.mergeLandedAt = time.Now()
	}
	writeJSON(w, http.StatusOK, attempt)
	st.last = time.Now()
}

func (s *stub) createSandbox(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Profile string `json:"profile"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profiles = append(s.profiles, body.Profile)
	if s.opts.sandboxLimit > 0 && len(s.sandboxes) >= s.opts.sandboxLimit {
		writeError(w, http.StatusTooManyRequests, "sandbox_capacity", "sandbox limit reached: max_per_user")
		return
	}
	s.nextSandbox++
	sb := &stubSandbox{id: fmt.Sprintf("sbx-%d", s.nextSandbox), workspace: r.PathValue("slug"), profile: body.Profile,
		created: time.Now(), failed: s.nextSandbox == s.opts.failSandbox}
	s.sandboxes[sb.id] = sb
	writeJSON(w, http.StatusCreated, sandboxObject(sb, "provisioning"))
}

// sandboxObject is sb as the hub shows it in state.
func sandboxObject(sb *stubSandbox, state string) map[string]any {
	var reason any
	if state == "failed" {
		reason = "image_pull"
	}
	return map[string]any{"id": sb.id, "workspace": sb.workspace, "profile": sb.profile, "state": state, "state_reason": reason}
}

// state is the state sb shows at now: failed when it fails, provisioning
// until the stub's provisioning time has passed, then ready.
func (s *stub) state(sb *stubSandbox, now time.Time) string {
	switch {
	case sb.failed:
		return "failed"
	case now.Before(sb.created.Add(s.opts.provisioning)):
		return "provisioning"
	}
	return "ready"
}

// sandbox is the sandbox of the request, or nil after answering 404. The
// caller holds s.mu.
func (s *stub) sandbox(w http.ResponseWriter, r *http.Request) *stubSandbox {
	sb, ok := s.sandboxes[r.PathValue("id")]
	if !ok || sb.workspace != r.PathValue("slug") {
		writeError(w, http.StatusNotFound, "sandbox_not_found", "no such sandbox")
		return nil
	}
	return sb
}

func (s *stub) getSandbox(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sb := s.sandbox(w, r); sb != nil {
		writeJSON(w, http.StatusOK, sandboxObject(sb, s.state(sb, time.Now())))
	}
}

func (s *stub) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sb := s.sandbox(w, r); sb != nil {
		delete(s.sandboxes, sb.id)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *stub) exec(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command []string `json:"command"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Command) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "command is required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sandbox(w, r) != nil {
		writeJSON(w, http.StatusOK, map[string]any{"exit_code": 0, "stdout": "", "stderr": "", "truncated": false,
			"timed_out": false, "duration_ms": 1})
	}
}

// scenarioOf is the scenario a source branch the harness names belongs
// to: its second segment, as in perf-<run id>/<scenario>/<n>.
func scenarioOf(branch string) string {
	parts := strings.Split(branch, "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
}

// requests returns "METHOD path" of every request the stub was sent.
func (s *stub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// count is how many requests were "METHOD path".
func (s *stub) count(request string) int {
	n := 0
	for _, r := range s.requests() {
		if r == request {
			n++
		}
	}
	return n
}

// unauthorizedRequests are the requests that did not carry the token.
func (s *stub) unauthorizedRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.unauthorized...)
}

// commitsOf are the commit requests of the scenario.
func (s *stub) commitsOf(scenario string) []stubCommit {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []stubCommit
	for _, c := range s.commits {
		if scenarioOf(c.branch) == scenario {
			out = append(out, c)
		}
	}
	return out
}

// allCommits are every commit request.
func (s *stub) allCommits() []stubCommit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubCommit(nil), s.commits...)
}

// changeTargets are the target branches of every change opened.
func (s *stub) changeTargets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, n := range slices.Sorted(maps.Keys(s.changes)) {
		out = append(out, s.changes[n].target)
	}
	return out
}

// createdRefs are the names of the branches the harness asked to create.
func (s *stub) createdRefs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.refs...)
}

// landsOf are the land requests of the scenario.
func (s *stub) landsOf(scenario string) landStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.lands[scenario]; st != nil {
		return *st
	}
	return landStats{}
}

// mergeabilityPolls are when each list of unknown changes came.
func (s *stub) mergeabilityPolls() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.mergePolls...)
}

// liveForks, liveSandboxes and openStreams count what exists now;
// createdForks, createdSandboxes and openedStreams what ever existed.
func (s *stub) liveForks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.forks)
}

func (s *stub) createdForks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextFork
}

func (s *stub) liveSandboxes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sandboxes)
}

func (s *stub) createdSandboxes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextSandbox
}

func (s *stub) openStreams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

func (s *stub) openedStreams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamsOpened
}

// mostStreams is the most event streams that were open at once.
func (s *stub) mostStreams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamsMax
}

// changesOf counts the changes the scenario opened, and those of them
// that are still open.
func (s *stub) changesOf(scenario string) (opened, open int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.changes {
		if ch.scenario == scenario {
			opened++
			if ch.state == "open" {
				open++
			}
		}
	}
	return opened, open
}

// sandboxProfiles are the profiles the sandbox creations named.
func (s *stub) sandboxProfiles() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.profiles...)
}

// config is a configuration for a run against s, with the report in a
// directory of the test's own.
func (s *stub) config(t *testing.T) perf.Config {
	return perf.Config{URL: s.URL, Token: stubToken, Workspaces: []string{"w"},
		ReportPath: filepath.Join(t.TempDir(), "report.json")}
}

// failWith answers every request with status and the error envelope.
func failWith(status int, errorType, message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { writeError(w, status, errorType, message) }
}

// hang never answers: it holds the request until the client gives up.
func hang(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

func writeError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": status, "message": message, "error_type": errorType}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// result is the result of the scenario name in rep.
func result(t *testing.T, rep perf.Report, name string) perf.Result {
	t.Helper()
	for _, r := range rep.Results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("the report has no result %q: %+v", name, rep.Results)
	return perf.Result{}
}
