package change_test

import (
	"context"
	"database/sql"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// stackOf reads GET …/changes/:number/stack of demo with the credential and
// fails the test unless it is answered 200.
func (w *world) stackOf(t *testing.T, number int64, credential string) map[string]any {
	t.Helper()
	status, out, _ := w.get(t, "/workspaces/demo/changes/"+strconv.FormatInt(number, 10)+"/stack", credential)
	if status != http.StatusOK {
		t.Fatalf("GET demo#%d/stack = %d %v, want 200", number, status, out)
	}
	return out
}

// stackNumbers are the numbers of the changes of a decoded stack answer.
func stackNumbers(stack map[string]any) []int64 {
	var out []int64
	list, _ := stack["changes"].([]any)
	for _, item := range list {
		s, _ := item.(map[string]any)
		n, _ := s["number"].(float64)
		out = append(out, int64(n))
	}
	return out
}

func TestStackRoute_ListsTheChainBelowTheChangeAndTheChangesAboveBottomFirst(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	m.hold(t)
	m.holdRestacks(t)
	chain := m.stacked(t, "a", "b", "c")
	alone := m.stacked(t, "x")[0]
	pair := m.stacked(t, "p", "q")
	m.write(t, func(ctx context.Context, tx *sql.Tx) error {
		return m.changes.SetNeedsRestack(ctx, tx, "demo", pair[1].Number, true, new(change.RestackConflict))
	})
	// demo#7 comes from mia's fork, on demo#6 from demo: a token bound to
	// the fork sees only the fork's changes of the stack.
	commit(t, m.t1, "r", pair[1].HeadSHA, "R\n", map[string]string{"r.txt": "r\n"})
	fromFork := m.openBranch(t, "r", func(in *change.OpenInput) { in.SourceWorkspace, in.Caller = "demo--t1", m.caller() })
	bound := m.IssuePAT(t, identity.PATSpec{UserID: m.mia, Permissions: []string{"changes:read"}, Workspace: "demo--t1", CreatedAt: time.Now().UTC()})
	numbers := func(chs ...change.Change) []int64 {
		var out []int64
		for _, ch := range chs {
			out = append(out, ch.Number)
		}
		return out
	}
	members := []string{"head_sha", "landing", "mergeable", "needs_restack", "number", "restack_reason", "state", "title", "version"}
	for _, c := range []struct {
		name       string
		asked      int64
		credential string
		want       []int64
		// conflicted is the change listed with needs_restack for a conflict,
		// 0 for none.
		conflicted int64
	}{
		{"a chain of three asked for its bottom", chain[0].Number, m.ownerKey, numbers(chain...), 0},
		{"a chain of three asked for its middle", chain[1].Number, m.ownerKey, numbers(chain...), 0},
		{"a chain of three asked for its top", chain[2].Number, m.ownerKey, numbers(chain...), 0},
		{"a change with no link and no dependants", alone.Number, m.ownerKey, []int64{alone.Number}, 0},
		{"a chain whose upper change is in conflict", pair[0].Number, m.ownerKey, numbers(pair[0], pair[1], fromFork), pair[1].Number},
		{"a token bound to a fork, on a stack with changes from elsewhere", fromFork.Number, bound, []int64{fromFork.Number}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := m.stackOf(t, c.asked, c.credential)
			if got["target_branch"] != "main" || len(got) != 2 || !slices.Equal(stackNumbers(got), c.want) {
				t.Fatalf("stack of demo#%d = %v, want target_branch main and changes %v", c.asked, got, c.want)
			}
			for _, item := range got["changes"].([]any) {
				s := item.(map[string]any)
				landing, _ := s["landing"].(map[string]any)
				if keys := keysOf(s); !reflect.DeepEqual(keys, members) || !reflect.DeepEqual(keysOf(landing), []string{"status"}) {
					t.Errorf("summary %v, want the members %v and a landing with status only", s, members)
				}
				if head, _ := s["head_sha"].(string); len(head) != 40 {
					t.Errorf("summary %v, want a full head_sha", s)
				}
				conflicted := int64(s["number"].(float64)) == c.conflicted
				if s["needs_restack"] != conflicted || (conflicted && s["restack_reason"] != change.RestackConflict) {
					t.Errorf("summary %v, want needs_restack %v for a conflict", s, conflicted)
				}
			}
			status, read, _ := m.get(t, "/workspaces/demo/changes/"+strconv.FormatInt(c.asked, 10)+"?include=stack", c.credential)
			if status != http.StatusOK || !reflect.DeepEqual(read["stack"], got) {
				t.Errorf("include=stack of demo#%d = %d %v, want the stack route's answer %v", c.asked, status, read["stack"], got)
			}
		})
	}
}

func TestStackRoute_ListsTheDraftAndOpenChangesAboveByDepthThenNumber(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.hold(t)
	w.holdRestacks(t)
	// demo#1 a; on it demo#2 b, demo#3 c and demo#6 f, which is abandoned;
	// demo#4 d on c; demo#5 e on b.
	on := map[string]string{"a": "", "b": "a", "c": "a", "d": "c", "e": "b", "f": "a"}
	heads := map[string]string{"": w.base}
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		heads[name] = commit(t, w.demo, name, heads[on[name]], name+"\n", map[string]string{name + ".txt": name + "\n"})
		w.openBranch(t, name, nil)
	}
	if status, out := w.send(t, http.MethodPost, "/workspaces/demo/changes/6/abandon", w.ownerKey, nil); status != http.StatusOK {
		t.Fatalf("abandon demo#6 = %d %v, want 200", status, out)
	}
	for _, c := range []struct {
		asked int64
		want  []int64
	}{
		{1, []int64{1, 2, 3, 4, 5}},
		{4, []int64{1, 3, 4}},
	} {
		if got := stackNumbers(w.stackOf(t, c.asked, w.ownerKey)); !slices.Equal(got, c.want) {
			t.Errorf("stack of demo#%d = %v, want %v", c.asked, got, c.want)
		}
	}
}

func TestStackRoute_RefusesAChangeItCannotReadOrDoesNotHave(t *testing.T) {
	t.Parallel()
	m := newMiaWorld(t)
	fromDemo := m.stacked(t, "a")[0].Number
	fromFork := m.openAsMia(t, "task")
	readPAT, _ := m.patFor(t, []string{"workspaces:read"}, nil)
	outsiderKey := m.IssueKey(t, m.CreateUser(t, "outsider"))
	bound := m.IssuePAT(t, identity.PATSpec{UserID: m.mia, Permissions: []string{"changes:read"}, Workspace: "demo--t1", CreatedAt: time.Now().UTC()})
	for _, c := range []struct {
		name, number, credential string
		status                   int
		errorType                string
	}{
		{"a number with no change", "999", m.ownerKey, http.StatusNotFound, "change_not_found"},
		{"a PAT without changes:read", fromFork, readPAT, http.StatusForbidden, "insufficient_scope"},
		{"a user outside the organization", fromFork, outsiderKey, http.StatusNotFound, "not_found"},
		{"a fork-bound token on a change from elsewhere", strconv.FormatInt(fromDemo, 10), bound, http.StatusNotFound, "change_not_found"},
	} {
		status, out, _ := m.get(t, "/workspaces/demo/changes/"+c.number+"/stack", c.credential)
		if status != c.status || errorType(out) != c.errorType {
			t.Errorf("%s: GET stack = %d %v, want %d %s", c.name, status, out, c.status, c.errorType)
		}
	}
}
