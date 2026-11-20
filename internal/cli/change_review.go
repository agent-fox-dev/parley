package cli

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// approvalsPath is the route of the verdicts of a change.
const approvalsPath = changePath + "/approvals"

// checkStatuses are the statuses parley change check --status takes, as
// the hub reads them.
var checkStatuses = []string{"pending", "passed", "failed"}

// versionUsage is the help of --version of a command that gives or reads
// evidence on one version of a change.
const versionUsage = "the version of the change, a positive integer (default: its current version)"

// newChangeVerdictCommand builds parley change approve or reject: a
// verdict on a version of a change. A rejection needs a comment that says
// why, so reject requires a --comment that is not empty.
func newChangeVerdictCommand(verdict, description string) *cobra.Command {
	reject := verdict == "reject"
	cmd := declare(command{
		Name:        verdict,
		Description: description,
		Method:      http.MethodPost,
		Path:        approvalsPath,
		Args:        []argSpec{slugArg, numberArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("version", "", versionUsage)
			fs.String("comment", "", "a comment on the verdict")
			if reject {
				// It fails only for a flag the set does not hold, and the
				// line above adds it.
				_ = cobra.MarkFlagRequired(fs, "comment")
			}
		},
		Run: func(c *call) error {
			fs := c.cmd.Flags()
			body := map[string]any{"verdict": verdict}
			addVersion(fs, body)
			if fs.Changed("comment") {
				body["comment"], _ = fs.GetString("comment")
			}
			return sendWith(c, nil, body)
		},
	})
	return checkFlags(withNumbers(cmd, true), func(cmd *cobra.Command) error {
		if err := checkVersion(cmd); err != nil {
			return err
		}
		if comment, _ := cmd.Flags().GetString("comment"); reject && comment == "" {
			return usageError("%s takes a --comment that says why, not an empty one", cmd.CommandPath())
		}
		return nil
	})
}

// newChangeApprovalsCommand builds parley change approvals: the verdicts
// on a version of a change, or with --withdraw the withdrawal of one.
func newChangeApprovalsCommand() *cobra.Command {
	cmd := declare(command{
		Name:        "approvals",
		Description: "List the verdicts on a version of a change, newest first, or withdraw one",
		Method:      http.MethodGet,
		Path:        approvalsPath,
		Args:        []argSpec{slugArg, numberArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("version", "", versionUsage)
			fs.String("withdraw", "", "withdraw the verdict with this id instead of listing")
			markWrites(fs, "withdraw")
		},
		Run: func(c *call) error {
			fs := c.cmd.Flags()
			if !fs.Changed("withdraw") {
				return sendWith(c, versionQuery(fs), nil)
			}
			// The withdrawal is the list route's child; the declaration
			// names the list route, which the JSON help shows.
			id, _ := fs.GetString("withdraw")
			resp, err := c.client.request(c.ctx, http.MethodDelete, routePath(approvalsPath, c.args)+"/"+pathSegment(id), nil, nil)
			if err != nil {
				return err
			}
			return c.printAnswer(resp)
		},
	})
	return checkFlags(withNumbers(cmd, true), func(cmd *cobra.Command) error {
		if err := checkVersion(cmd); err != nil {
			return err
		}
		fs := cmd.Flags()
		if !fs.Changed("withdraw") {
			return nil
		}
		// A withdrawal names one verdict, which is on one version already.
		if fs.Changed("version") {
			return usageError("%s takes --withdraw or --version, not both", cmd.CommandPath())
		}
		if id, _ := fs.GetString("withdraw"); id == "" {
			return usageError("%s takes --withdraw <id>, not an empty one", cmd.CommandPath())
		}
		return nil
	})
}

// newChangeCheckCommand builds parley change check: the result of a named
// check on a version of a change, as a check runner reports it.
func newChangeCheckCommand() *cobra.Command {
	cmd := declare(command{
		Name:        "check",
		Description: "Report the result of a named check on a version of a change",
		Method:      http.MethodPut,
		Path:        changePath + "/checks/:name",
		Args:        []argSpec{slugArg, numberArg, {Name: "name", Required: true}},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("status", "", "the check's status: pending, passed or failed")
			// It fails only for a flag the set does not hold, and the line
			// above adds it.
			_ = cobra.MarkFlagRequired(fs, "status")
			fs.String("version", "", versionUsage)
			fs.String("summary", "", "a short summary of the result")
			fs.String("url", "", "an http or https URL of the check run")
		},
		Run: func(c *call) error {
			fs := c.cmd.Flags()
			body := map[string]any{}
			body["status"], _ = fs.GetString("status")
			addVersion(fs, body)
			for _, flag := range []string{"summary", "url"} {
				if fs.Changed(flag) {
					body[flag], _ = fs.GetString(flag)
				}
			}
			return sendWith(c, nil, body)
		},
	})
	return checkFlags(withNumbers(cmd, true), func(cmd *cobra.Command) error {
		if err := checkVersion(cmd); err != nil {
			return err
		}
		if status, _ := cmd.Flags().GetString("status"); !slices.Contains(checkStatuses, status) {
			return usageError("%s takes --status pending, passed or failed, got %q", cmd.CommandPath(), status)
		}
		return nil
	})
}

// newChangeChecksCommand builds parley change checks: the check results
// that apply to a version of a change.
func newChangeChecksCommand() *cobra.Command {
	cmd := declare(command{
		Name:        "checks",
		Description: "List the check results that apply to a version of a change, carried-over ones included",
		Method:      http.MethodGet,
		Path:        changePath + "/checks",
		Args:        []argSpec{slugArg, numberArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("version", "", versionUsage)
		},
		Run: func(c *call) error {
			return sendWith(c, versionQuery(c.cmd.Flags()), nil)
		},
	})
	return checkFlags(withNumbers(cmd, true), checkVersion)
}

// checkVersion refuses a --version that is not a positive integer. Unlike
// withNumbers it refuses latest too, because the evidence routes read a
// version number alone.
func checkVersion(cmd *cobra.Command) error {
	fs := cmd.Flags()
	if !fs.Changed("version") {
		return nil
	}
	if value, _ := fs.GetString("version"); !isPositiveNumber(value) {
		return usageError("%s takes --version <n>, a positive integer, got %q", cmd.CommandPath(), value)
	}
	return nil
}

// addVersion adds the body's version, as a JSON integer, when --version is
// given; checkVersion accepted it, so it parses.
func addVersion(fs *pflag.FlagSet, body map[string]any) {
	if fs.Changed("version") {
		value, _ := fs.GetString("version")
		body["version"], _ = strconv.ParseInt(value, 10, 64)
	}
}

// versionQuery is the query of a read of the evidence: version when
// --version is given, else none, so that the hub reads the current version.
func versionQuery(fs *pflag.FlagSet) url.Values {
	if !fs.Changed("version") {
		return nil
	}
	value, _ := fs.GetString("version")
	return url.Values{"version": {value}}
}
