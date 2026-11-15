package scm_test

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/agent-fox-dev/parley/internal/scm"
)

// blameRange is a range of a blame answer: its lines, the sha of its
// commit and its original line.
type blameRange struct {
	start, end int
	sha        string
	origStart  int
}

// ranges are the ranges of a blame answer.
func (r reply) ranges(t *testing.T) []blameRange {
	t.Helper()
	list, ok := r.json["ranges"].([]any)
	if r.status != http.StatusOK || !ok {
		t.Fatalf("%d %s, want 200 with ranges", r.status, r.body)
	}
	got := []blameRange{}
	for _, it := range list {
		o, _ := it.(map[string]any)
		cm, _ := o["commit"].(map[string]any)
		s, _ := cm["sha"].(string)
		start, _ := o["start_line"].(float64)
		end, _ := o["end_line"].(float64)
		orig, _ := o["original_start_line"].(float64)
		got = append(got, blameRange{int(start), int(end), s, int(orig)})
	}
	return got
}

// rangeOf is the range of a blame answer whose commit is sha.
func (r reply) rangeOf(t *testing.T, sha string) map[string]any {
	t.Helper()
	list, _ := r.json["ranges"].([]any)
	for _, it := range list {
		o, _ := it.(map[string]any)
		if cm, _ := o["commit"].(map[string]any); cm["sha"] == sha {
			return o
		}
	}
	t.Fatalf("%d %s, want a range of %s", r.status, r.body, sha)
	return nil
}

func TestBlame_GroupsTheWholeFileIntoRangesOfOneCommit(t *testing.T) {
	w := newWorld(t)
	id := w.addBlame(t)
	for _, q := range []string{"ref=main&path=f.txt", "path=f.txt"} {
		r := w.get(t, scmPath("blame", "/blame?"+q), w.ownerKey)
		want := []blameRange{{1, 2, id["c1"], 1}, {3, 3, id["c3"], 3}, {4, 6, id["c2"], 4}}
		if got := r.ranges(t); !reflect.DeepEqual(got, want) || r.json["resolved_sha"] != id["main"] || r.json["path"] != "f.txt" {
			t.Errorf("%s: %s, want resolved_sha %s, path f.txt and the ranges %v", q, r.body, id["main"], want)
		}
		if first := r.rangeOf(t, id["c1"]); first["original_path"] != "f.txt" {
			t.Errorf("%s: the first range = %v, want original_path f.txt", q, first)
		}
	}
	if r := w.get(t, scmPath("blame", "/blame?path=empty.txt"), w.ownerKey); len(r.ranges(t)) != 0 {
		t.Errorf("empty.txt: %s, want ranges []", r.body)
	}
}

func TestBlame_ReturnsOnlyTheRangesInsideALineSpan(t *testing.T) {
	w := newWorld(t)
	id := w.addBlame(t)
	c1, c2, c3 := id["c1"], id["c2"], id["c3"]
	for _, c := range []struct {
		span string
		want []blameRange
	}{
		{"start_line=2&end_line=4", []blameRange{{2, 2, c1, 2}, {3, 3, c3, 3}, {4, 4, c2, 4}}},
		{"start_line=5&end_line=999", []blameRange{{5, 6, c2, 5}}},
		{"start_line=6", []blameRange{{6, 6, c2, 6}}},
		{"end_line=1", []blameRange{{1, 1, c1, 1}}},
	} {
		r := w.get(t, scmPath("blame", "/blame?path=f.txt&"+c.span), w.ownerKey)
		if got := r.ranges(t); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ranges %v, want %v", c.span, got, c.want)
		}
	}
}

func TestBlame_DescribesTheCommitOfEachRangeFollowingRenames(t *testing.T) {
	var sha string
	w := newWorld(t, func(d *scm.Deps) { d.Changes = commitLookups{&sha} })
	id := w.addBlame(t)
	sha = id["c2"]

	r := w.get(t, scmPath("blame", "/blame?path=renamed.txt"), w.ownerKey)
	if got, want := r.ranges(t), []blameRange{{1, 4, id["c1"], 1}, {5, 5, id["c4"], 5}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("renamed.txt: ranges %v, want %v", got, want)
	}
	before, after := r.rangeOf(t, id["c1"]), r.rangeOf(t, id["c4"])
	if before["original_path"] != "before.txt" || after["original_path"] != "renamed.txt" {
		t.Errorf("original paths %v and %v, want before.txt for the lines from before the rename, renamed.txt after", before["original_path"], after["original_path"])
	}
	// The commit of a range is the commit object every route answers, without
	// stats.
	commit := w.get(t, scmPath("blame", "/commits/"+id["c1"]), w.ownerKey).json
	delete(commit, "stats")
	if !reflect.DeepEqual(before["commit"], commit) {
		t.Errorf("the commit of the range of c1 = %v, want the commit object %v without stats", before["commit"], commit)
	}

	f := w.get(t, scmPath("blame", "/blame?path=f.txt"), w.ownerKey)
	cm, _ := f.rangeOf(t, id["c2"])["commit"].(map[string]any)
	if want := map[string]any{"workspace_slug": "demo", "number": 12.0, "version": 3.0}; !reflect.DeepEqual(cm["change"], want) {
		t.Errorf("the commit of the range of c2, which the change lookup knows: change %v, want %v", cm["change"], want)
	}
}

func TestBlame_RefusesAPathOrSpanItCannotBlame(t *testing.T) {
	w := newWorld(t)
	w.addBlame(t)
	for _, c := range []struct {
		query     string
		status    int
		errorType string
	}{
		{"", http.StatusBadRequest, "invalid_request"},
		{"path=nope", http.StatusNotFound, "path_not_found"},
		{"path=docs", http.StatusBadRequest, "invalid_request"},
		{"path=sub", http.StatusBadRequest, "invalid_request"},
		{"path=bin.dat", http.StatusBadRequest, "invalid_request"},
		{"path=big.txt", http.StatusRequestEntityTooLarge, "payload_too_large"},
		{"path=f.txt&start_line=0", http.StatusBadRequest, "invalid_request"},
		{"path=f.txt&start_line=5&end_line=4", http.StatusBadRequest, "invalid_request"},
		{"path=f.txt&start_line=7", http.StatusBadRequest, "invalid_request"},
		{"path=f.txt&start_line=x", http.StatusBadRequest, "invalid_request"},
		{"path=f.txt&end_line=2.5", http.StatusBadRequest, "invalid_request"},
		{"path=f.txt&ref=nope", http.StatusNotFound, "ref_not_found"},
	} {
		r := w.get(t, scmPath("blame", "/blame?"+c.query), w.ownerKey)
		if r.status != c.status || r.errorType() != c.errorType {
			t.Errorf("%q: %d %s, want %d %s", c.query, r.status, r.body, c.status, c.errorType)
		}
	}
	r := w.get(t, scmPath("blame", "/blame?path=edge.txt"), w.ownerKey)
	if got := r.ranges(t); len(got) != 1 || got[0].end != 32768 {
		t.Errorf("edge.txt, exactly 2 MiB: ranges %v, want one range of its %d lines", got, 32768)
	}
	big := w.get(t, scmPath("blame", "/blame?path=big.txt"), w.ownerKey)
	if e, _ := big.json["error"].(map[string]any); e["size"] != float64(len(blameEdge)+1) {
		t.Errorf("big.txt: %s, want the size %d inside error", big.body, len(blameEdge)+1)
	}
}
