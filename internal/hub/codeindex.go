package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/codeindex"
)

// codeIndexHook is the start hook that binds the code index component and,
// on a hub with [code_index], refuses a platform the index cannot run on.
const codeIndexHook = "code-index"

// composeCodeIndex composes the code index: the trees and indexes of the
// workspaces' default branches (package codeindex). A hub whose
// configuration has no [code_index] gets the component and nothing it does.
//
// The component exists from the composition on (CodeIndex); tests change
// its dependencies with AdjustCodeIndex.
func composeCodeIndex(c *Composition) {
	comp := &codeindex.Component{}
	c.codeIndex = comp
	c.AddStartHook(codeIndexHook, func(_ context.Context, rt *Runtime) error {
		d := codeindex.Deps{Settings: rt.Config.CodeIndex, DataDir: rt.Config.DataDir()}
		for _, adjust := range c.codeIndexAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return comp.CheckPlatform()
	})
}

// CodeIndex returns the hub's code index component, which works once its
// start hook has bound it.
func (c *Composition) CodeIndex() *codeindex.Component { return c.codeIndex }

// AdjustCodeIndex adds fn to the functions that change the code index
// component's dependencies after the start hook has built them and before
// it binds them, in the order they were added.
func (c *Composition) AdjustCodeIndex(fn func(*codeindex.Deps)) {
	c.codeIndexAdjusters = append(c.codeIndexAdjusters, fn)
}
