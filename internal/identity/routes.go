package identity

import (
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// Routes are this package's public routes over the configured providers.
type Routes struct {
	Providers []Provider
}

// Register mounts the routes on the server.
func (rs *Routes) Register(srv *server.Server) {
	srv.Register(server.Route{Method: "GET", Pattern: "/auth/providers", Public: true,
		CacheControl: "public, max-age=300", Handler: rs.listProviders})
}

// providerItem is a provider as GET /auth/providers lists it; it never
// carries the client secret.
type providerItem struct {
	Name         string `json:"name"`
	AuthorizeURL string `json:"authorize_url"`
}

func (rs *Routes) listProviders(w http.ResponseWriter, _ *http.Request) error {
	items := make([]providerItem, 0, len(rs.Providers))
	for _, p := range rs.Providers {
		items = append(items, providerItem{Name: p.Name, AuthorizeURL: p.AuthorizeURL()})
	}
	server.WritePage(w, server.Page{Limit: len(items)}, items, func(it providerItem) string { return it.Name })
	return nil
}
