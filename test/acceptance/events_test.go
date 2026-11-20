package acceptance

import (
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

func TestLong_CursorOlderThanRetentionIsExpired(t *testing.T) {
	harness.Require(t, harness.Long)
}
