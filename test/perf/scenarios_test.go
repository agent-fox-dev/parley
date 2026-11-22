package perf_test

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/test/perf"
)

// run runs the harness against s with cfg and fails the test when the run
// does.
func run(t *testing.T, cfg perf.Config) perf.Report {
	t.Helper()
	rep, err := perf.Run(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Run = %v, want no error", err)
	}
	return rep
}

// checkResult checks the samples, the target and the status of the
// scenario's result and returns its measurement.
func checkResult(t *testing.T, rep perf.Report, name string, samples int, target perf.Target, status perf.Status) float64 {
	t.Helper()
	r := result(t, rep, name)
	if r.Samples != samples || r.Target != target || r.Status != status || r.Measured == nil {
		t.Errorf("%s = %+v, want %d samples, target %+v, status %s and a measurement", name, r, samples, target, status)
		return math.NaN()
	}
	return *r.Measured
}

func TestRun_TimesEachRequestOfTheRequestDurationScenarios(t *testing.T) {
	t.Parallel()
	t.Run("within the targets", func(t *testing.T) {
		t.Parallel()
		s := newStubWith(t, stubOptions{forkDelay: func(int) time.Duration { return 20 * time.Millisecond }}, nil)
		cfg := s.config(t)
		cfg.SandboxProfile = "agent"
		rep := run(t, cfg)
		tests := []struct {
			name    string
			samples int
			value   float64
		}{
			{"fork_create", 50, 1000},
			{"file_read", 200, 100},
			{"commit_create", 50, 500},
			{"sandbox_exec", 50, 200},
		}
		for _, tt := range tests {
			checkResult(t, rep, tt.name, tt.samples, perf.Target{Comparison: perf.LT, Value: tt.value, Unit: "ms"}, perf.Met)
		}
		if m := result(t, rep, "fork_create").Measured; m == nil || *m < 20 {
			t.Errorf("fork_create measured %v, want the p95 of requests that each took 20 ms or more", m)
		}
		for _, route := range []string{"GET /api/v1/workspaces/w/scm/file", "GET /api/v1/workspaces/w/scm/tree"} {
			if n := s.count(route); n != 100 {
				t.Errorf("%d requests %s, want 100", n, route)
			}
		}
		commits := s.commitsOf("commit_create")
		if len(commits) != 50 {
			t.Errorf("commit_create sent %d commits, want 50", len(commits))
		}
		for i, c := range commits {
			if len(c.paths) != 100 || !strings.HasSuffix(c.workspace, "-f51") {
				t.Errorf("commit %d of commit_create holds %d operations in %s, want 100 in the scenario's fork", i, len(c.paths), c.workspace)
			}
		}
		// sandbox_ready creates five, sandbox_exec one.
		if got := s.sandboxProfiles(); !slices.Equal(got, slices.Repeat([]string{"agent"}, 6)) {
			t.Errorf("sandbox creations named %q, want six naming agent", got)
		}
		if u := s.unauthorizedRequests(); len(u) > 0 {
			t.Errorf("requests without the bearer token: %q", u)
		}
	})
	t.Run("slow forks miss", func(t *testing.T) {
		t.Parallel()
		// The nearest-rank p95 of 50 requests is the 48th: three slow
		// requests are the fewest that put it over the target.
		slow := func(n int) time.Duration {
			if n == 7 || n == 23 || n == 41 {
				return 1100 * time.Millisecond
			}
			return 0
		}
		s := newStubWith(t, stubOptions{forkDelay: slow}, nil)
		rep := run(t, s.config(t))
		m := checkResult(t, rep, "fork_create", 50, perf.Target{Comparison: perf.LT, Value: 1000, Unit: "ms"}, perf.Missed)
		if m < 1100 {
			t.Errorf("fork_create measured %v, want one of the slow requests", m)
		}
	})
}

func TestRun_MeasuresLandingLatencyThroughputAndConcurrency(t *testing.T) {
	t.Parallel()
	s := newStubWith(t, stubOptions{
		landDelay: 50 * time.Millisecond,
		landFails: func(scenario string, n int) bool { return scenario == "land_concurrency" && (n == 3 || n == 11) },
	}, nil)
	rep := run(t, s.config(t))

	if m := checkResult(t, rep, "land", 20, perf.Target{Comparison: perf.LT, Value: 2000, Unit: "ms"}, perf.Met); m < 50 {
		t.Errorf("land measured %v, want the p95 of lands that each took 50 ms or more", m)
	}
	if n := len(s.commitsOf("land")); n != 400 {
		t.Errorf("land sent %d commits, want 20 changes of 20", n)
	}

	m := checkResult(t, rep, "land_throughput", 120, perf.Target{Comparison: perf.GE, Value: 60, Unit: "per_minute"}, perf.Met)
	tp := s.landsOf("land_throughput")
	if tp.count != 120 || tp.max > 8 || tp.max < 2 {
		t.Errorf("land_throughput sent %d land requests, at most %d in flight; want 120, from 2 to 8 in flight", tp.count, tp.max)
	}
	// The rate is the lands over the time the harness spent landing, which
	// spans every land request the stub saw, from the first one's arrival
	// to the last one's answer.
	var landed, of int
	var took float64
	tr := result(t, rep, "land_throughput")
	if _, err := fmt.Sscanf(tr.Detail, "%d of %d landed in %f s", &landed, &of, &took); err != nil || landed != 120 || of != 120 {
		t.Fatalf("land_throughput's detail %q (%v), want 120 of 120 landed in a time", tr.Detail, err)
	}
	if want := 120 * 60 / took; math.Abs(m-want) > want/100 {
		t.Errorf("land_throughput measured %v per minute, want 120 lands per %v s, %v", m, took, want)
	}
	if span := tp.last.Sub(tp.first).Seconds(); took < span-0.001 {
		t.Errorf("land_throughput landed in %v s, less than the %v s from the first land request to the last answer", took, span)
	}

	if m := checkResult(t, rep, "land_concurrency", 16, perf.Target{Comparison: perf.LE, Value: 0, Unit: "count"}, perf.Missed); m != 2 {
		t.Errorf("land_concurrency measured %v, want the 2 attempts that failed", m)
	}
	if c := s.landsOf("land_concurrency"); c.count != 16 || c.max != 16 {
		t.Errorf("land_concurrency sent %d land requests, at most %d in flight; want 16 at once", c.count, c.max)
	}

	// Every change's files are its own, so that no land conflicts.
	owner := map[string]string{}
	for _, c := range s.allCommits() {
		for _, p := range c.paths {
			if b, ok := owner[p]; ok && b != c.branch && scenarioOf(c.branch) != "commit_create" {
				t.Fatalf("%s is written by %s and %s", p, b, c.branch)
			}
			owner[p] = c.branch
		}
	}
}

func TestRun_MeasuresMergeabilityAndEventLatency(t *testing.T) {
	t.Parallel()
	t.Run("measured", func(t *testing.T) {
		t.Parallel()
		s := newStubWith(t, stubOptions{mergeDelay: 2 * time.Second, eventDelay: 30 * time.Millisecond}, nil)
		rep := run(t, s.config(t))
		m := checkResult(t, rep, "mergeability", 100, perf.Target{Comparison: perf.LE, Value: 60, Unit: "s"}, perf.Met)
		if m < 2 || m >= 5 {
			t.Errorf("mergeability measured %v s, want from 2 to 5", m)
		}
		polls := s.mergeabilityPolls()
		for i := 1; i < len(polls); i++ {
			if gap := polls[i].Sub(polls[i-1]); gap < time.Second {
				t.Errorf("mergeability polled %v after its previous poll, want a second or more", gap)
			}
		}
		e := checkResult(t, rep, "event_latency", 50, perf.Target{Comparison: perf.LE, Value: 1000, Unit: "ms"}, perf.Met)
		if e < 30 || e >= 1000 {
			t.Errorf("event_latency measured %v ms, want from 30 to 1000", e)
		}
		if n := len(s.commitsOf("event_latency")); n != 50 {
			t.Errorf("event_latency sent %d commits, want 50", n)
		}
	})
	t.Run("an event that does not come", func(t *testing.T) {
		t.Parallel()
		s := newStubWith(t, stubOptions{withholdEvent: 10}, nil)
		cfg := s.config(t)
		cfg.EventWait = 200 * time.Millisecond
		rep := run(t, cfg)
		if r := result(t, rep, "event_latency"); r.Status != perf.Error || r.Measured != nil || !strings.Contains(r.Detail, "event") {
			t.Errorf("event_latency = %+v, want error naming the event that did not come", r)
		}
	})
	t.Run("changes that stay unknown", func(t *testing.T) {
		t.Parallel()
		s := newStubWith(t, stubOptions{mergeDelay: time.Hour}, nil)
		cfg := s.config(t)
		// Long enough for the scenario to open its changes and poll, so
		// that the deadline ends it while the changes are still unknown.
		cfg.ScenarioDeadline = 3 * time.Second
		rep := run(t, cfg)
		if r := result(t, rep, "mergeability"); r.Status != perf.Error || r.Detail != "deadline exceeded" {
			t.Errorf("mergeability = %+v, want error saying deadline exceeded", r)
		}
		if n := len(s.mergeabilityPolls()); n == 0 {
			t.Errorf("mergeability never listed its unknown changes before the deadline, want it to have polled")
		}
	})
}

// check asserts what holds after any run: nothing the run created is
// left, and every change and branch it made is its own.
func check(t *testing.T, s *stub, rep perf.Report) {
	t.Helper()
	// A stream the harness closed ends on the stub when it reads the
	// closed connection, a moment later.
	for deadline := time.Now().Add(2 * time.Second); s.openStreams() > 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if f, sb, st := s.liveForks(), s.liveSandboxes(), s.openStreams(); f != 0 || sb != 0 || st != 0 {
		t.Errorf("after the run %d forks, %d sandboxes and %d streams are left, want none", f, sb, st)
	}
	target := "perf/" + rep.RunID
	for _, got := range s.changeTargets() {
		if got != target {
			t.Errorf("a change targets %q, want %q", got, target)
		}
	}
	for _, got := range s.createdRefs() {
		if got != target {
			t.Errorf("the run created the branch %q, want only %q", got, target)
		}
	}
	for _, c := range s.allCommits() {
		if c.branch == stubDefaultBranch || c.branch == target {
			t.Errorf("a commit was sent to %s of %s", c.branch, c.workspace)
		}
	}
}

func TestRun_RemovesWhatItCreatedAndKeepsToItsOwnTargetBranch(t *testing.T) {
	t.Parallel()
	t.Run("every request succeeds", func(t *testing.T) {
		t.Parallel()
		s := newStub(t, nil)
		cfg := s.config(t)
		cfg.SandboxProfile = "agent"
		rep := run(t, cfg)
		check(t, s, rep)
		if s.createdForks() == 0 || s.createdSandboxes() == 0 || s.openedStreams() == 0 || len(s.changeTargets()) == 0 {
			t.Errorf("the run created %d forks, %d sandboxes, %d streams and %d changes, want some of each",
				s.createdForks(), s.createdSandboxes(), s.openedStreams(), len(s.changeTargets()))
		}
		if len(rep.CleanupErrors) != 0 {
			t.Errorf("cleanup_errors = %q, want none", rep.CleanupErrors)
		}
	})
	t.Run("commits fail from the tenth", func(t *testing.T) {
		t.Parallel()
		s := newStubWith(t, stubOptions{failCommitsFrom: 10}, nil)
		cfg := s.config(t)
		cfg.SandboxProfile = "agent"
		rep := run(t, cfg)
		check(t, s, rep)
		// The scenarios whose commits failed ended in error, and what they
		// created was removed all the same.
		for _, name := range []string{"commit_create", "land", "event_latency"} {
			if r := result(t, rep, name); r.Status != perf.Error {
				t.Errorf("%s = %+v, want error", name, r)
			}
		}
		if s.openedStreams() == 0 {
			t.Errorf("event_latency opened no stream before its commits failed")
		}
	})
	t.Run("the first fork delete fails", func(t *testing.T) {
		t.Parallel()
		s := newStubWith(t, stubOptions{failForkDeletes: 1}, nil)
		rep := run(t, s.config(t))
		if len(rep.CleanupErrors) != 1 || !strings.Contains(rep.CleanupErrors[0], "DELETE") ||
			!strings.Contains(rep.CleanupErrors[0], "500") || !strings.Contains(rep.CleanupErrors[0], "/workspaces/w-f") {
			t.Errorf("cleanup_errors = %q, want one naming DELETE, the fork and 500", rep.CleanupErrors)
		}
		if r := result(t, rep, "fork_create"); r.Status != perf.Met {
			t.Errorf("fork_create = %+v, want met", r)
		}
	})
}

func TestRun_CountsWhatExistsAtOnceUntilTheCountOrTheHubsRefusal(t *testing.T) {
	t.Parallel()
	s := newStubWith(t, stubOptions{forkLimit: 200, sandboxLimit: 20}, nil)
	cfg := s.config(t)
	cfg.Capacity, cfg.SandboxProfile, cfg.Workspaces = true, "agent", []string{"w1", "w2"}
	rep := run(t, cfg)

	count := func(v float64) perf.Target { return perf.Target{Comparison: perf.GE, Value: v, Unit: "count"} }
	f := result(t, rep, "active_forks")
	if f.Measured == nil || *f.Measured != 400 || f.Status != perf.Missed || f.Target != count(5000) ||
		!strings.Contains(f.Detail, stubForkLimit) {
		t.Errorf("active_forks = %+v, want 400 missed against 5000, its detail naming %s", f, stubForkLimit)
	}
	// Each workspace was asked once more than it accepted, after the
	// forks of the latency scenarios were deleted.
	for _, slug := range []string{"w1", "w2"} {
		if n := s.count("POST /api/v1/workspaces/" + slug + "/forks"); n < 201 {
			t.Errorf("%d fork creations in %s, want the 200 it accepts and its refusal", n, slug)
		}
	}

	if m := checkResult(t, rep, "open_changes", 2000, count(2000), perf.Met); m != 2000 {
		t.Errorf("open_changes measured %v, want 2000", m)
	}
	if opened, open := s.changesOf("open_changes"); opened != 2000 || open != 0 {
		t.Errorf("open_changes opened %d changes and left %d open, want 2000 and none", opened, open)
	}

	if m := checkResult(t, rep, "event_streams", 500, count(500), perf.Met); m != 500 {
		t.Errorf("event_streams measured %v, want 500", m)
	}
	if n := s.mostStreams(); n != 500 {
		t.Errorf("at most %d event streams were open at once, want 500", n)
	}

	sb := result(t, rep, "sandboxes_running")
	if sb.Measured == nil || *sb.Measured != 20 || sb.Status != perf.Missed || sb.Target != count(200) ||
		!strings.Contains(sb.Detail, "sandbox_capacity") {
		t.Errorf("sandboxes_running = %+v, want 20 missed against 200, its detail naming sandbox_capacity", sb)
	}

	check(t, s, rep)
	if len(rep.CleanupErrors) != 0 {
		t.Errorf("cleanup_errors = %q, want none", rep.CleanupErrors)
	}
	if u := s.unauthorizedRequests(); len(u) > 0 {
		t.Errorf("requests without the bearer token: %q", u)
	}
}

func TestRun_MeasuresHowLongASandboxTakesToBeReady(t *testing.T) {
	t.Parallel()
	ready := perf.Target{Comparison: perf.LE, Value: 60, Unit: "s"}
	t.Run("every sandbox gets ready", func(t *testing.T) {
		t.Parallel()
		s := newStubWith(t, stubOptions{provisioning: 300 * time.Millisecond}, nil)
		cfg := s.config(t)
		cfg.SandboxProfile = "agent"
		rep := run(t, cfg)
		if m := checkResult(t, rep, "sandbox_ready", 5, ready, perf.Met); m < 0.3 || m >= 5 {
			t.Errorf("sandbox_ready measured %v s, want from 0.3 to 5", m)
		}
		for i, p := range s.sandboxProfiles() {
			if p != "agent" {
				t.Errorf("sandbox creation %d named profile %q, want agent", i+1, p)
			}
		}
		check(t, s, rep)
	})
	t.Run("the third sandbox fails", func(t *testing.T) {
		t.Parallel()
		s := newStubWith(t, stubOptions{failSandbox: 3}, nil)
		cfg := s.config(t)
		cfg.SandboxProfile = "agent"
		rep := run(t, cfg)
		if r := result(t, rep, "sandbox_ready"); r.Status != perf.Error || r.Measured != nil || !strings.Contains(r.Detail, "image_pull") {
			t.Errorf("sandbox_ready = %+v, want error naming the state_reason image_pull", r)
		}
		if n := s.createdSandboxes(); n != 4 {
			t.Errorf("%d sandboxes were created, want three of sandbox_ready and one of sandbox_exec", n)
		}
		check(t, s, rep)
	})
}
