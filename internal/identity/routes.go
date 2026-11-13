package identity

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/server"
)

// Routes returns the routes of the identity service, for the hub to
// register. Its admin routes need AdminCheck installed as an access check.
func (s *Service) Routes() []server.Route {
	routes := []server.Route{
		{Method: http.MethodGet, Pattern: "/auth/providers", Public: true, CacheControl: "public, max-age=300",
			Handler: s.listProviders},
		// Not a create route: a login always issues a new key, so an
		// Idempotency-Key is ignored rather than replayed.
		{Method: http.MethodPost, Pattern: "/auth/callback", Public: true, Handler: s.callback},
	}
	routes = append(routes, s.userRoutes()...)
	routes = append(routes, s.ownUserRoutes()...)
	routes = append(routes, s.keyRoutes()...)
	routes = append(routes, s.tokenRoutes()...)
	return append(routes, s.orgRoutes()...)
}

// providerItem is one entry of GET /auth/providers.
type providerItem struct {
	Name         string `json:"name"`
	AuthorizeURL string `json:"authorize_url"`
}

// listProviders answers GET /auth/providers: the configured identity
// providers ordered by name, each with the URL a client sends the browser
// to. The route needs no credential, so an item holds the name and that URL
// and nothing else of the provider's configuration.
func (s *Service) listProviders(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var after string
	hasAfter, err := page.After(&after)
	if err != nil {
		return err
	}
	providers := slices.SortedFunc(slices.Values(s.deps.Providers), func(a, b config.Provider) int {
		return cmp.Compare(a.Name, b.Name)
	})
	items := make([]providerItem, 0, len(providers))
	for _, p := range providers {
		if hasAfter && p.Name <= after {
			continue
		}
		u, err := authorizeURL(p)
		if err != nil {
			return fmt.Errorf("provider %s: authorize_url: %w", p.Name, err)
		}
		items = append(items, providerItem{Name: p.Name, AuthorizeURL: u})
	}
	return server.WritePage(w, page, items, func(i providerItem) any { return i.Name })
}
