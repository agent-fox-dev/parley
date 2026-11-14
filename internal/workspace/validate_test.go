package workspace_test

import (
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

// ruleCase is one row of a validator's table: the value and, when the
// validator refuses it, a part of the error's text that names the rule.
type ruleCase struct {
	value string
	rule  string // "" when the value is accepted
}

// checkRule runs a validator over its table and asserts the verdict per row.
func checkRule(t *testing.T, name string, validate func(string) error, cases []ruleCase) {
	t.Helper()
	for _, c := range cases {
		err := validate(c.value)
		switch {
		case c.rule == "" && err != nil:
			t.Errorf("%s(%q) = %v, want nil", name, c.value, err)
		case c.rule != "" && (err == nil || !strings.Contains(err.Error(), c.rule)):
			t.Errorf("%s(%q) = %v, want an error naming %q", name, c.value, err, c.rule)
		}
	}
}

func TestValidateSlug_AcceptsOnlyLowerCaseNamesOfThreeToSixtyFourCharacters(t *testing.T) {
	t.Parallel()
	checkRule(t, "ValidateSlug", workspace.ValidateSlug, []ruleCase{
		{"abc", ""},
		{"a1b", ""},
		{"my-repo-2", ""},
		{"a" + strings.Repeat("b", 63), ""},
		{"", "3 to 64 characters"},
		{"ab", "3 to 64 characters"},
		{"a" + strings.Repeat("b", 64), "3 to 64 characters"},
		{"1abc", "start with a letter"},
		{"-abc", "start with a letter"},
		{"abc-", "end with a letter or digit"},
		{"a--b", "two hyphens"},
		{"Abc", "a to z"},
		{"a_b", "a to z"},
		{"a.b", "a to z"},
		{"a b", "a to z"},
		{"abç", "a to z"},
	})
}

func TestValidateGitURL_AcceptsHTTPSAndGitAtFormsWithoutContactingTheHost(t *testing.T) {
	t.Parallel()
	// No host below resolves (.invalid is reserved), so a validator that
	// looked a host up would refuse the accepted rows or hang.
	checkRule(t, "ValidateGitURL", workspace.ValidateGitURL, []ruleCase{
		{"https://example.invalid/a/b.git", ""},
		{"https://example.invalid:8443/a", ""},
		{"https://[::1]:8443/a", ""},
		{"git@example.invalid:a/b.git", ""},
		{"", "https://"},
		{"http://example.invalid/a", "https://"},
		{"ssh://git@example.invalid/a", "https://"},
		{"file:///tmp/a", "https://"},
		{"/tmp/a", "https://"},
		{"other@example.invalid:a", "https://"},
		{"https://example.invalid", "path"},
		{"https://example.invalid/", "path"},
		{"https:///a", "host"},
		{"https://example.invalid:/a", "port"},
		{"https://example.invalid:x/a", "port"},
		{"https://user@example.invalid/a", "user"},
		{"https://user:pw@example.invalid/a", "user"},
		{"https://example.invalid/a b", "space or control"},
		{"git@example.invalid:", "path"},
		{"git@:a", "host"},
	})
}

func TestValidateBranchName_AppliesEveryClauseOfTheBranchNameRule(t *testing.T) {
	t.Parallel()
	checkRule(t, "ValidateBranchName", workspace.ValidateBranchName, []ruleCase{
		{"main", ""},
		{"feature/x", ""},
		{"fix-42", ""},
		{"a.b", ""},
		{"a@b", ""},
		{"release/1.2", ""},
		{"", "empty"},
		{"@", "'@'"},
		{"a b", "space"},
		{"a~b", "~"},
		{"a^b", "~"},
		{"a:b", "~"},
		{"a?b", "~"},
		{"a*b", "~"},
		{"a[b", "~"},
		{`a\b`, "~"},
		{"a..b", "'..'"},
		{"a//b", "'..'"},
		{"a@{b", "'..'"},
		{"-a", "start with"},
		{"/a", "start with"},
		{"a/", "end with"},
		{"a.", "end with"},
		{"a.lock", "end with"},
		{".a", "component"},
		{"a/.b", "component"},
		{"a.lock/b", "component"},
		{"a\tb", "control character"},
		{"a\x7fb", "control character"},
	})
}

func TestValidateForkName_AcceptsOnlyLowerCaseNamesOfOneToThirtyTwoCharacters(t *testing.T) {
	t.Parallel()
	checkRule(t, "ValidateForkName", workspace.ValidateForkName, []ruleCase{
		{"a", ""},
		{"7", ""},
		{"a-b", ""},
		{"1x", ""},
		{strings.Repeat("a", 32), ""},
		{"", "1 to 32 characters"},
		{strings.Repeat("a", 33), "1 to 32 characters"},
		{"-a", "start with a letter or digit"},
		{"a-", "end with a letter or digit"},
		{"a--b", "two hyphens"},
		{"A", "a to z"},
		{"a_b", "a to z"},
		{"é", "a to z"},
	})
}

func TestParseDuration_ReadsAWholeNumberOfMinutesHoursOrDays(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		value string
		want  time.Duration
		rule  string // a part of the error's text; "" when the value parses
	}{
		{"90m", 90 * time.Minute, ""},
		{"24h", 24 * time.Hour, ""},
		{"7d", 168 * time.Hour, ""},
		{"1m", time.Minute, ""},
		{"", 0, "whole number"},
		{"0m", 0, "above zero"},
		{"0d", 0, "above zero"},
		{"-1h", 0, "whole number"},
		{"+1h", 0, "whole number"},
		{"1.5h", 0, "whole number"},
		{"10", 0, "m, h or d"},
		{"10s", 0, "m, h or d"},
		{"1h30m", 0, "whole number"},
		{"h", 0, "whole number"},
		{" 5m", 0, "whole number"},
		{"5M", 0, "m, h or d"},
		{"99999999999999999999m", 0, "too long"},
		{"9999999999999999d", 0, "too long"},
	} {
		got, err := workspace.ParseDuration(c.value)
		switch {
		case c.rule == "" && (err != nil || got != c.want):
			t.Errorf("ParseDuration(%q) = %v, %v; want %v, nil", c.value, got, err, c.want)
		case c.rule != "" && (err == nil || !strings.Contains(err.Error(), c.rule)):
			t.Errorf("ParseDuration(%q) = %v, %v; want an error naming %q", c.value, got, err, c.rule)
		}
	}
}
