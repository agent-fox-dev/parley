package acceptance

import (
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

func TestSandbox_ForkWithSandboxChecksOutAndPushOpensAChange(t *testing.T) {
	gateway := harness.Require(t, harness.Gateway)
	t.Parallel()
	harness.Start(t, harness.Options{
		TOML: "[sandbox]\ngateway_url = \"" + gateway + "\"\ntoken_file = \"gateway-token\"\n" +
			"[[sandbox.profiles]]\nname = \"agent\"\nttl = \"1h\"\n",
		Files: map[string]string{"gateway-token": "acceptance"},
	})
}
