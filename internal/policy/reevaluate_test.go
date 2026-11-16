package policy_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/policy"
)

// The documents the landing of a change touching billing/a.go follows:
// approval gives it one requirement of one approval, blocking blocks it,
// and checked asks a check as well.
const (
	approval = `{"rules": [{"name": "billing", "paths": ["billing/**"], "require": {"approvals": 1}}]}`
	blocking = `{"rules": [{"name": "billing", "paths": ["billing/**"], "require": {"block": true}}]}`
	checked  = `{"rules": [{"name": "billing", "paths": ["billing/**"], "require": {"approvals": 1, "checks": ["ci"]}}]}`
)

// openBilling opens a change of api from a new branch of base that adds
// billing/a.go, and returns its number.
func (w *world) openBilling(t *testing.T, branch string) int64 {
	t.Helper()
	return w.openTouching(t, branch, "billing/a.go")
}

// openTouching opens a change of api from a new branch of base that adds
// the file path, with adjust run on the input, and returns its number. A
// change whose source is a task fork of api has the branch made in the
// fork's repository.
func (w *world) openTouching(t *testing.T, branch, path string, adjust ...func(*change.OpenInput)) int64 {
	t.Helper()
	in := change.OpenInput{SourceWorkspace: "api"}
	for _, fn := range adjust {
		fn(&in)
	}
	commit(t, w.workspaces.RepoPath(in.SourceWorkspace), branch, w.base, map[string]string{path: branch + "\n"})
	return w.open(t, "api", branch, adjust...).Number
}

// landing is the landing GET answers for the change number of api.
func (w *world) landing(t *testing.T, number int64) map[string]any {
	t.Helper()
	r := w.get(t, "/api/v1/workspaces/api/changes/"+strconv.FormatInt(number, 10), w.danaKey)
	l, ok := r.json["landing"].(map[string]any)
	if r.status != http.StatusOK || !ok {
		t.Fatalf("GET %s = %d %s, want the change with its landing", change.Ref("api", number), r.status, r.body)
	}
	return l
}

// only is the one requirement of the landing l, nil when it has none or
// more than one.
func only(l map[string]any) map[string]any {
	reqs, _ := l["requirements"].([]any)
	if len(reqs) != 1 {
		return nil
	}
	req, _ := reqs[0].(map[string]any)
	return req
}

// putPolicy replaces api's policy with doc through the route, as dana,
// and fails the test unless it answers 200.
func (w *world) putPolicy(t *testing.T, doc string) {
	t.Helper()
	if r := w.put(t, "/api/v1/workspaces/api/policy", w.danaKey, doc, ""); r.status != http.StatusOK {
		t.Fatalf("PUT the policy = %d %s, want 200", r.status, r.body)
	}
}

func TestPutPolicy_ReevaluatesEveryDraftAndOpenChangeAndLeavesClosedOnes(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	fromFork := func(in *change.OpenInput) { in.SourceWorkspace = "api--f1" }
	asDraft := func(in *change.OpenInput) { in.Draft = true }
	pendingChecks := []any{map[string]any{"name": "test", "status": "pending"}, map[string]any{"name": "billing-e2e", "status": "pending"}}
	billing := map[string]any{"block": false, "checks": pendingChecks,
		"approvals": map[string]any{"required": 1.0, "counted": 0.0, "approvers": []any{"owner", "user:dana"}}}
	rows := []struct {
		name, branch, path string
		adjust             []func(*change.OpenInput)
		// closes, when set, lands or abandons the change before the
		// update, which must leave its landing as it is.
		closes func(ctx context.Context, tx *sql.Tx, n int64) error
		// before is the status and reason of the landing under the
		// never-set policy, whose one requirement is the default.
		before [2]any
		// status, reason and rule are those of the landing after the
		// update, whose one requirement has the members of members.
		status  string
		reason  any
		rule    string
		members map[string]any
		n       int64
	}{
		{name: "an open change on docs", branch: "docs", path: "docs/a.md", before: [2]any{change.LandingReady, nil},
			status: change.LandingReady, rule: "docs", members: map[string]any{"block": false, "checks": []any{}}},
		{name: "an open change on billing", branch: "billing", path: "billing/a.go", before: [2]any{change.LandingReady, nil},
			status: change.LandingPending, rule: "billing", members: billing},
		{name: "an open change on secrets", branch: "secrets", path: "deploy/secrets/k", before: [2]any{change.LandingReady, nil},
			status: change.LandingBlocked, rule: "secrets", members: map[string]any{"block": true}},
		{name: "a draft on billing", branch: "draft", path: "billing/b.go", adjust: []func(*change.OpenInput){asDraft},
			before: [2]any{change.LandingBlocked, "draft"}, status: change.LandingBlocked, reason: "draft", rule: "billing", members: billing},
		{name: "an open change on billing from a task fork", branch: "forked", path: "billing/c.go", adjust: []func(*change.OpenInput){fromFork},
			before: [2]any{change.LandingReady, nil}, status: change.LandingPending, rule: "billing", members: billing},
		{name: "an abandoned change on billing", branch: "gone", path: "billing/a.go", closes: func(ctx context.Context, tx *sql.Tx, n int64) error {
			return w.changes.Abandon(ctx, tx, "api", n, "undone", w.danaCaller())
		}},
		{name: "a landed change on billing", branch: "done", path: "billing/a.go", closes: func(ctx context.Context, tx *sql.Tx, n int64) error {
			return w.changes.SetLanded(ctx, tx, "api", n, change.Landed{SHA: w.base, At: time.Now(),
				By: change.Actor{ActorType: change.CredentialAPIKey, ActorID: new(w.dana), Username: new("dana")}, LandID: "l1"})
		}},
	}
	closed := map[int64]map[string]any{}
	for i := range rows {
		r := &rows[i]
		r.n = w.openTouching(t, r.branch, r.path, r.adjust...)
		w.checked(t, "api", r.n)
		if r.closes != nil {
			w.inTx(t, func(ctx context.Context, tx *sql.Tx) error { return r.closes(ctx, tx, r.n) })
			closed[r.n] = w.landing(t, r.n)
			continue
		}
		if l := w.landing(t, r.n); l["status"] != r.before[0] || l["reason"] != r.before[1] || only(l)["rule"] != "default" {
			t.Fatalf("%s: under the never-set policy the landing is %v, want %v on the default requirement", r.name, l, r.before)
		}
	}
	w.putPolicy(t, threeRules)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			l := w.landing(t, r.n)
			if r.closes != nil {
				if !reflect.DeepEqual(l, closed[r.n]) {
					t.Errorf("the landing is %v, was %v before the update", l, closed[r.n])
				}
				return
			}
			req := only(l)
			if l["status"] != r.status || l["reason"] != r.reason || req["rule"] != r.rule {
				t.Fatalf("the landing is %v, want %s (reason %v) on the one requirement %s", l, r.status, r.reason, r.rule)
			}
			for name, want := range r.members {
				if !reflect.DeepEqual(req[name], want) {
					t.Errorf("%s of the requirement = %v, want %v", name, req[name], want)
				}
			}
		})
	}
}

func TestPutPolicy_AnnouncesEveryChangeItMakesLandableOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	docs, billing, secrets := w.openTouching(t, "docs", "docs/a.md"), w.openBilling(t, "billing"), w.openTouching(t, "secrets", "deploy/secrets/k")
	for _, n := range []int64{docs, billing, secrets} {
		if ch := w.checked(t, "api", n); ch.Mergeable != change.MergeableClean {
			t.Fatalf("%s is %s, want clean", ch.Ref, ch.Mergeable)
		}
	}
	w.putPolicy(t, threeRules)
	for _, c := range []struct {
		name, doc string
		// ready are the changes the update announces.
		ready []int64
	}{
		{"pending and blocked to ready, ready kept", `{}`, []int64{billing, secrets}},
		{"ready to pending and blocked, ready kept", threeRules, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			seq := w.seq(t)
			w.putPolicy(t, c.doc)
			var got []int64
			for _, e := range w.eventsAfter(t, seq, change.EventReady) {
				n, _ := strconv.ParseInt(e.ResourceID, 10, 64)
				got = append(got, n)
			}
			if !reflect.DeepEqual(got, c.ready) {
				t.Errorf("parley.change.ready was written for %v, want %v", got, c.ready)
			}
		})
	}
	if l := w.landing(t, billing); l["status"] != change.LandingPending {
		t.Errorf("the billing change is %v, want pending again", l["status"])
	}
}

func TestPutPolicy_CountsTheEvidenceOfAChangeUnderTheNewRules(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	n := w.openBilling(t, "billing")
	ch := w.checked(t, "api", n)
	ev := &evidence{number: n, version: ch.Version, approvals: 1, pass: true}
	w.policy.RegisterEvidence(ev)
	if l := w.landing(t, n); l["status"] != change.LandingReady {
		t.Fatalf("under the never-set policy the change is %v, want ready", l["status"])
	}
	w.putPolicy(t, `{"rules": [{"name": "billing", "paths": ["billing/**"], "require": {"approvals": 1, "checks": ["test"]}}]}`)
	l := w.landing(t, n)
	req := only(l)
	approvals, _ := req["approvals"].(map[string]any)
	if l["status"] != change.LandingReady || req["rule"] != "billing" || approvals["counted"] != 1.0 ||
		!reflect.DeepEqual(req["checks"], []any{map[string]any{"name": "test", "status": "passed"}}) {
		t.Errorf("the landing is %v, want ready on billing with one counted approval and test passed", l)
	}
	// The evidence source is only ever read: its one method is a query.
	ev.mu.Lock()
	defer ev.mu.Unlock()
	if len(ev.asked) == 0 {
		t.Error("the update did not read the evidence of the change")
	}
	for _, q := range ev.asked {
		if q.Workspace != "api" || q.Number != n || q.Version != ch.Version {
			t.Errorf("the evidence was asked for %s at version %d, want %s at %d", change.Ref(q.Workspace, q.Number), q.Version, ch.Ref, ch.Version)
		}
	}
}

func TestPutPolicy_FailsAsAWholeWhenAChangeCannotBeReevaluated(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	docs, secrets, billing := w.openTouching(t, "docs", "docs/a.md"), w.openTouching(t, "secrets", "deploy/secrets/k"), w.openBilling(t, "billing")
	for _, n := range []int64{docs, secrets, billing} {
		w.checked(t, "api", n)
	}
	// The docs change, clean and pending, would be made landable by the
	// update, and announced, before the third change fails it.
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		if err := w.changes.SetLanding(ctx, tx, "api", docs, change.Landing{Status: change.LandingPending}); err != nil {
			return err
		}
		return w.changes.AnnounceLandable(ctx, tx, "api", docs)
	})
	before := map[int64]map[string]any{}
	for _, n := range []int64{docs, secrets, billing} {
		before[n] = w.landing(t, n)
	}
	w.policy.RegisterEvidence(&evidence{fail: billing})
	seq := w.seq(t)
	r := w.put(t, "/api/v1/workspaces/api/policy", w.danaKey, threeRules, "")
	if r.status != http.StatusInternalServerError || r.errorType() != "internal" {
		t.Fatalf("PUT the policy = %d %s, want 500 internal", r.status, r.body)
	}
	if p := w.get(t, "/api/v1/workspaces/api/policy", w.danaKey); p.json["revision"] != 0.0 {
		t.Errorf("the policy is at revision %v, want 0", p.json["revision"])
	}
	for n, l := range before {
		if got := w.landing(t, n); !reflect.DeepEqual(got, l) {
			t.Errorf("the landing of %s is %v, was %v", change.Ref("api", n), got, l)
		}
	}
	for _, typ := range []string{"parley.policy.update", change.EventReady} {
		if events := w.eventsAfter(t, seq, typ); len(events) != 0 {
			t.Errorf("%d %s events were written", len(events), typ)
		}
	}
}

func TestReevaluate_ReadsTheRevisionInsideItsTransaction(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	n := w.openBilling(t, "billing")
	w.checked(t, "api", n)
	var (
		res  policy.Result
		rev  int64
		done = make(chan error, 1)
	)
	// The re-evaluation asks for its transaction while this one holds the
	// store with revision 1 stored and not yet committed.
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		doc, err := validate(t, threeRules)
		if err != nil {
			return err
		}
		if _, err := policy.Replace(ctx, tx, "api", *doc, nil, w.byDana(), time.Now()); err != nil {
			return err
		}
		go func() {
			var err error
			res, rev, err = w.policy.Reevaluate(ctx, "api", n)
			done <- err
		}()
		return nil
	})
	if err := <-done; err != nil || rev != 1 || res.Status != change.LandingPending {
		t.Fatalf("Reevaluate = %s by revision %d, %v; want pending by revision 1", res.Status, rev, err)
	}
	if l := w.landing(t, n); l["status"] != change.LandingPending || only(l)["rule"] != "billing" {
		t.Errorf("the landing is %v, want pending on billing, revision 1's", l)
	}
}

func TestReevaluate_StoresTheLandingOfTheCurrentVersionAndReturnsItWithTheRevision(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.setPolicy(t, "api", threeRules, w.byDana(), time.Now())
	n := w.openBilling(t, "b")
	if l := w.readChange(t, "api", n).Landing; l.Status != change.LandingPending || len(l.Requirements) != 1 {
		t.Errorf("the open stored the landing %+v, want the policy's, pending on billing", l)
	}
	for _, c := range []struct {
		name string
		// files, when set, are the files of a new version of the change
		// made from base.
		files        map[string]string
		status, rule string
	}{
		{"the version that touches billing", nil, change.LandingPending, "billing"},
		{"a version that touches docs only", map[string]string{"docs/x.md": "x\n"}, change.LandingReady, "docs"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.files != nil {
				head := commit(t, w.api, "b", w.base, c.files)
				w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
					_, err := w.changes.RecordVersion(ctx, tx, change.VersionInput{Workspace: "api", Branch: "b", Head: head,
						Kind: change.KindPush, Caller: w.danaCaller()})
					return err
				})
			}
			res, rev, err := w.policy.Reevaluate(ctx, "api", n)
			if err != nil || rev != 1 || res.Status != c.status || len(res.Requirements) == 0 || res.Requirements[0].Rule != c.rule {
				t.Fatalf("Reevaluate = %+v, %d, %v; want %s on %s by revision 1", res, rev, err, c.status, c.rule)
			}
			r := w.get(t, "/api/v1/workspaces/api/changes/"+strconv.FormatInt(n, 10), w.danaKey)
			if want := asJSON(t, res); r.status != http.StatusOK || !reflect.DeepEqual(r.json["landing"], want) {
				t.Errorf("GET the change = %d with landing %v, want %v", r.status, r.json["landing"], want)
			}
		})
	}
}

func TestReevaluate_AnnouncesAChangeOnceWhenItBecomesLandable(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ev := &evidence{}
	w.policy.RegisterEvidence(ev)
	ctx := context.Background()
	// Every row runs on the one change, which arrange sets as the row needs
	// it.
	n := w.openBilling(t, "b")
	w.checked(t, "api", n)
	// to is a document and the approvals the evidence counts, which give
	// the status a re-evaluation finds.
	type to struct {
		doc       string
		approvals int
		status    string
	}
	ready, pending, blocked := to{approval, 1, change.LandingReady}, to{approval, 0, change.LandingPending}, to{blocking, 1, change.LandingBlocked}
	pendingOther := to{checked, 1, change.LandingPending}
	reevaluate := func(t *testing.T, n int64, now to) {
		t.Helper()
		w.setPolicy(t, "api", now.doc, w.byDana(), time.Now())
		ev.count(now.approvals)
		if res, _, err := w.policy.Reevaluate(ctx, "api", n); err != nil || res.Status != now.status {
			t.Fatalf("Reevaluate = %s, %v; want %s", res.Status, err, now.status)
		}
	}
	// arrange gives the change n the landing status before while it is
	// clean, announced as the hub announces it, and then the mergeable.
	arrange := func(t *testing.T, n int64, before, mergeable string) {
		t.Helper()
		w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
			if err := w.changes.SetMergeable(ctx, tx, "api", n, change.MergeableClean, nil); err != nil {
				return err
			}
			if err := w.changes.SetLanding(ctx, tx, "api", n, change.Landing{Status: before}); err != nil {
				return err
			}
			if err := w.changes.AnnounceLandable(ctx, tx, "api", n); err != nil {
				return err
			}
			return w.changes.SetMergeable(ctx, tx, "api", n, mergeable, nil)
		})
	}

	for _, c := range []struct {
		name              string
		before, mergeable string
		now               to
		want              int
	}{
		{"pending to ready, clean", change.LandingPending, change.MergeableClean, ready, 1},
		{"blocked to ready, clean", change.LandingBlocked, change.MergeableClean, ready, 1},
		{"still ready, clean", change.LandingReady, change.MergeableClean, ready, 0},
		{"pending to ready, mergeable unknown", change.LandingPending, change.MergeableUnknown, ready, 0},
		{"pending to ready, conflicting", change.LandingPending, change.MergeableConflict, ready, 0},
		{"ready to pending, clean", change.LandingReady, change.MergeableClean, pending, 0},
		{"pending to blocked, clean", change.LandingPending, change.MergeableClean, blocked, 0},
		{"pending with other requirements, clean", change.LandingPending, change.MergeableClean, pendingOther, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			arrange(t, n, c.before, c.mergeable)
			seq := w.seq(t)
			reevaluate(t, n, c.now)
			events := w.readyEvents(t, seq, n)
			if len(events) != c.want {
				t.Fatalf("%d parley.change.ready events, want %d", len(events), c.want)
			}
			for _, e := range events {
				var meta map[string]any
				if err := json.Unmarshal(e.Metadata, &meta); err != nil {
					t.Fatal(err)
				}
				if head := w.readChange(t, "api", n).HeadSHA; e.ResourceType != "change" || e.ActorType != "system" || e.ActorID != nil ||
					meta["head_sha"] != head || meta["number"] != float64(n) || meta["version"] != 1.0 || meta["ref"] != change.Ref("api", n) {
					t.Errorf("event on %s by %s %v with %v, want on the change by the system with its number, ref, version 1 and head %s",
						e.ResourceType, e.ActorType, e.ActorID, meta, head)
				}
			}
		})
	}

	// becomeLandable makes the change n landable through the mergeability
	// result the check stores, its landing being ready already.
	becomeLandable := func(t *testing.T, n int64) {
		t.Helper()
		ch := w.readChange(t, "api", n)
		w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
			return w.changes.StoreMergeability(ctx, tx, "api", n, ch.HeadSHA, w.base, nil)
		})
	}
	for _, c := range []struct {
		name      string
		mergeable string
		steps     []func(t *testing.T, n int64)
		want      int
	}{
		{"pending, ready, ready", change.MergeableClean, []func(*testing.T, int64){
			func(t *testing.T, n int64) { reevaluate(t, n, ready) },
			func(t *testing.T, n int64) { reevaluate(t, n, ready) },
		}, 1},
		{"pending, ready, pending, ready", change.MergeableClean, []func(*testing.T, int64){
			func(t *testing.T, n int64) { reevaluate(t, n, ready) },
			func(t *testing.T, n int64) { reevaluate(t, n, pending) },
			func(t *testing.T, n int64) { reevaluate(t, n, ready) },
		}, 2},
		{"ready while unknown, then clean by the check, then ready", change.MergeableUnknown, []func(*testing.T, int64){
			func(t *testing.T, n int64) { reevaluate(t, n, ready) },
			becomeLandable,
			func(t *testing.T, n int64) { reevaluate(t, n, ready) },
		}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			arrange(t, n, change.LandingPending, c.mergeable)
			seq := w.seq(t)
			for _, step := range c.steps {
				step(t, n)
			}
			if events := w.readyEvents(t, seq, n); len(events) != c.want {
				t.Errorf("%d parley.change.ready events in all, want %d", len(events), c.want)
			}
		})
	}
}

func TestReevaluate_RefusesAClosedOrUnknownChangeAndWritesNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	abandoned, landed := w.openBilling(t, "gone"), w.openBilling(t, "done")
	w.checked(t, "api", abandoned)
	w.checked(t, "api", landed)
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		if err := w.changes.Abandon(ctx, tx, "api", abandoned, "undone", w.danaCaller()); err != nil {
			return err
		}
		return w.changes.SetLanded(ctx, tx, "api", landed, change.Landed{SHA: w.base, At: time.Now(),
			By: change.Actor{ActorType: change.CredentialAPIKey, ActorID: new(w.dana), Username: new("dana")}, LandID: "l1"})
	})
	for _, c := range []struct {
		name   string
		slug   string
		number int64
		want   error
	}{
		{"an abandoned change", "api", abandoned, policy.ErrChangeClosed},
		{"a landed change", "api", landed, policy.ErrChangeClosed},
		{"an unknown number", "api", 9999, policy.ErrChangeNotFound},
		{"an unknown workspace", "nope", 1, policy.ErrChangeNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			var before change.Landing
			if c.want == policy.ErrChangeClosed {
				before = w.readChange(t, c.slug, c.number).Landing
			}
			seq := w.seq(t)
			res, rev, err := w.policy.Reevaluate(ctx, c.slug, c.number)
			if !errors.Is(err, c.want) || res.Status != "" || rev != 0 {
				t.Errorf("Reevaluate = %+v, %d, %v; want no result and %v", res, rev, err, c.want)
			}
			if c.want == policy.ErrChangeClosed {
				if after := w.readChange(t, c.slug, c.number).Landing; !reflect.DeepEqual(asJSON(t, after), asJSON(t, before)) {
					t.Errorf("the landing is %+v, was %+v", after, before)
				}
			}
			if events := w.eventsAfter(t, seq, ""); len(events) != 0 {
				t.Errorf("%d events were written, the first %s", len(events), events[0].Type)
			}
		})
	}
}

func TestReevaluate_StoresNothingWhenTheReadyEventCannotBeWritten(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ev := &evidence{}
	w.policy.RegisterEvidence(ev)
	ctx := context.Background()
	w.setPolicy(t, "api", approval, w.byDana(), time.Now())
	n := w.openBilling(t, "b")
	if ch := w.checked(t, "api", n); ch.Mergeable != change.MergeableClean || ch.Landing.Status != change.LandingPending {
		t.Fatalf("the change is %s and %s, want clean and pending", ch.Mergeable, ch.Landing.Status)
	}
	seq := w.seq(t)
	ev.count(1)
	w.readyGate.fail.Store(true)
	if _, _, err := w.policy.Reevaluate(ctx, "api", n); err == nil {
		t.Fatal("Reevaluate succeeded with a log that cannot take the ready event")
	}
	if l := w.readChange(t, "api", n).Landing; l.Status != change.LandingPending || len(w.readyEvents(t, seq, n)) != 0 {
		t.Errorf("after the failure the landing is %s with %d ready events, want pending and none", l.Status, len(w.readyEvents(t, seq, n)))
	}
	w.readyGate.fail.Store(false)
	if res, _, err := w.policy.Reevaluate(ctx, "api", n); err != nil || res.Status != change.LandingReady {
		t.Fatalf("Reevaluate = %s, %v; want ready", res.Status, err)
	}
	if l := w.readChange(t, "api", n).Landing; l.Status != change.LandingReady || len(w.readyEvents(t, seq, n)) != 1 {
		t.Errorf("then the landing is %s with %d ready events, want ready and one", l.Status, len(w.readyEvents(t, seq, n)))
	}
}

func TestReevaluate_GovernsAChangeIntoATaskForkByTheNeverSetDocument(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.setPolicy(t, "api", threeRules, w.byDana(), time.Now())
	commit(t, w.workspaces.RepoPath("api--f1"), "f", w.base, map[string]string{"deploy/secrets/k": "k\n"})
	n := w.open(t, "api--f1", "f").Number
	res, rev, err := w.policy.Reevaluate(context.Background(), "api--f1", n)
	if err != nil || rev != 0 || res.Status != change.LandingReady || len(res.Requirements) != 1 || res.Requirements[0].Rule != "default" {
		t.Errorf("Reevaluate = %+v, %d, %v; want ready on the default requirement by revision 0", res, rev, err)
	}
}

func TestLand_IsJudgedByThePolicyOfTheChangesTarget(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.setPolicy(t, "api", threeRules, w.byDana(), time.Now())
	for _, c := range []struct {
		name   string
		files  map[string]string
		status int
		// blocked is the status the answer names the change blocked by,
		// "" for a change that lands.
		blocked string
	}{
		{"a change the policy keeps pending", map[string]string{"billing/a.go": "a\n"}, http.StatusConflict, change.LandingPending},
		{"a change the policy lets land", map[string]string{"docs/a.md": "a\n"}, http.StatusOK, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			branch := "land" + strconv.Itoa(c.status)
			commit(t, w.api, branch, "main", c.files)
			n := w.open(t, "api", branch).Number
			w.checked(t, "api", n)
			r := w.send(t, http.MethodPost, "/api/v1/workspaces/api/changes/"+strconv.FormatInt(n, 10)+"/land", w.danaKey, map[string]any{}, nil)
			// The attempt is the answer, or the details of an error.
			attempt := r.json
			if e, ok := r.json["error"].(map[string]any); ok {
				attempt, _ = e["details"].(map[string]any)
			}
			blocked, _ := attempt["blocked"].(map[string]any)
			if r.status != c.status || attempt["policy_revision"] != 1.0 || (c.blocked != "" && blocked["status"] != c.blocked) {
				t.Errorf("POST land = %d %s, want %d by revision 1, blocked %q", r.status, r.body, c.status, c.blocked)
			}
		})
	}
}

func TestJudgeVersion_JudgesTheVersionALandLandsAgainstTheChangeReadInItsTransaction(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.setPolicy(t, "api", threeRules, w.byDana(), time.Now())
	// Version 1 touches the secrets the policy blocks, version 2 docs only.
	n := w.openTouching(t, "b", "deploy/secrets/k")
	older := w.checked(t, "api", n)
	head := commit(t, w.api, "b", w.base, map[string]string{"docs/x.md": "x\n"})
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := w.changes.RecordVersion(ctx, tx, change.VersionInput{Workspace: "api", Branch: "b", Head: head,
			Kind: change.KindPush, Caller: w.danaCaller()})
		return err
	})
	current := w.checked(t, "api", n)
	// A stored landing that the judgement of the older version leaves and
	// that of the current version replaces.
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		return w.changes.SetLanding(ctx, tx, "api", n, change.Landing{Status: change.LandingPending})
	})
	for _, c := range []struct {
		name         string
		v            change.Change
		status, rule string
		stored       string
	}{
		{"an older version, by its own commits", older, change.LandingBlocked, "secrets", change.LandingPending},
		{"the current version, stored", current, change.LandingReady, "docs", change.LandingReady},
	} {
		t.Run(c.name, func(t *testing.T) {
			seq := w.seq(t)
			res, rev, err := w.policy.JudgeVersion(ctx, "api", n, c.v.Version, *c.v.BaseSHA, c.v.HeadSHA)
			if err != nil || rev != 1 || res.Status != c.status || len(res.Requirements) != 1 || res.Requirements[0].Rule != c.rule {
				t.Errorf("JudgeVersion of version %d = %+v, %d, %v; want %s on %s by revision 1", c.v.Version, res, rev, err, c.status, c.rule)
			}
			if l := w.readChange(t, "api", n).Landing; l.Status != c.stored {
				t.Errorf("the stored landing is %s, want %s", l.Status, c.stored)
			}
			if c.stored == change.LandingPending && len(w.eventsAfter(t, seq, "")) != 0 {
				t.Error("judging an older version wrote events")
			}
		})
	}
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		return w.changes.Abandon(ctx, tx, "api", n, "undone", w.danaCaller())
	})
	for _, c := range []struct {
		name   string
		number int64
		v      change.Change
		want   error
	}{
		{"a closed change, at an older version", n, older, policy.ErrChangeClosed},
		{"a closed change, at its current version", n, current, policy.ErrChangeClosed},
		{"an unknown change", 9999, older, policy.ErrChangeNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := w.policy.JudgeVersion(ctx, "api", c.number, c.v.Version, *c.v.BaseSHA, c.v.HeadSHA); !errors.Is(err, c.want) {
				t.Errorf("JudgeVersion = %v, want %v", err, c.want)
			}
		})
	}
}

func TestEvaluateLanding_AnswersForAVersionTheHubRecordsAsItStarts(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.setPolicy(t, "api", threeRules, w.byDana(), time.Now())
	n := w.openBilling(t, "b")
	commit(t, w.api, "b", w.base, map[string]string{"docs/x.md": "x\n"})
	w.Restart(t)
	ch := w.readChange(t, "api", n)
	if ch.Version != 2 || ch.Landing.Status != change.LandingReady || len(ch.Landing.Requirements) != 1 ||
		!strings.Contains(string(ch.Landing.Requirements[0]), `"rule":"docs"`) {
		t.Errorf("after the restart the change is at version %d with landing %s %s, want version 2 ready on docs",
			ch.Version, ch.Landing.Status, ch.Landing.Requirements)
	}
}
