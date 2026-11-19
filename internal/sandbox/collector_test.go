package sandbox_test

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

func TestPolicyCollector_ReportsTheFirstDenialOfADestinationAsOneEvent(t *testing.T) {
	h := startCollectorHub(t)
	rows := []struct {
		name  string
		entry runtime.LogEntry
		want  map[string]any
	}{
		{
			name:  "a request",
			entry: runtime.LogEntry{Program: "curl", Host: "example.org", Port: 443, Method: "GET", Path: "/a"},
			want:  map[string]any{"program": "curl", "host": "example.org", "port": 443.0, "method": "GET", "path": "/a"},
		},
		{
			name:  "a blocked connection",
			entry: runtime.LogEntry{Program: "git", Host: "10.0.0.5", Port: 22},
			want:  map[string]any{"program": "git", "host": "10.0.0.5", "port": 22.0, "method": nil, "path": nil},
		},
		{
			name:  "a path holding an exposed value",
			entry: runtime.LogEntry{Program: "curl", Host: "example.org", Port: 443, Method: "POST", Path: "/q?key=supersecretvalue"},
			want:  map[string]any{"program": "curl", "host": "example.org", "port": 443.0, "method": "POST", "path": "/q?key=[redacted]"},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			id := h.readySandbox(t, func(r *sandbox.Record) { r.SessionID = new(t1Session) })
			h.svc.SetValues(id, sandbox.Values{Exposed: []string{"supersecretvalue"}})
			before := h.counted()
			row.entry.Action = "deny"
			h.rt.EmitPolicyLog("parley", id, row.entry)
			h.collector(t).Pass(context.Background())
			h.reported(t, id, 1)
			evs := h.denials(t, id)
			if len(evs) != 1 {
				t.Fatalf("denial events = %d; want 1", len(evs))
			}
			e := evs[0]
			if e.actorType != "system" || e.actorID != nil || e.resourceType != "sandbox" {
				t.Errorf("event actor %s %v, resource %s; want system <nil>, sandbox", e.actorType, e.actorID, e.resourceType)
			}
			if e.workspace == nil || *e.workspace != "ws" || e.session == nil || *e.session != t1Session {
				t.Errorf("event workspace %v, session %v; want ws, %s", e.workspace, e.session, t1Session)
			}
			if !reflect.DeepEqual(e.metadata, row.want) {
				t.Errorf("metadata = %v; want %v", e.metadata, row.want)
			}
			if got := h.counted() - before; got != 1 {
				t.Errorf("denials counted = %v; want 1", got)
			}
		})
	}
}

func TestPolicyCollector_CountsARepeatedDestinationWithoutAnotherEvent(t *testing.T) {
	h := startCollectorHub(t)
	s, u := h.sandbox(t, nil), h.sandbox(t, nil)
	h.deny(s, "curl", "example.org", 443, "GET", "/a")
	h.deny(s, "curl", "example.org", 443, "GET", "/b")
	h.deny(s, "curl", "example.org", 443, "POST", "/c")
	h.deny(s, "curl", "example.org", 443, "", "")
	h.deny(s, "curl", "example.org", 80, "GET", "/a")
	h.deny(s, "wget", "example.org", 443, "GET", "/a")
	h.deny(s, "curl", "example.com", 443, "GET", "/a")
	h.deny(u, "curl", "example.org", 443, "GET", "/a")
	h.collector(t).Pass(context.Background())
	// The last entry of each log is a first denial, whose event is written
	// after every earlier entry of that log was counted.
	h.reported(t, s, 4)
	h.reported(t, u, 1)
	var got [][]any
	for _, e := range h.denials(t, s) {
		got = append(got, []any{e.metadata["program"], e.metadata["host"], e.metadata["port"]})
	}
	want := [][]any{{"curl", "example.org", 443.0}, {"curl", "example.org", 80.0}, {"wget", "example.org", 443.0}, {"curl", "example.com", 443.0}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("denial events of s = %v; want %v", got, want)
	}
	if n := len(h.denials(t, u)); n != 1 {
		t.Errorf("denial events of u = %d; want 1", n)
	}
	if got := h.counted(); got != 8 {
		t.Errorf("denials counted = %v; want 8", got)
	}
}

func TestPolicyCollector_FollowsThePolicyLogOfEachReadySandboxOnly(t *testing.T) {
	h := startCollectorHub(t)
	a := h.sandbox(t, nil)
	h.sandbox(t, func(r *sandbox.Record) { r.State = sandbox.StateStopped })
	c := h.sandbox(t, func(r *sandbox.Record) { r.State = sandbox.StateProvisioning })
	col := h.collector(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	col.Pass(ctx)
	eventually(t, "the collector to follow "+a, func() bool { return slices.Equal(h.followed(), []string{a}) })

	bg := context.Background()
	if _, err := h.svc.Transition(bg, nil, c, sandbox.StateProvisioning, sandbox.StateReady, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Transition(bg, nil, a, sandbox.StateReady, sandbox.StateStopped, "", nil); err != nil {
		t.Fatal(err)
	}
	col.Pass(ctx)
	eventually(t, "the collector to follow "+c+" alone", func() bool { return slices.Equal(h.followed(), []string{c}) })
	if got := sandbox.PolicyFollowers(col); !slices.Equal(got, []string{c}) {
		t.Errorf("followers = %v; want [%s]", got, c)
	}

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		col.Run(ctx)
	}()
	cancel()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the collector did not return within 5 s of the end of its context")
	}
	if got := h.followed(); len(got) != 0 {
		t.Errorf("open log streams after the end = %v; want none", got)
	}
}

func TestPolicyCollector_OpensAFailedPolicyLogAgainAfterTheRetryDelay(t *testing.T) {
	h := startCollectorHub(t)
	rows := []struct {
		name string
		// unreachable keeps the runtime unreachable through the first
		// attempt after the delay.
		unreachable bool
	}{
		{name: "a stream that breaks"},
		{name: "a runtime that cannot be reached", unreachable: true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			id := h.readySandbox(t, nil)
			col := h.collector(t)
			waitStopped := func() {
				t.Helper()
				eventually(t, "the follower of "+id+" to stop", func() bool { return len(sandbox.PolicyFollowers(col)) == 0 })
			}
			before := h.counted()
			h.deny(id, "curl", "x.example", 443, "GET", "/x")
			col.Pass(ctx)
			h.reported(t, id, 1)

			h.rt.SetUnreachable(row.unreachable)
			h.breakLog(t, id)
			waitStopped()
			col.Pass(ctx)
			if got := sandbox.PolicyFollowers(col); len(got) != 0 {
				t.Fatalf("followers right after the failure = %v; want none", got)
			}
			h.retryClock.Advance(denialRetry - time.Second)
			col.Pass(ctx)
			if got := sandbox.PolicyFollowers(col); len(got) != 0 {
				t.Fatalf("followers before the retry delay = %v; want none", got)
			}
			h.retryClock.Advance(time.Second)
			if row.unreachable {
				col.Pass(ctx)
				waitStopped()
				h.rt.SetUnreachable(false)
				col.Pass(ctx)
				if got := sandbox.PolicyFollowers(col); len(got) != 0 {
					t.Fatalf("followers right after the second failure = %v; want none", got)
				}
				h.retryClock.Advance(denialRetry)
			}
			col.Pass(ctx)
			eventually(t, "the collector to follow "+id+" again", func() bool { return slices.Equal(h.followed(), []string{id}) })

			h.deny(id, "curl", "y.example", 443, "GET", "/y")
			h.reported(t, id, 2)
			if got := h.counted() - before; got != 2 {
				t.Errorf("denials counted = %v; want 2 (the replayed one once)", got)
			}

			// A second collector on the same store, as after a restart,
			// reads the log from its start and reports what is new alone.
			col.Stop()
			h.collector(t).Pass(ctx)
			h.deny(id, "curl", "z.example", 443, "GET", "/z")
			h.reported(t, id, 3)
			var hosts []any
			for _, e := range h.denials(t, id) {
				hosts = append(hosts, e.metadata["host"])
			}
			if want := []any{"x.example", "y.example", "z.example"}; !reflect.DeepEqual(hosts, want) {
				t.Errorf("denial events = %v; want %v", hosts, want)
			}
		})
	}
}

func TestPolicyCollector_StoresANewProposalAsOpenWithOneEvent(t *testing.T) {
	h := startCollectorHub(t)
	rows := []struct {
		name     string
		findings []runtime.Finding
		want     any
	}{
		{name: "without findings", want: nil},
		{name: "with low, high and medium findings", findings: []runtime.Finding{{Severity: "low", Message: "l"}, {Severity: "high", Message: "h"}, {Severity: "medium", Message: "m"}}, want: "high"},
		{name: "with a medium finding", findings: []runtime.Finding{{Severity: "medium", Message: "m"}}, want: "medium"},
		{name: "with a low finding", findings: []runtime.Finding{{Severity: "low", Message: "l"}}, want: "low"},
		{name: "with a severity of the runtime's own and a low one", findings: []runtime.Finding{{Severity: "security", Message: "s"}, {Severity: "low", Message: "l"}}, want: "low"},
		{name: "with a severity of the runtime's own alone", findings: []runtime.Finding{{Severity: "security", Message: "s"}}, want: "security"},
	}
	idPattern := regexp.MustCompile(`^sbp_[0-9a-f]{32}$`)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			id := h.readySandbox(t, func(r *sandbox.Record) { r.SessionID = new(t1Session); allowProposals(r) })
			proposed := h.propose(id, `{"host": "registry.npmjs.org", "port": 443}`, row.findings...)
			h.collector(t).Pass(context.Background())
			got := h.proposals(t, id, sandbox.ProposalOpen)
			if len(got) != 1 {
				t.Fatalf("open proposals = %+v; want 1", got)
			}
			p := got[0]
			if !idPattern.MatchString(p.ID) || p.SandboxID != id || p.RuntimeID != proposed.ID || p.State != sandbox.ProposalOpen {
				t.Errorf("proposal id %q, sandbox %q, runtime id %q, state %q; want sbp_…, %s, %s, open", p.ID, p.SandboxID, p.RuntimeID, p.State, id, proposed.ID)
			}
			if string(p.Rule) != string(proposed.Rule.Body) || p.Summary != proposed.Summary || !reflect.DeepEqual(p.Findings, row.findings) {
				t.Errorf("rule %s, summary %q, findings %v; want %s, %q, %v", p.Rule, p.Summary, p.Findings, proposed.Rule.Body, proposed.Summary, row.findings)
			}
			if p.Scope != nil || p.WorkspaceRuleID != nil || p.DecidedBy != nil || p.DecidedAt != nil || p.Comment != nil {
				t.Errorf("decision fields %v %v %v %v %v; want all nil", p.Scope, p.WorkspaceRuleID, p.DecidedBy, p.DecidedAt, p.Comment)
			}
			if !p.CreatedAt.Equal(recordStart) {
				t.Errorf("created_at = %v; want %v", p.CreatedAt, recordStart)
			}
			evs := h.sandboxEvents(t, sandbox.EventProposal, id)
			if len(evs) != 1 {
				t.Fatalf("proposal events = %d; want 1", len(evs))
			}
			e := evs[0]
			if e.actorType != "system" || e.actorID != nil || e.resourceType != "sandbox" {
				t.Errorf("event actor %s %v, resource %s; want system <nil>, sandbox", e.actorType, e.actorID, e.resourceType)
			}
			if e.workspace == nil || *e.workspace != "ws" || e.session == nil || *e.session != t1Session {
				t.Errorf("event workspace %v, session %v; want ws, %s", e.workspace, e.session, t1Session)
			}
			want := map[string]any{"proposal_id": p.ID, "summary": proposed.Summary, "max_severity": row.want}
			if !reflect.DeepEqual(e.metadata, want) {
				t.Errorf("metadata = %v; want %v", e.metadata, want)
			}
		})
	}
}

func TestPolicyCollector_StoresAProposalSeenBeforeOnce(t *testing.T) {
	h := startCollectorHub(t)
	id := h.sandbox(t, allowProposals)
	h.propose(id, `{"host": "example.org"}`)
	ctx := context.Background()
	col := h.collector(t)
	col.Pass(ctx)
	h.retryClock.Advance(collectorInterval)
	col.Pass(ctx)
	// A new collector on the same store, as after a restart.
	col.Stop()
	h.collector(t).Pass(ctx)
	if n := h.runtimeCalls(runtime.OpListProposals, ""); n != 3 {
		t.Errorf("proposal listings = %d; want 3", n)
	}
	if got := h.proposals(t, id, sandbox.ProposalOpen); len(got) != 1 {
		t.Errorf("proposals = %+v; want 1", got)
	}
	if n := len(h.sandboxEvents(t, sandbox.EventProposal, id)); n != 1 {
		t.Errorf("proposal events = %d; want 1", n)
	}
}

func TestPolicyCollector_RejectsAProposalOfASandboxWhoseProfileDidNotAllowThem(t *testing.T) {
	h := startCollectorHub(t)
	rows := []struct {
		name    string
		sandbox func(t *testing.T) string
		rejects int
		stored  int
	}{
		{name: "created from a profile that does not allow proposals", rejects: 1, sandbox: func(t *testing.T) string {
			return h.readySandbox(t, func(r *sandbox.Record) { r.Profile = "closed" })
		}},
		{name: "created from such a profile the configuration no longer has", rejects: 1, sandbox: func(t *testing.T) string {
			return h.readySandbox(t, func(r *sandbox.Record) { r.Profile = "gone" })
		}},
		{name: "allowed them at creation by a profile that now does not", stored: 1, sandbox: func(t *testing.T) string {
			return h.readySandbox(t, func(r *sandbox.Record) { r.Profile = "closed"; allowProposals(r) })
		}},
		{name: "a sandbox the hub does not hold", sandbox: h.unheldSandbox},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			id := row.sandbox(t)
			h.propose(id, `{"host": "example.org"}`)
			h.collector(t).Pass(context.Background())
			if n := h.runtimeCalls(runtime.OpRejectProposal, id); n != row.rejects {
				t.Errorf("reject calls = %d; want %d", n, row.rejects)
			}
			if got := h.proposals(t, id, sandbox.ProposalOpen); len(got) != row.stored {
				t.Errorf("proposals = %+v; want %d", got, row.stored)
			}
			if n := len(h.sandboxEvents(t, sandbox.EventProposal, id)); n != row.stored {
				t.Errorf("proposal events = %d; want %d", n, row.stored)
			}
		})
	}
}

func TestPolicyCollector_ListsProposalsAgainAtTheNextPollAfterAFailure(t *testing.T) {
	h := startCollectorHub(t)
	id := h.sandbox(t, allowProposals)
	ctx := context.Background()
	col := h.collector(t)
	h.propose(id, `{"host": "a.example"}`)
	col.Pass(ctx)
	before := h.proposals(t, id, sandbox.ProposalOpen)

	h.propose(id, `{"host": "b.example"}`)
	h.rt.FailOperation(runtime.OpListProposals, runtime.ErrUnavailable, "the gateway is down")
	h.retryClock.Advance(collectorInterval)
	col.Pass(ctx)
	if got := h.proposals(t, id, sandbox.ProposalOpen); !reflect.DeepEqual(got, before) {
		t.Errorf("proposals after a failed listing = %+v; want %+v", got, before)
	}
	h.rt.FailOperation(runtime.OpListProposals, nil, "")
	col.Pass(ctx)
	if n := h.runtimeCalls(runtime.OpListProposals, ""); n != 2 {
		t.Errorf("proposal listings within the poll interval of the failure = %d; want 2", n)
	}
	h.retryClock.Advance(collectorInterval)
	col.Pass(ctx)
	if got := h.proposals(t, id, sandbox.ProposalOpen); len(got) != 2 {
		t.Errorf("proposals after the runtime recovered = %+v; want 2", got)
	}
	if n := len(h.sandboxEvents(t, sandbox.EventProposal, id)); n != 2 {
		t.Errorf("proposal events = %d; want 2", n)
	}
}

func TestPolicyCollector_ListsProposalsFirstOneIntervalAfterItStarts(t *testing.T) {
	h := startCollectorHub(t)
	id := h.sandbox(t, allowProposals)
	h.propose(id, `{"host": "example.org"}`)
	ctx, cancel := context.WithCancel(context.Background())
	col := h.collector(t)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		col.Run(ctx)
	}()
	defer func() {
		cancel()
		<-returned
	}()
	// The follower is started under the pass's lock, which PolicyFollowers
	// takes: once it is there, the first pass is over.
	eventually(t, "the first pass to follow "+id, func() bool { return slices.Equal(sandbox.PolicyFollowers(col), []string{id}) })
	if n := h.runtimeCalls(runtime.OpListProposals, ""); n != 0 {
		t.Fatalf("proposal listings of the first pass = %d; want 0", n)
	}
	h.clock.Advance(collectorInterval)
	eventually(t, "the proposal of "+id+" to be stored", func() bool { return len(h.proposals(t, id, sandbox.ProposalOpen)) == 1 })
}
