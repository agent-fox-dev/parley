package codeindex_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// demoPart is the part of a tree's path that names the workspace demo, for
// holding its builds.
var demoPart = string(filepath.Separator) + "demo" + string(filepath.Separator)

// later sends a query of the workspace demo by the owner beside the test
// and returns where its answer arrives.
func (w *world) later(t *testing.T, route, query string) <-chan answered {
	t.Helper()
	ch := make(chan answered, 1)
	go func() { ch <- w.ask(t, "demo", route, query, w.key) }()
	return ch
}

// waits fails t when the query behind ch answers within 300 ms.
func waits(t *testing.T, ch <-chan answered, what string) {
	t.Helper()
	select {
	case a := <-ch:
		t.Fatalf("%s answered %d %s while the build of the head is held, want it to wait", what, a.code, a.body)
	case <-time.After(300 * time.Millisecond):
	}
}

// arrives is the answer behind ch, within 20 s.
func arrives(t *testing.T, ch <-chan answered, what string) answered {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(20 * time.Second):
		t.Fatalf("waited 20 s for %s to answer", what)
		return answered{}
	}
}

func TestWait_AnswersOnceTheIndexHoldsTheHeadTheRequestArrivedAt(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, enabled, s.adjust)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")

	release := s.hold(demoPart)
	t.Cleanup(release)
	b := w.commit(t, "demo", "main", map[string]string{"fresh.go": "package a\n\n// freshword\nfunc FreshFunc() {}\n"})
	s.waitBegan(t, demoPart)
	search, symbols := w.later(t, "code-search", "q=freshword"), w.later(t, "symbols", "name=FreshFunc")
	waits(t, search, "the search")
	waits(t, symbols, "the symbol lookup")
	release()

	var found searchAnswer
	if a := arrives(t, search, "the search"); a.code != http.StatusOK || json.Unmarshal(a.body, &found) != nil ||
		found.IndexedSHA == nil || *found.IndexedSHA != b || !slices.Equal(found.paths(), []string{"fresh.go"}) {
		t.Errorf("once the build ended the search answers %d %s, want 200 with indexed_sha %s and fresh.go", a.code, a.body, b)
	}
	var syms symbolsAnswer
	if a := arrives(t, symbols, "the symbol lookup"); a.code != http.StatusOK || json.Unmarshal(a.body, &syms) != nil ||
		syms.IndexedSHA == nil || *syms.IndexedSHA != b || !slices.Equal(syms.names(), []string{"fresh.go FreshFunc"}) {
		t.Errorf("once the build ended the symbol lookup answers %d %s, want 200 with indexed_sha %s and FreshFunc", a.code, a.body, b)
	}
}

// The request arrived at B; B is never indexed, the head moves on to C, and
// the index of C answers.
func TestWait_AnswersFromALaterHeadThanTheOneTheRequestArrivedAt(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, enabled, s.adjust)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")

	release := s.hold(demoPart)
	t.Cleanup(release)
	b := w.commit(t, "demo", "main", map[string]string{"b.go": "package a\n\n// arrivalword\n"})
	s.waitBegan(t, demoPart)
	search := w.later(t, "code-search", "q=arrivalword")
	waits(t, search, "the search")
	c := w.commit(t, "demo", "main", map[string]string{"c.go": "package a\n"})
	s.failWith(func(tree string) error {
		if strings.Contains(tree, b) {
			return errors.New("the build of the arrival head is lost")
		}
		return nil
	})
	release()

	var found searchAnswer
	if a := arrives(t, search, "the search"); a.code != http.StatusOK || json.Unmarshal(a.body, &found) != nil ||
		found.IndexedSHA == nil || *found.IndexedSHA != c || !slices.Equal(found.paths(), []string{"b.go"}) {
		t.Errorf("the search answers %d %s, want 200 from the index of the later head %s, which holds b.go", a.code, a.body, c)
	}
}

func TestWait_NeverAnswersFromACommitOlderThanTheHeadAtArrival(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	first := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")

	const commits, searchers = 20, 8
	var mu sync.Mutex
	// The commit that added word i, to its number; the first commit, which
	// holds none, is older than them all.
	order := map[string]int{first: -1}
	newest := -1
	done := make(chan struct{})
	var wg sync.WaitGroup
	for range searchers {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				mu.Lock()
				i := newest
				mu.Unlock()
				if i < 0 {
					time.Sleep(time.Millisecond)
					continue
				}
				a := w.ask(t, "demo", "code-search", fmt.Sprintf("q=raceword%02d", i), w.key)
				if a.code != http.StatusOK {
					t.Errorf("the search for word %d = %d %s, want 200", i, a.code, a.body)
					continue
				}
				var got searchAnswer
				_ = json.Unmarshal(a.body, &got)
				mu.Lock()
				at, known := order[str(got.IndexedSHA)]
				mu.Unlock()
				// A commit the test has not recorded yet is one written
				// after the request was sent.
				if known && at < i {
					t.Errorf("the search for word %d was answered from the commit of word %d, older than the head when it arrived", i, at)
				}
				if !slices.Contains(got.paths(), fmt.Sprintf("w%02d.go", i)) {
					t.Errorf("the search for word %d answers %q from %s, want the file that holds it", i, got.paths(), str(got.IndexedSHA))
				}
			}
		})
	}
	for i := range commits {
		sha := w.commit(t, "demo", "main", map[string]string{fmt.Sprintf("w%02d.go", i): fmt.Sprintf("package a\n\n// raceword%02d\n", i)})
		mu.Lock()
		order[sha], newest = i, i
		mu.Unlock()
	}
	w.reaches(t, "demo", w.head(t, "demo"))
	close(done)
	wg.Wait()
}

func TestWait_QueuesTheBuildNoOneElseDid(t *testing.T) {
	t.Parallel()
	w := newComposedWorld(t, hubtest.Config{TOML: enabled, MaintenanceInterval: time.Hour})
	first := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")
	head := w.commit(t, "demo", "main", map[string]string{"b.go": "package a\n\n// lateword\n"})
	w.reaches(t, "demo", head)
	// The record falls behind with no build pending and no pass to see it.
	if _, err := w.Store.Main().Exec(`UPDATE code_index_records SET indexed_sha = ? WHERE workspace_slug = 'demo'`, first); err != nil {
		t.Fatal(err)
	}
	if jobs := w.jobs(t, "demo"); len(jobs) != 0 {
		t.Fatalf("the queue holds %q of demo, want no build", jobs)
	}
	if got, _ := w.search(t, "demo", "lateword"); got.IndexedSHA == nil || *got.IndexedSHA != head || !slices.Equal(got.paths(), []string{"b.go"}) {
		t.Errorf("the search answers %+v, want the index of the head %s", got, head)
	}
}

func TestWait_GivesUpAtTheReadTimeoutAndSaysWhereTheIndexIs(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, "[code_index]\n[scm]\nread_timeout = \"1s\"\n", s.adjust)
	first := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")
	t.Cleanup(s.hold(demoPart))
	t.Cleanup(s.hold(string(filepath.Separator) + "never" + string(filepath.Separator)))
	head := w.commit(t, "demo", "main", map[string]string{"b.go": "package a\n"})
	neverHead := w.addWorkspace(t, "never", map[string]string{"a.go": "package a\n"})

	for _, tc := range []struct {
		slug, route, query, head string
		indexed                  *string
	}{
		{"demo", "code-search", "q=package", head, &first},
		{"demo", "symbols", "name=a", head, &first},
		{"never", "code-search", "q=package", neverHead, nil},
	} {
		start := time.Now()
		a := w.ask(t, tc.slug, tc.route, tc.query, w.key)
		r := a.refusal()
		if a.code != http.StatusServiceUnavailable || r.Error.ErrorType != "code_index_not_ready" || a.header.Get("Retry-After") != "5" ||
			r.Error.State != "building" || str(r.Error.HeadSHA) != tc.head || str(r.Error.IndexedSHA) != str(tc.indexed) || a.header.Get("ETag") != "" {
			t.Errorf("%s of %s: %d %s (Retry-After %q), want 503 code_index_not_ready, Retry-After 5, state building, head_sha %s and indexed_sha %s",
				tc.route, tc.slug, a.code, a.body, a.header.Get("Retry-After"), tc.head, str(tc.indexed))
		}
		if took := time.Since(start); took < 900*time.Millisecond || took > 5*time.Second {
			t.Errorf("%s of %s answered after %s, want after the read timeout of 1s", tc.route, tc.slug, took.Round(time.Millisecond))
		}
	}
}

func TestWait_RefusesAtOnceWhenTheBuildOfTheHeadFailed(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newClockedWorld(t, s)
	first := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.queue(t, "demo")
	w.run(t, "demo")
	b := w.noticed(t, "demo", map[string]string{"b.go": "package a\n\n// failword\n"})
	s.failWith(func(string) error { return errors.New("no luck") })
	w.run(t, "demo")

	start := time.Now()
	a := w.ask(t, "demo", "code-search", "q=failword", w.key)
	r := a.refusal()
	retry, err := strconv.Atoi(a.header.Get("Retry-After"))
	if a.code != http.StatusServiceUnavailable || r.Error.ErrorType != "code_index_not_ready" || r.Error.State != "failed" ||
		str(r.Error.HeadSHA) != b || str(r.Error.IndexedSHA) != first || err != nil || retry < 1 || retry > 4 {
		t.Errorf("%d %s (Retry-After %q), want 503 code_index_not_ready, state failed, head_sha %s, indexed_sha %s and a Retry-After of 1 to 4, the seconds to the next attempt",
			a.code, a.body, a.header.Get("Retry-After"), b, first)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the refusal took %s, want it at once", took.Round(time.Millisecond))
	}

	s.failWith(nil)
	w.clock.Advance(4 * time.Second)
	w.run(t, "demo")
	if got, _ := w.search(t, "demo", "failword"); got.IndexedSHA == nil || *got.IndexedSHA != b {
		t.Errorf("once the build succeeded the search answers %+v, want the index of %s", got, b)
	}
}

func TestWait_ObservesHowLongEachRequestWaited(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, "[code_index]\n[scm]\nread_timeout = \"1s\"\n", s.adjust)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")
	// Refused before the waiting rule: observed under no outcome.
	if a := w.ask(t, "demo", "code-search", "", w.key); a.code != http.StatusBadRequest {
		t.Fatalf("a search without q = %d, want 400", a.code)
	}
	w.search(t, "demo", "package") // at once

	release := s.hold(demoPart)
	t.Cleanup(release)
	w.commit(t, "demo", "main", map[string]string{"b.go": "package a\n"})
	s.waitBegan(t, demoPart)
	waited := w.later(t, "code-search", "q=package")
	waits(t, waited, "the search")
	release()
	if a := arrives(t, waited, "the search"); a.code != http.StatusOK {
		t.Fatalf("the search that waited = %d %s, want 200", a.code, a.body)
	}

	t.Cleanup(s.hold(demoPart))
	w.commit(t, "demo", "main", map[string]string{"c.go": "package a\n"})
	if a := w.ask(t, "demo", "code-search", "q=package", w.key); a.code != http.StatusServiceUnavailable {
		t.Fatalf("the search of a held build = %d %s, want 503", a.code, a.body)
	}

	text := w.metricsText(t)
	for line, want := range map[string]string{
		`parley_code_index_wait_seconds_count{outcome="ready"}`:   "2",
		`parley_code_index_wait_seconds_count{outcome="timeout"}`: "1",
		`parley_code_index_wait_seconds_count{outcome="failed"}`:  "0",
	} {
		if !strings.Contains(text, line+" "+want+"\n") {
			t.Errorf("GET /metrics has no %s %s:\n%s", line, want, grep(text, "parley_code_index_wait_seconds_count"))
		}
	}
	var sum float64
	for l := range strings.SplitSeq(text, "\n") {
		if rest, ok := strings.CutPrefix(l, `parley_code_index_wait_seconds_sum{outcome="ready"} `); ok {
			sum, _ = strconv.ParseFloat(strings.TrimSpace(rest), 64)
		}
	}
	if sum < 0.3 || sum > 20 {
		t.Errorf("the ready requests waited %v s in sum, want at least the 0.3 s the second one was held", sum)
	}
}

// The request arrived at B. The index reaches C, a later head, while the
// branch has moved on to D, whose build has not ended: C answers.
func TestWait_AnswersFromAHeadAfterArrivalThatIsNoLongerTheNewest(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, enabled, s.adjust)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")

	releaseB := s.hold(demoPart)
	t.Cleanup(releaseB)
	b := w.commit(t, "demo", "main", map[string]string{"b.go": "package a\n\n// arrivalword\n"})
	s.waitBegan(t, demoPart)
	search := w.later(t, "code-search", "q=arrivalword")
	waits(t, search, "the search")
	c := w.commit(t, "demo", "main", map[string]string{"c.go": "package a\n"})
	s.failWith(func(tree string) error {
		if strings.Contains(tree, b) {
			return errors.New("the build of the arrival head is lost")
		}
		return nil
	})
	// The build of C is held where it begins, so that D can be committed
	// while it runs; the build of D is held for good.
	sep := string(filepath.Separator)
	releaseC := s.holdOnly(sep + c + sep)
	t.Cleanup(releaseC)
	releaseB()
	s.waitBegan(t, sep+c+sep)
	d := w.commit(t, "demo", "main", map[string]string{"d.go": "package a\n"})
	t.Cleanup(s.holdOnly(sep + d + sep))
	releaseC()

	var found searchAnswer
	if a := arrives(t, search, "the search"); a.code != http.StatusOK || json.Unmarshal(a.body, &found) != nil ||
		str(found.IndexedSHA) != c || !slices.Contains(found.paths(), "b.go") {
		t.Errorf("the search answers %d %s, want 200 from the index of %s, a head after the one it arrived at, while the build of %s runs", a.code, a.body, c, d)
	}
}

// A request that waits is told when its workspace's index is turned off.
func TestWait_EndsWhenTheWorkspacesSettingIsTurnedOff(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, enabled, s.adjust)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")
	t.Cleanup(s.hold(demoPart))
	w.commit(t, "demo", "main", map[string]string{"b.go": "package a\n"})
	s.waitBegan(t, demoPart)
	search := w.later(t, "code-search", "q=package")
	waits(t, search, "the search")

	if code, body := w.Call(t, http.MethodPatch, "/api/v1/workspaces/demo", w.key, map[string]any{"settings": map[string]any{"code_index": false}}); code != http.StatusOK {
		t.Fatalf("PATCH code_index false = %d %s, want 200", code, body)
	}
	if a := arrives(t, search, "the search"); a.code != http.StatusConflict || a.refusal().Error.ErrorType != "code_index_off" {
		t.Errorf("the waiting search answers %d %s, want 409 code_index_off once the setting is off", a.code, a.body)
	}
}

// An index whose files are gone from the disk is no index: the request
// waits for the build that puts it back.
func TestWait_BuildsAgainAnIndexWhoseFilesAreGone(t *testing.T) {
	t.Parallel()
	w := newComposedWorld(t, hubtest.Config{TOML: enabled, MaintenanceInterval: time.Hour})
	head := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n\n// goneword\n"})
	w.indexed(t, "demo")
	if err := os.RemoveAll(filepath.Dir(w.index.TreeDir("demo", head))); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.search(t, "demo", "goneword"); str(got.IndexedSHA) != head || !slices.Equal(got.paths(), []string{"a.go"}) {
		t.Errorf("the search answers %+v, want a.go from an index of %s built again", got, head)
	}
}
