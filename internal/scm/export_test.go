package scm

import (
	"net/http"
	"sync/atomic"

	"github.com/agent-fox-dev/parley/internal/server"
)

// WriteProbeRoute is the write route POST /workspaces/:slug/scm/probe,
// reported under the op probe, whose handler counts its runs in ran and
// answers 200 with what the write gate handed it: the workspace's slug, the
// caller's access level, the credential's type and whether it is bound.
func WriteProbeRoute(c *Component, ran *atomic.Int64) server.Route {
	return c.writeRoute(http.MethodPost, "/probe", "probe", func(w http.ResponseWriter, _ *http.Request, wr *write) error {
		ran.Add(1)
		server.WriteJSON(w, http.StatusOK, map[string]any{
			"slug": wr.ws.Slug, "level": string(wr.decision.Level), "credential": wr.principal.Type, "bound": wr.bound,
		})
		return nil
	})
}

// SetRebaseHook makes hook run in every rebase once its commits are
// replayed and before its branch moves, as another writer would run
// meanwhile; nil removes it.
func SetRebaseHook(c *Component, hook func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replayed = hook
}

// ReplaceLines is src with its lines start to end, 1-based and inclusive,
// replaced by content as a replace_declaration replaces them; ok is false
// when they are not lines of src.
func ReplaceLines(src string, start, end int, content string) (string, bool) {
	from, to, ok := lineSpan([]byte(src), start, end)
	if !ok {
		return "", false
	}
	return string(spliceLines([]byte(src), from, to, []byte(content))), true
}
