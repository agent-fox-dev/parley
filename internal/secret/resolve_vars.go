package secret

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// ResolvedVar is a variable a workspace runs with: the whole entry of the
// first step of its chain that holds the key, and that step's origin.
type ResolvedVar struct {
	Variable
	Origin Origin
}

// varReader reads every variable of one scope through q, in the order of
// their keys with A to Z lowered.
type varReader func(ctx context.Context, q rowsQuerier, scope Scope, scopeID string) ([]Variable, error)

// scopeVars is the store's varReader.
func scopeVars(ctx context.Context, q rowsQuerier, scope Scope, scopeID string) ([]Variable, error) {
	return readVarPage(ctx, q, scope, scopeID, "", -1)
}

// ResolveVars returns the variables the workspace slug runs with, in the
// order of their keys with A to Z lowered: of the variables of the steps of
// its Chain, for each key compared ignoring ASCII case, the whole entry of
// the first step that holds it, with that step's origin. The steps are read
// in one read transaction, so that the answer is one state of the store;
// a step that cannot be read fails the whole resolution, and nothing of it
// is returned. A slug with no record is ErrWorkspaceNotFound.
func (c *Component) ResolveVars(ctx context.Context, slug string) ([]ResolvedVar, error) {
	steps, err := c.Chain(ctx, slug)
	if err != nil {
		return nil, err
	}
	if c.deps.Store == nil {
		return nil, errNotBound
	}
	read := scopeVars
	if r := c.readVars.Load(); r != nil {
		read = *r
	}
	tx, err := c.deps.Store.Main().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("resolve the variables of %s: %w", slug, err)
	}
	defer func() { _ = tx.Rollback() }()
	seen := make(map[string]bool)
	merged := []ResolvedVar{}
	for _, s := range steps {
		vars, err := read(ctx, tx, s.Scope, s.ScopeID)
		if err != nil {
			return nil, fmt.Errorf("resolve the variables of %s, %s scope: %w", slug, s.Origin, err)
		}
		for _, v := range vars {
			if k := v.Position(); !seen[k] {
				seen[k] = true
				merged = append(merged, ResolvedVar{Variable: v, Origin: s.Origin})
			}
		}
	}
	slices.SortFunc(merged, func(a, b ResolvedVar) int { return strings.Compare(a.Position(), b.Position()) })
	return merged, nil
}

// resolvedItem is a resolved variable as the route answers it.
type resolvedItem struct {
	Key       string       `json:"key"`
	Value     string       `json:"value"`
	Origin    Origin       `json:"origin"`
	CreatedAt timefmt.Time `json:"created_at"`
	UpdatedAt timefmt.Time `json:"updated_at"`
}

// resolvedVars answers GET /workspaces/:slug/vars/resolved with a page of
// the variables the workspace runs with, merged along its chain on every
// request. Who asks plays no part beyond reading the workspace, which the
// access check has decided: the chain is the workspace's. A workspace whose
// record went after the check is not found; a store that fails is answered
// 500 with none of the list.
func (c *Component) resolvedVars(w http.ResponseWriter, r *http.Request) error {
	t, err := targetOf(r)
	if err != nil {
		return err
	}
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var after string
	if _, err := page.After(&after); err != nil {
		return err
	}
	merged, err := c.ResolveVars(r.Context(), t.id)
	if errors.Is(err, ErrWorkspaceNotFound) {
		return server.NotFound(ErrWorkspaceNotFound.Error())
	}
	if err != nil {
		return err
	}
	items := []resolvedItem{}
	for _, v := range merged {
		if len(items) > page.Limit {
			break
		}
		if v.Position() > after {
			items = append(items, resolvedItem{Key: v.Key, Value: v.Value, Origin: v.Origin,
				CreatedAt: timefmt.Time(v.CreatedAt), UpdatedAt: timefmt.Time(v.UpdatedAt)})
		}
	}
	return server.WritePage(w, page, items, func(it resolvedItem) any { return asciiLower(it.Key) })
}
