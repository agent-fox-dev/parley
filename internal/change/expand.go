package change

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/server"
)

// Expander computes the member of the change object that a read's include
// names, from the change as it is read; its error fails the read.
type Expander func(ctx context.Context, ch Change) (any, error)

// includeVersions is the include name this component serves itself: the
// change's versions, oldest first.
const includeVersions = "versions"

// includeNames are the names include accepts, in the order their members
// follow the change's own. Every name but versions is served by the
// expander another feature registers.
var includeNames = []string{includeVersions, "approvals", "checks", "stack", "overlaps"}

// RegisterExpander makes fn the expander of the include name, replacing the
// one before; nil removes it. Like every collaborator it is registered on
// the instance while the hub is composed
// (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md). It
// panics for a name include does not accept, or for versions, which the
// component serves itself: both are programming errors.
func (c *Component) RegisterExpander(name string, fn Expander) {
	if name == includeVersions || !slices.Contains(includeNames, name) {
		panic(fmt.Sprintf("change.RegisterExpander: %q is not an include name an expander serves", name))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if fn == nil {
		delete(c.expanders, name)
		return
	}
	if c.expanders == nil {
		c.expanders = map[string]Expander{}
	}
	c.expanders[name] = fn
}

// parseInclude reads the names the include parameters of a read list, each
// a comma list, in the order of includeNames; a name outside them, the
// empty name among them, is 400 invalid_request.
func parseInclude(values []string) ([]string, error) {
	asked := map[string]bool{}
	for _, v := range values {
		for name := range strings.SplitSeq(v, ",") {
			if !slices.Contains(includeNames, name) {
				return nil, server.InvalidRequest(fmt.Sprintf("include takes a comma list of %s; %q is none of them",
					strings.Join(includeNames, ", "), name))
			}
			asked[name] = true
		}
	}
	var out []string
	for _, name := range includeNames {
		if asked[name] {
			out = append(out, name)
		}
	}
	return out, nil
}

// expand computes the members the include names add to the change ch,
// read through q: versions from the change's versions, every other name
// from its expander, and no member for a name without one.
func (c *Component) expand(ctx context.Context, q Querier, ch Change, names []string) ([]server.Member, error) {
	c.mu.RLock()
	expanders := maps.Clone(c.expanders)
	c.mu.RUnlock()
	var out []server.Member
	for _, name := range names {
		if name == includeVersions {
			vs, err := readVersions(ctx, q, ch.WorkspaceSlug, ch.Number)
			if err != nil {
				return nil, err
			}
			out = append(out, server.Member{Name: name, Value: vs})
			continue
		}
		fn := expanders[name]
		if fn == nil {
			continue
		}
		v, err := fn(ctx, ch)
		if err != nil {
			return nil, fmt.Errorf("include %s of %s: %w", name, ch.Ref, err)
		}
		out = append(out, server.Member{Name: name, Value: v})
	}
	return out, nil
}
