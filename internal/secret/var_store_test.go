package secret_test

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/secret"
)

func TestCreateVars_StoresAKeyOnceAgainstAConcurrentCreate(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	bodies := []map[string]any{keyed("k", "a1"), keyed("K", "a2")}
	// What the scope holds when the request of the same index wins.
	won := [][]string{{"a1=", "k="}, {"a2=", "K="}}
	for round := range 20 {
		slug := fmt.Sprintf("race%d", round)
		w.AddWorkspace(t, access.Record{Slug: slug, OwnerID: w.ownerID, OrgID: w.orgID,
			Kind: access.KindRepository, Status: access.StatusReady})
		path := "/workspaces/" + slug + "/vars"
		var answers [2]answer
		var errs [2]error
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, body := range bodies {
			wg.Go(func() {
				<-start
				answers[i], errs[i] = w.post(w.ownerKey, path, body)
			})
		}
		close(start)
		wg.Wait()
		if err := errors.Join(errs[:]...); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		winner := slices.IndexFunc(answers[:], func(a answer) bool { return a.status == http.StatusCreated })
		if winner < 0 {
			t.Errorf("round %d: answers %d %s and %d %s, want one 201", round, answers[0].status, answers[0].body, answers[1].status, answers[1].body)
			continue
		}
		loser := answers[1-winner]
		if typ, _ := errorOf(t, loser); loser.status != http.StatusConflict || typ != "conflict" {
			t.Errorf("round %d: the other request = %d %s, want 409 conflict", round, loser.status, loser.body)
		}
		if got := w.storedVars(t, secret.ScopeWorkspace, slug); !slices.Equal(got, won[winner]) {
			t.Errorf("round %d: the scope holds %v, want %v", round, got, won[winner])
		}
	}
}
