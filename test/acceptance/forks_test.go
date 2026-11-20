package acceptance

import (
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

func TestLong_ForkDisappearsAtItsTTL(t *testing.T) {
	harness.Require(t, harness.Long)
}

func TestLargeRepo_ForkCreationDoesNotGrowWithSize(t *testing.T) {
	harness.Require(t, harness.LargeRepo)
}
