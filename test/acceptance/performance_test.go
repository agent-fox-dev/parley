package acceptance

import (
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

func TestPerformance_SingleClientTargetsOfTheReferenceHost(t *testing.T) {
	harness.Require(t, harness.ReferenceHost)
}
