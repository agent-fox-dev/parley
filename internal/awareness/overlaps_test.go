package awareness_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/awareness"
)

func TestPartners_LeaveOutTheChangesOfTheSameStack(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// dependsOn is each change's depends_on, 0 for none.
		dependsOn map[int64]int64
		// want are the partners of each change.
		want map[int64][]int64
	}{
		{
			name:      "two changes in one stack",
			dependsOn: map[int64]int64{1: 0, 2: 1},
			want:      map[int64][]int64{1: {}, 2: {}},
		},
		{
			name:      "three changes in one stack",
			dependsOn: map[int64]int64{1: 0, 2: 1, 3: 2},
			want:      map[int64][]int64{1: {}, 2: {}, 3: {}},
		},
		{
			name:      "two changes on one bottom",
			dependsOn: map[int64]int64{1: 0, 4: 1, 5: 1},
			want:      map[int64][]int64{1: {}, 4: {}, 5: {}},
		},
		{
			name:      "a change outside the stack",
			dependsOn: map[int64]int64{1: 0, 2: 1, 3: 2, 4: 1, 5: 1, 6: 0},
			want:      map[int64][]int64{1: {6}, 2: {6}, 3: {6}, 4: {6}, 5: {6}, 6: {1, 2, 3, 4, 5}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			for _, n := range slices.Sorted(maps.Keys(tc.dependsOn)) {
				ch := f.touching(n, 1, "", "api/a.go")
				if d := tc.dependsOn[n]; d != 0 {
					ch.DependsOn = &d
				}
				f.version(ch)
			}
			f.pass()
			var wantPairs []string
			for _, n := range slices.Sorted(maps.Keys(tc.want)) {
				if got := f.partners(n); !slices.Equal(got, tc.want[n]) {
					t.Errorf("partners of %d = %v, want %v", n, got, tc.want[n])
				}
				for _, p := range tc.want[n] {
					if n < p {
						wantPairs = append(wantPairs, fmt.Sprintf("%d-%d", n, p), fmt.Sprintf("%d-%d", n, p))
					}
				}
			}
			slices.Sort(wantPairs)
			if got := f.announced(); !slices.Equal(got, wantPairs) {
				t.Errorf("announced = %v, want %v (no pair within a stack)", got, wantPairs)
			}
		})
	}
}

func TestOverlapsRoute_ListsTheOtherInFlightChangesOnTheTargetByCount(t *testing.T) {
	t.Parallel()
	s := newServed(t)
	for _, c := range []struct {
		number int64
		state  string
		target string
		paths  []string
	}{
		{1, awareness.StateOpen, "main", []string{"a", "b", "c", "d"}},
		{2, awareness.StateDraft, "main", []string{"a"}},
		{3, awareness.StateOpen, "main", []string{"a", "b", "c"}},
		{4, awareness.StateOpen, "main", []string{"d"}},
		{5, awareness.StateOpen, "release", []string{"a"}},
		{6, awareness.StateAbandoned, "main", []string{"a"}},
		{7, awareness.StateOpen, "main", []string{"z"}},
	} {
		ch := s.touching(c.number, 1, "", c.paths...)
		ch.TargetBranch = c.target
		if c.state == awareness.StateAbandoned {
			s.version(ch)
			ch.State = c.state
			s.emit(eventAbandon, ch)
			continue
		}
		ch.State = c.state
		s.version(ch)
	}
	s.pass()

	page := s.overlaps(t, "changes/1/overlaps")
	if got, want := page.counts(), []string{"3:3", "2:1", "4:1"}; !slices.Equal(got, want) {
		t.Errorf("overlaps of 1 (number:count) = %v, want %v", got, want)
	}
	if page.NextCursor != nil {
		t.Errorf("next_cursor = %q, want null", *page.NextCursor)
	}
	if got := page.Items[0].Paths; !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("paths in common with 3 = %v, want [a b c]", got)
	}

	first := s.overlaps(t, "changes/1/overlaps?limit=2")
	if got, want := first.counts(), []string{"3:3", "2:1"}; !slices.Equal(got, want) {
		t.Errorf("first page of 2 = %v, want %v", got, want)
	}
	if first.NextCursor == nil {
		t.Fatal("the first page of 2 has no next_cursor")
	}
	second := s.overlaps(t, "changes/1/overlaps?limit=2&cursor="+url.QueryEscape(*first.NextCursor))
	if got, want := second.counts(), []string{"4:1"}; !slices.Equal(got, want) {
		t.Errorf("second page of 2 = %v, want %v", got, want)
	}
	if second.NextCursor != nil {
		t.Errorf("next_cursor of the last page = %q, want null", *second.NextCursor)
	}
}

func TestOverlapsRoute_ListsNoneForALandedOrAbandonedChange(t *testing.T) {
	t.Parallel()
	landed := func(ch *awareness.Change) string {
		ch.State, ch.Landed = awareness.StateLanded, &awareness.Landed{SHA: strings.Repeat("5", 40), At: time.Now().UTC()}
		return eventLand
	}
	abandoned := func(ch *awareness.Change) string {
		ch.State = awareness.StateAbandoned
		return eventAbandon
	}
	for _, tc := range []struct {
		name string
		end  func(*awareness.Change) string
		// indexed is whether the worker has run after the change ended.
		indexed bool
	}{
		{"landed", landed, true},
		{"abandoned", abandoned, true},
		{"abandoned and not yet indexed", abandoned, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newServed(t)
			one := s.touching(1, 1, "", "api/a.go")
			s.version(one)
			s.version(s.touching(2, 1, "", "api/a.go"))
			s.version(s.touching(3, 1, "", "api/a.go", "api/b.go"))
			s.pass()
			s.emit(tc.end(&one), one)
			if tc.indexed {
				s.pass()
			}

			status, raw := s.get("changes/1/overlaps")
			if status != http.StatusOK {
				t.Fatalf("GET overlaps of 1 = %d %s, want 200", status, raw)
			}
			if got := strings.TrimSpace(string(raw)); got != `{"items":[],"next_cursor":null}` {
				t.Errorf("overlaps of 1 = %s, want {\"items\":[],\"next_cursor\":null}", got)
			}
		})
	}
}

func TestOverlapsRoute_RefusesANumberThatNamesNoChange(t *testing.T) {
	t.Parallel()
	s := newServed(t)
	s.version(s.touching(1, 1, "", "api/a.go"))
	s.version(s.touching(2, 1, "", "api/a.go"))
	s.pass()
	for _, number := range []string{"999", "0", "-1", "abc"} {
		t.Run(number, func(t *testing.T) {
			status, raw := s.get("changes/" + number + "/overlaps")
			var body struct {
				Error struct {
					ErrorType string `json:"error_type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("body %s: %v", raw, err)
			}
			if status != http.StatusNotFound || body.Error.ErrorType != "change_not_found" {
				t.Errorf("GET overlaps of %s = %d %s, want 404 change_not_found", number, status, raw)
			}
		})
	}
}

func TestOverlapsRoute_IsWhatAReadOfTheChangeIncludes(t *testing.T) {
	t.Parallel()
	h := newChangeHub(t)
	one := h.open(t, "feat-a", map[string]string{"api/a.go": "a2\n", "api/b.go": "b\n"})
	two := h.open(t, "feat-b", map[string]string{"api/a.go": "a3\n", "api/b.go": "b2\n"})
	three := h.open(t, "feat-c", map[string]string{"api/b.go": "b3\n"})
	if err := h.comp.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}

	path := fmt.Sprintf("/api/v1/workspaces/demo/changes/%d", one)
	status, raw := h.Call(t, http.MethodGet, path+"/overlaps", h.key, nil)
	if status != http.StatusOK {
		t.Fatalf("GET overlaps = %d %s, want 200", status, raw)
	}
	var listed struct {
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	var page overlapsPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if got, want := page.counts(), []string{fmt.Sprintf("%d:2", two), fmt.Sprintf("%d:1", three)}; !slices.Equal(got, want) {
		t.Fatalf("overlaps of %d = %v, want %v", one, got, want)
	}

	status, raw = h.Call(t, http.MethodGet, path+"?include=overlaps", h.key, nil)
	if status != http.StatusOK {
		t.Fatalf("GET change with overlaps = %d %s, want 200", status, raw)
	}
	var read struct {
		Overlaps json.RawMessage `json:"overlaps"`
	}
	if err := json.Unmarshal(raw, &read); err != nil {
		t.Fatal(err)
	}
	if string(read.Overlaps) != string(listed.Items) {
		t.Errorf("include=overlaps = %s, want the overlaps route's items %s", read.Overlaps, listed.Items)
	}
}

// overlapsPage is a page of the overlaps route.
type overlapsPage struct {
	Items []struct {
		Change struct {
			Number int64 `json:"number"`
		} `json:"change"`
		Paths []string `json:"paths"`
		Count int      `json:"count"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// counts are the page's items, each as "number:count", in order.
func (p overlapsPage) counts() []string {
	out := []string{}
	for _, it := range p.Items {
		out = append(out, fmt.Sprintf("%d:%d", it.Change.Number, it.Count))
	}
	return out
}

// overlaps gets the page of the overlaps route at path, which must answer
// 200 with exactly items and next_cursor.
func (s *served) overlaps(t *testing.T, path string) overlapsPage {
	t.Helper()
	status, raw := s.get(path)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", path, status, raw)
	}
	if !jsonHasKeys(t, raw, "items", "next_cursor") {
		t.Errorf("GET %s = %s, want exactly items and next_cursor", path, raw)
	}
	var page overlapsPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestOverlaps_LeaveOpeningAChangeOnClaimedPathsUnaffected(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	// opening opens a change from other, which sets otherFile, lets a pass
	// index it and, with claimed, has the admin claim api/a.go, then opens
	// a change from feat, which sets api/a.go. It returns the answer to
	// that open and feat as a read gives it after the next pass, which
	// indexes it, once the background mergeability check of feat has
	// decided its mergeable, each with the author's id, which is the hub's
	// own owner, as "owner". The read waits for that check so that the two
	// hubs are compared at the same point of it, wherever load has put it.
	opening := func(otherFile string, claimed bool) (h *changeHub, number int64, opened, read map[string]any) {
		h = newChangeHubWith(t, (&clock{now: at}).Now, nil)
		h.open(t, "other", map[string]string{otherFile: "other\n"})
		if err := h.comp.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		if claimed {
			body := map[string]any{"paths": []string{"api/a.go"}, "intent": "rework a"}
			if status, raw := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/claims", h.Token, body); status != http.StatusCreated {
				t.Fatalf("POST claims = %d %s, want 201", status, raw)
			}
		}
		if err := json.Unmarshal(h.opened(t, "feat", map[string]string{"api/a.go": "feat\n"}), &opened); err != nil {
			t.Fatal(err)
		}
		number = int64(opened["number"].(float64))
		if err := h.comp.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			read = nil
			status, raw := h.Call(t, http.MethodGet, fmt.Sprintf("/api/v1/workspaces/demo/changes/%d", number), h.key, nil)
			if status != http.StatusOK || json.Unmarshal(raw, &read) != nil {
				t.Fatalf("GET change %d = %d %s, want 200 with the change", number, status, raw)
			}
			if read["mergeable"] != "unknown" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("change %d still has mergeable unknown after 30 s, want its check decided", number)
			}
		}
		for _, ch := range []map[string]any{opened, read} {
			author, _ := ch["author"].(map[string]any)
			if author == nil || author["actor_id"] != h.owner {
				t.Fatalf("author = %v, want demo's owner %s", ch["author"], h.owner)
			}
			author["actor_id"] = "owner"
		}
		return h, number, opened, read
	}
	_, _, alone, aloneRead := opening("docs/x.md", false)
	h, number, crowded, crowdedRead := opening("api/a.go", true)

	overlaps, err := h.comp.Overlaps(t.Context(), nil, "demo", number)
	if err != nil || len(overlaps) != 1 {
		t.Fatalf("overlaps of %d = %v, %v; want the other change", number, overlaps, err)
	}
	for _, c := range []struct {
		what           string
		crowded, alone map[string]any
	}{{"the answer to the open", crowded, alone}, {"the read after a pass", crowdedRead, aloneRead}} {
		for _, key := range slices.Sorted(maps.Keys(c.alone)) {
			if !reflect.DeepEqual(c.crowded[key], c.alone[key]) {
				t.Errorf("%s: %s = %v on claimed, overlapping paths, want %v as without them", c.what, key, c.crowded[key], c.alone[key])
			}
		}
		if len(c.crowded) != len(c.alone) {
			t.Errorf("%s has %d members on claimed, overlapping paths, want %d as without them", c.what, len(c.crowded), len(c.alone))
		}
	}
	if crowded["state"] != "open" || crowded["version"] != float64(1) {
		t.Errorf("state, version = %v, %v; want open, 1", crowded["state"], crowded["version"])
	}
}
