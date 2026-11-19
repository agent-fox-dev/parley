package sandbox_test

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

func TestPolicyCollector_ReportsTheFirstDenialOfADestinationAsOneEvent(t *testing.T) {
	h := startDenialHub(t)
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
	h := startDenialHub(t)
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
	h := startDenialHub(t)
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
	h := startDenialHub(t)
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
