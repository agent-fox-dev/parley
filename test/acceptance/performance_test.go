package acceptance

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// The size of the reference origin and the number of samples of each
// latency target.
const (
	referenceCommits = 1000
	referenceFiles   = 5000
	samples          = 100
)

// p95 is the 95th percentile of d, by the nearest-rank method.
func p95(d []time.Duration) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[int(math.Ceil(0.95*float64(len(s))))-1]
}

// timed returns how long fn took.
func timed(fn func()) time.Duration {
	start := time.Now()
	fn()
	return time.Since(start)
}

// fastImport runs git fast-import in the repository whose git directory is
// dir, reading the stream.
func fastImport(t *testing.T, dir, stream string) {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+dir, "fast-import", "--quiet", "--date-format=now")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	cmd.Stdin = strings.NewReader(stream)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git fast-import: %v\n%s", err, stderr.String())
	}
}

// referenceOrigin is an origin whose main has 1,000 commits over 5,000
// files in 50 directories: the first commit adds the files, every later one
// changes one of them.
func referenceOrigin(t *testing.T, hub *harness.Hub) *harness.Origin {
	t.Helper()
	o := hub.Origin()
	var b strings.Builder
	commit := func(msg string) {
		fmt.Fprintf(&b, "commit refs/heads/main\ncommitter Acceptance <acceptance@example.test> now\ndata %d\n%s\n", len(msg), msg)
	}
	commit("Add the reference files")
	fmt.Fprintf(&b, "from %s\n", o.Main)
	for i := range referenceFiles {
		content := fmt.Sprintf("file %d\n", i)
		fmt.Fprintf(&b, "M 100644 inline %s\ndata %d\n%s\n", referenceFile(i), len(content), content)
	}
	for c := 1; c < referenceCommits; c++ {
		commit(fmt.Sprint("Change ", c))
		content := fmt.Sprintf("file %d, change %d\n", c, c)
		fmt.Fprintf(&b, "M 100644 inline %s\ndata %d\n%s\n", referenceFile(c), len(content), content)
	}
	fastImport(t, o.Dir(), b.String())
	o.Publish()
	o.Main = o.Head("main")
	return o
}

// referenceFile is the path of the reference origin's file i.
func referenceFile(i int) string { return fmt.Sprintf("dir-%02d/file-%04d.txt", i%50, i) }

// preparedChanges pushes n branches to demo, branch i made of commits
// commits on main that each add a file of its own, and opens a change of
// each through the API, returning their numbers.
func preparedChanges(t *testing.T, hub *harness.Hub, demo *harness.Workspace, prefix string, n, commits int) []int {
	t.Helper()
	clone := hub.Clone(demo.Remote, demo.Owner.Key)
	main := hub.Rev(clone, "origin/main")
	var b strings.Builder
	refspecs := []string{"push", "-q", "origin"}
	for i := range n {
		branch := fmt.Sprintf("%s-%03d", prefix, i)
		for c := range commits {
			msg := fmt.Sprintf("%s commit %d", branch, c)
			fmt.Fprintf(&b, "commit refs/heads/%s\ncommitter Acceptance <acceptance@example.test> now\ndata %d\n%s\n", branch, len(msg), msg)
			if c == 0 {
				fmt.Fprintf(&b, "from %s\n", main)
			}
			content := msg + "\n"
			fmt.Fprintf(&b, "M 100644 inline %s/%d.txt\ndata %d\n%s\n", branch, c, len(content), content)
		}
		refspecs = append(refspecs, "refs/heads/"+branch)
	}
	fastImport(t, clone+"/.git", b.String())
	hub.Git(clone, "", refspecs...).Must(t)
	numbers := make([]int, n)
	for i := range n {
		branch := fmt.Sprintf("%s-%03d", prefix, i)
		r := hub.Call(http.MethodPost, "/api/v1/workspaces/demo/changes", demo.Owner.Key, map[string]any{
			"source": map[string]any{"branch": branch}, "target_branch": "main", "title": branch, "intent": "measure " + branch,
		}).Want(t, http.StatusCreated)
		numbers[i] = harness.Num(r.Body["number"])
	}
	return numbers
}

// The performance scenario does not run in parallel with the others: its
// targets are those of one client on an otherwise idle hub and host.
func TestPerformance_SingleClientTargetsOfTheReferenceHost(t *testing.T) {
	harness.Require(t, harness.ReferenceHost)
	hub := harness.Start(t)
	alice := hub.User("alice")
	demo := hub.RegisterWithin(10*time.Minute, alice, "demo", referenceOrigin(t, hub), nil)
	measureLatencies(t, hub, demo)
	measureLandThroughput(t, hub, demo)
	measureEventDelay(t, hub, alice.Key)
}

// measureLatencies checks the p95 of each single-request target over
// samples requests.
func measureLatencies(t *testing.T, hub *harness.Hub, demo *harness.Workspace) {
	t.Helper()
	key := demo.Owner.Key
	ws := "/api/v1/workspaces/demo"
	hundredFiles := func(i int) []any {
		ops := make([]any, 100)
		for f := range ops {
			ops[f] = put(fmt.Sprintf("bulk-%03d/%03d.txt", i, f), fmt.Sprintf("bulk %d %d\n", i, f))
		}
		return ops
	}
	var twentyCommits []int
	latency := []struct {
		name  string
		bound time.Duration
		setup func()
		op    func(i int)
	}{
		{"fork creation", time.Second, nil, func(int) {
			hub.Call(http.MethodPost, ws+"/forks", key, map[string]any{}).Want(t, http.StatusCreated)
		}},
		{"read of one file", 100 * time.Millisecond, nil, func(i int) {
			hub.Call(http.MethodGet, ws+"/scm/file?ref=main&path="+referenceFile(i*37), key, nil).Want(t, http.StatusOK)
		}},
		{"read of one tree", 100 * time.Millisecond, nil, func(i int) {
			hub.Call(http.MethodGet, fmt.Sprintf("%s/scm/tree?ref=main&path=dir-%02d", ws, i%50), key, nil).Want(t, http.StatusOK)
		}},
		{"commit of 100 files", 500 * time.Millisecond, nil, func(i int) {
			hub.Call(http.MethodPost, ws+"/scm/commits", key, map[string]any{
				"branch": fmt.Sprint("bulk-", i), "base_ref": "main", "message": fmt.Sprint("Bulk ", i), "operations": hundredFiles(i),
			}).Want(t, http.StatusCreated)
		}},
		{"land of a change of 20 commits", 2 * time.Second, func() {
			twentyCommits = preparedChanges(t, hub, demo, "twenty", samples, 20)
		}, func(i int) {
			hub.Land(key, "demo", twentyCommits[i])
		}},
	}
	for _, l := range latency {
		if l.setup != nil {
			l.setup()
		}
		d := make([]time.Duration, samples)
		for i := range samples {
			d[i] = timed(func() { l.op(i) })
		}
		if got := p95(d); got >= l.bound {
			t.Errorf("%s: p95 %v over %d samples, want under %v", l.name, got, samples, l.bound)
		} else {
			t.Logf("%s: p95 %v", l.name, got)
		}
	}
}

// measureLandThroughput checks that eight clients land at least 60
// disjoint changes in a minute.
func measureLandThroughput(t *testing.T, hub *harness.Hub, demo *harness.Workspace) {
	t.Helper()
	key, ws := demo.Owner.Key, "/api/v1/workspaces/demo"
	ready := preparedChanges(t, hub, demo, "disjoint", 80, 1)
	var next, landed atomic.Int64
	deadline := time.Now().Add(time.Minute)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				i := int(next.Add(1)) - 1
				if i >= len(ready) {
					return
				}
				r := hub.Call(http.MethodPost, fmt.Sprintf("%s/changes/%d/land?wait=60", ws, ready[i]), key, nil)
				if r.Status == http.StatusOK && time.Now().Before(deadline) {
					landed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if landed.Load() < 60 {
		t.Errorf("%d changes landed on main within one minute, want at least 60", landed.Load())
	}
}

// measureEventDelay checks that a live subscriber receives each claim's
// event within a second of the claim's 201.
func measureEventDelay(t *testing.T, hub *harness.Hub, key string) {
	t.Helper()
	ws := "/api/v1/workspaces/demo"
	stream := hub.Events(key, "types=parley.claim.create")
	var worst time.Duration
	for i := range samples {
		r := hub.Call(http.MethodPost, ws+"/claims", key, map[string]any{"paths": []string{fmt.Sprint("p", i)}, "intent": "measure"}).
			Want(t, http.StatusCreated)
		answered, id := time.Now(), harness.Str(harness.Field(r.Body, "claim.id"))
		var arrived time.Time
		harness.EventuallyWithin(t, 5*time.Second, "the event of claim "+id, func() bool {
			for _, e := range stream.All() {
				if e.Record["resource_id"] == id {
					arrived = e.At
					return true
				}
			}
			return false
		})
		worst = max(worst, arrived.Sub(answered))
	}
	if worst >= time.Second {
		t.Errorf("an event reached the live subscriber %v after the 201 of the request that caused it, want under 1 s", worst)
	}
}
