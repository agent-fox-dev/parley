package harness

import (
	"os"
	"testing"
)

// Gate is something a scenario needs beyond git and the Go toolchain, which
// the person running the suite says is there by setting Var.
type Gate struct {
	// Need is what the scenario needs, as the skip reason words it.
	Need string
	Var  string
}

// The gates of the suite.
var (
	Gateway       = Gate{Need: "an OpenShell gateway", Var: "PARLEY_ACCEPTANCE_OPENSHELL_GATEWAY_URL"}
	Long          = Gate{Need: "a wait of more than two minutes", Var: "PARLEY_ACCEPTANCE_LONG"}
	LargeRepo     = Gate{Need: "a repository of 1 GiB", Var: "PARLEY_ACCEPTANCE_LARGE_REPO"}
	ReferenceHost = Gate{Need: "the reference host (8 cores, 32 GB, local SSD)", Var: "PARLEY_ACCEPTANCE_REFERENCE_HOST"}
)

// Require skips tb, naming the gate's variable, unless the variable is set,
// and returns its value. A scenario calls it before it starts anything. A
// set variable never skips, so a gateway that is set but broken fails the
// scenario rather than hiding it.
func Require(tb testing.TB, g Gate) string {
	tb.Helper()
	v := os.Getenv(g.Var)
	if v == "" {
		tb.Skipf("needs %s: set %s to run it", g.Need, g.Var)
	}
	return v
}
