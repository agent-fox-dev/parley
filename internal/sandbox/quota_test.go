package sandbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// limits are the four sandbox limits of a hub, in the order they are
// checked.
type limits struct{ sandboxes, perOrg, perWorkspace, perUser int }

// only is limits with the limit name at n and the others at 100.
func only(name string, n int) limits {
	l := limits{100, 100, 100, 100}
	l.set(name, n)
	return l
}

// set sets the limit name to n.
func (l *limits) set(name string, n int) {
	switch name {
	case "max_sandboxes":
		l.sandboxes = n
	case "max_per_org":
		l.perOrg = n
	case "max_per_workspace":
		l.perWorkspace = n
	case "max_per_user":
		l.perUser = n
	}
}

// setLimits makes l the limits of the hub's next creates: the limits are
// the loaded settings, which a create reads each time.
func (h *createHub) setLimits(l limits) {
	s := h.svc.Settings()
	s.MaxSandboxes, s.MaxPerOrg, s.MaxPerWorkspace, s.MaxPerUser = l.sandboxes, l.perOrg, l.perWorkspace, l.perUser
}

// quotaHub is a create hub with a create world, the user omar, who creates
// no sandbox through the hub, and the org other.
type quotaHub struct {
	*createHub
	omar, other string
}

func startQuotaHub(t *testing.T) *quotaHub {
	t.Helper()
	h := &quotaHub{createHub: startCreateHub(t, agentProfile)}
	h.world(t)
	h.omar, h.other = h.CreateUser(t, "omar"), h.CreateOrg(t, "other")
	return h
}

// stored describes a sandbox a quota test stores directly: its workspace,
// the root of the workspace's family, the org (none for ""), the user who
// created it and its state.
type stored struct{ ws, family, org, user, state string }

// store stores s and returns its id.
func (h *createHub) store(t *testing.T, s stored) string {
	t.Helper()
	rec := sandbox.Record{
		ID: sandbox.NewID(), Workspace: s.ws, Profile: "agent", State: s.state,
		ExpiresAt: recordStart.Add(time.Hour), LastActivityAt: recordStart,
		CreatedBy: sandbox.Creator{ActorType: "api_key", ActorID: s.user, Username: "someone"},
		CreatedAt: recordStart, UpdatedAt: recordStart, UserID: s.user, OrgID: s.org, FamilyRoot: s.family,
	}
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return sandbox.Insert(ctx, tx, rec) })
	return rec.ID
}

// exec runs the statement query with args in a store transaction.
func (h *createHub) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, query, args...)
		return err
	})
}

// capacity is the error object of a sandbox_capacity answer.
type capacity struct {
	ErrorType string `json:"error_type"`
	Message   string `json:"message"`
	Limit     string `json:"limit"`
	Maximum   int    `json:"maximum"`
	Current   int    `json:"current"`
}

// wantCapacity fails the test unless a is 429 sandbox_capacity naming the
// limit at maximum with the count current.
func wantCapacity(t *testing.T, a answer, limit string, maximum, current int) {
	t.Helper()
	var env struct{ Error capacity }
	_ = json.Unmarshal(a.body, &env)
	want := capacity{"sandbox_capacity", "sandbox limit reached: " + limit, limit, maximum, current}
	if a.status != http.StatusTooManyRequests || env.Error != want {
		t.Errorf("create = %d %s; want 429 with %+v", a.status, a.body, want)
	}
}

func TestQuota_RefusesAtEachLimit(t *testing.T) {
	t.Parallel()
	h := startQuotaHub(t)
	acme := h.org
	rows := []struct {
		limit string
		seeds [2]stored
	}{
		{"max_sandboxes", [2]stored{
			{"far", "far", h.other, h.omar, sandbox.StateReady}, {"far--x", "far", h.other, h.omar, sandbox.StateReady},
		}},
		{"max_per_org", [2]stored{
			{"lib", "lib", acme, h.omar, sandbox.StateProvisioning}, {"tool", "tool", acme, h.omar, sandbox.StateProvisioning},
		}},
		{"max_per_workspace", [2]stored{
			{"demo", "demo", acme, h.omar, sandbox.StateReady}, {"demo--t2", "demo", acme, h.omar, sandbox.StateReady},
		}},
		{"max_per_user", [2]stored{
			{"far", "far", h.other, h.dana, sandbox.StateReady}, {"near", "near", h.other, h.dana, sandbox.StateReady},
		}},
	}
	for _, row := range rows {
		t.Run(row.limit, func(t *testing.T) {
			h.exec(t, `DELETE FROM sandboxes`)
			h.setLimits(only(row.limit, 2))
			first := h.store(t, row.seeds[0])
			h.store(t, row.seeds[1])
			before := h.footprint(t)
			wantCapacity(t, h.post(t, "demo--t1", map[string]any{}), row.limit, 2, 2)
			if after := h.footprint(t); after != before {
				t.Errorf("a refused create left %+v; want %+v", after, before)
			}
			h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return sandbox.Remove(ctx, tx, first) })
			sandboxObject(t, h.post(t, "demo--t1", map[string]any{}))
		})
	}
}

func TestQuota_ReportsFirstExceeded(t *testing.T) {
	t.Parallel()
	h := startQuotaHub(t)
	for range 2 {
		h.store(t, stored{"demo", "demo", h.org, h.dana, sandbox.StateReady})
	}
	// Two sandboxes are counted by every limit. The earlier limit of each
	// row is 2 and the later ones 1, so that the later ones are exceeded
	// too and an answer naming one of them has another maximum.
	rows := []struct {
		name   string
		exceed []string
	}{
		{"sandboxes and org", []string{"max_sandboxes", "max_per_org"}},
		{"org and workspace", []string{"max_per_org", "max_per_workspace"}},
		{"workspace and user", []string{"max_per_workspace", "max_per_user"}},
		{"all four", []string{"max_sandboxes", "max_per_org", "max_per_workspace", "max_per_user"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			l := only(row.exceed[0], 2)
			for _, later := range row.exceed[1:] {
				l.set(later, 1)
			}
			h.setLimits(l)
			wantCapacity(t, h.post(t, "demo--t1", map[string]any{}), row.exceed[0], 2, 2)
		})
	}
}

func TestQuota_CountingRules(t *testing.T) {
	t.Parallel()
	h := startQuotaHub(t)
	created := func(t *testing.T, slug string) string {
		t.Helper()
		id, _ := sandboxObject(t, h.post(t, slug, map[string]any{}))["id"].(string)
		return id
	}
	remove := func(t *testing.T, id string) {
		t.Helper()
		h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return sandbox.Remove(ctx, tx, id) })
	}

	// The parent and a sibling fork count toward the fork's family, until
	// one of their sandboxes has ended.
	h.setLimits(only("max_per_workspace", 2))
	created(t, "demo")
	sibling := created(t, "demo--t2")
	wantCapacity(t, h.post(t, "demo--t1", map[string]any{}), "max_per_workspace", 2, 2)
	for _, state := range []string{sandbox.StateStopped, sandbox.StateCompleted, sandbox.StateFailed, sandbox.StateDeleted} {
		t.Run("sibling "+state, func(t *testing.T) {
			h.exec(t, `UPDATE sandboxes SET state = ? WHERE id = ?`, state, sibling)
			remove(t, created(t, "demo--t1"))
		})
	}

	// Only the creating user's own sandboxes count toward max_per_user,
	// whatever workspace they are on.
	t.Run("per user", func(t *testing.T) {
		h.exec(t, `DELETE FROM sandboxes`)
		h.setLimits(only("max_per_user", 1))
		h.store(t, stored{"demo--t1", "demo", h.org, h.omar, sandbox.StateReady})
		created(t, "demo--t1")
		wantCapacity(t, h.post(t, "demo", map[string]any{}), "max_per_user", 1, 1)
	})

	// A fork of another workspace of the org counts toward the org.
	t.Run("per org", func(t *testing.T) {
		h.exec(t, `DELETE FROM sandboxes`)
		h.setLimits(only("max_per_org", 1))
		h.store(t, stored{"lib--f1", "lib", h.org, h.omar, sandbox.StateProvisioning})
		wantCapacity(t, h.post(t, "demo", map[string]any{}), "max_per_org", 1, 1)
	})

	// A workspace in no org is not held to max_per_org, not even by the
	// sandboxes of other workspaces in no org.
	t.Run("no org", func(t *testing.T) {
		h.exec(t, `DELETE FROM sandboxes`)
		h.setLimits(only("max_per_org", 1))
		h.repository(t, "solo", workspace.StatusReady)
		h.exec(t, `UPDATE workspaces SET org_id = '' WHERE slug = 'solo'`)
		h.store(t, stored{"alone", "alone", "", h.omar, sandbox.StateReady})
		created(t, "solo")
	})
}

func TestQuota_ConcurrentCreates(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, "max_per_workspace = 5\n"+agentProfile)
	h.world(t)
	before := h.footprint(t)
	const n = 12
	type result struct {
		status int
		err    capacity
	}
	results := make([]result, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			req, err := http.NewRequest(http.MethodPost, h.URL("/api/v1/workspaces/demo/sandboxes"), strings.NewReader("{}"))
			if err != nil {
				results[i].status = -1
				return
			}
			req.Header.Set("Authorization", "Bearer "+h.key)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results[i].status = -1
				return
			}
			defer func() { _ = resp.Body.Close() }()
			var env struct{ Error capacity }
			_ = json.NewDecoder(resp.Body).Decode(&env)
			results[i] = result{resp.StatusCode, env.Error}
		})
	}
	close(start)
	wg.Wait()
	counts := map[int]int{}
	for _, r := range results {
		counts[r.status]++
		if r.status == http.StatusTooManyRequests && (r.err.Limit != "max_per_workspace" || r.err.Maximum != 5) {
			t.Errorf("a refused create answered %+v; want limit max_per_workspace, maximum 5", r.err)
		}
	}
	if counts[http.StatusCreated] != 5 || counts[http.StatusTooManyRequests] != 7 {
		t.Errorf("statuses %v; want 5 × 201 and 7 × 429", counts)
	}
	recs, err := sandbox.ListByState(context.Background(), h.Store.Main(), sandbox.StateProvisioning)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 5 {
		t.Errorf("the store holds %d sandboxes in provisioning; want 5", len(recs))
	}
	for _, r := range recs {
		if r.Workspace != "demo" {
			t.Errorf("sandbox %s is of %s; want demo", r.ID, r.Workspace)
		}
	}
	// A refused create mints no token: at most one per sandbox stored.
	if after := h.footprint(t); after.tokens-before.tokens > 5 {
		t.Errorf("the creates added %d tokens; want at most 5", after.tokens-before.tokens)
	}
}
