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
	commit(t, w.api, branch, w.base, map[string]string{"billing/a.go": branch + "\n"})
	return w.open(t, "api", branch).Number
}

func TestReevaluate_StoresTheLandingOfTheCurrentVersionAndReturnsItWithTheRevision(t *testing.T) {
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
	w := newWorld(t)
	ev := &counted{}
	w.policy.RegisterEvidence(ev)
	ctx := context.Background()
	// to is a document and the approvals the evidence counts, which give
	// the status a re-evaluation finds.
	type to struct {
		doc       string
		approvals int64
		status    string
	}
	ready, pending, blocked := to{approval, 1, change.LandingReady}, to{approval, 0, change.LandingPending}, to{blocking, 1, change.LandingBlocked}
	pendingOther := to{checked, 1, change.LandingPending}
	reevaluate := func(t *testing.T, n int64, now to) {
		t.Helper()
		w.setPolicy(t, "api", now.doc, w.byDana(), time.Now())
		ev.n.Store(now.approvals)
		if res, _, err := w.policy.Reevaluate(ctx, "api", n); err != nil || res.Status != now.status {
			t.Fatalf("Reevaluate = %s, %v; want %s", res.Status, err, now.status)
		}
	}
	// arrange gives the change n, whose check has run, the landing status
	// before while it is clean, announced as the hub announces it, and
	// then the mergeable.
	arrange := func(t *testing.T, n int64, before, mergeable string) {
		t.Helper()
		w.checked(t, "api", n)
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

	for i, c := range []struct {
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
			n := w.openBilling(t, "row"+strconv.Itoa(i))
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
	for i, c := range []struct {
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
			n := w.openBilling(t, "seq"+strconv.Itoa(i))
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
	w := newWorld(t)
	ev := &counted{}
	w.policy.RegisterEvidence(ev)
	ctx := context.Background()
	w.setPolicy(t, "api", approval, w.byDana(), time.Now())
	n := w.openBilling(t, "b")
	if ch := w.checked(t, "api", n); ch.Mergeable != change.MergeableClean || ch.Landing.Status != change.LandingPending {
		t.Fatalf("the change is %s and %s, want clean and pending", ch.Mergeable, ch.Landing.Status)
	}
	seq := w.seq(t)
	ev.n.Store(1)
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

func TestEvaluateVersion_JudgesTheCommitsOfAnOlderVersionAndStoresNothing(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.setPolicy(t, "api", threeRules, w.byDana(), time.Now())
	first := w.readChange(t, "api", w.openBilling(t, "b"))
	head := commit(t, w.api, "b", w.base, map[string]string{"docs/x.md": "x\n"})
	w.inTx(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := w.changes.RecordVersion(ctx, tx, change.VersionInput{Workspace: "api", Branch: "b", Head: head,
			Kind: change.KindPush, Caller: w.danaCaller()})
		return err
	})
	ch := w.readChange(t, "api", first.Number)
	res, rev, err := w.policy.EvaluateVersion(ctx, ch, first.Version, *first.BaseSHA, first.HeadSHA)
	if err != nil || rev != 1 || res.Status != change.LandingPending || len(res.Requirements) != 1 || res.Requirements[0].Rule != "billing" {
		t.Errorf("EvaluateVersion of version 1 = %+v, %d, %v; want pending on billing by revision 1", res, rev, err)
	}
	if l := w.readChange(t, "api", ch.Number).Landing; l.Status != change.LandingReady {
		t.Errorf("the stored landing is %s, want the current version's, ready", l.Status)
	}
}

func TestEvaluateLanding_AnswersForAVersionTheHubRecordsAsItStarts(t *testing.T) {
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
