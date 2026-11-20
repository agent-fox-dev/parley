// Package upgrade holds the rule the upgrade test applies to the data a
// release recorded before it starts the current hub on that data.
package upgrade

import "fmt"

// Compatible returns nil when the current binary can open data whose store
// recorded the steps recorded, given the steps registered for that store:
// every recorded step is still registered, and in the order the steps were
// recorded. Otherwise it names the first recorded step that breaks the rule,
// so that a renamed, removed or reordered step fails with its name rather
// than with what applying a step a second time would do.
func Compatible(recorded, registered []string) error {
	position := make(map[string]int, len(registered))
	for i, name := range registered {
		position[name] = i
	}
	last, lastName := -1, ""
	for _, name := range recorded {
		p, ok := position[name]
		switch {
		case !ok:
			return fmt.Errorf("recorded schema step %q is not registered", name)
		case p < last:
			return fmt.Errorf("recorded schema step %q is registered out of order: before %q, which was recorded first", name, lastName)
		}
		last, lastName = p, name
	}
	return nil
}
