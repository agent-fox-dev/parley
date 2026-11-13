package cli

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// tokenIDArg is the <token_id> argument: the id of one of the caller's
// personal access tokens.
var tokenIDArg = argSpec{Name: "token_id", Required: true}

// newTokensCommand builds parley tokens: the caller's own personal access
// tokens.
func newTokensCommand() *cobra.Command {
	return group("tokens", "Create, read, change and revoke your personal access tokens",
		declare(command{
			Name:        "list",
			Description: "List your personal access tokens, newest first",
			Method:      http.MethodGet,
			Path:        "/user/tokens",
			Auth:        authUser,
		}),
		declare(command{
			Name:        "create",
			Description: "Create a personal access token",
			Method:      http.MethodPost,
			Path:        "/user/tokens",
			Auth:        authUser,
			ShowsOnce:   true,
			Flags: func(fs *pflag.FlagSet) {
				fs.String("name", "", "the token's name")
				fs.String("permissions", "", "the token's scopes, separated by commas")
				// They fail only for a flag the set does not hold.
				_ = cobra.MarkFlagRequired(fs, "name")
				_ = cobra.MarkFlagRequired(fs, "permissions")
				fs.StringArray("paths", nil, "globs of the repository paths the token may write, separated by commas; repeatable, and sent only when given")
				fs.Int("expires", 90, "the token's lifetime in days, or 0 for a token that does not expire")
			},
			Run: createToken,
		}),
		declare(command{
			Name:        "show",
			Description: "Show one of your personal access tokens",
			Method:      http.MethodGet,
			Path:        "/user/tokens/:token_id",
			Args:        []argSpec{tokenIDArg},
			Auth:        authUser,
		}),
		declare(command{
			Name:        "revoke",
			Description: "Revoke one of your personal access tokens",
			Method:      http.MethodDelete,
			Path:        "/user/tokens/:token_id",
			Args:        []argSpec{tokenIDArg},
			Auth:        authUser,
		}),
		changePermissions("replace", "Replace the scopes of a token; a token left with none is revoked", http.MethodPut, true),
		changePermissions("add", "Add scopes to a token", http.MethodPatch, false),
		changePermissions("remove", "Remove scopes from a token; a token left with none is revoked", http.MethodDelete, true),
	)
}

// splitList is the comma-separated list s, split at every comma as given:
// no quoting, so a glob may hold any other character, and the empty string
// is the empty list.
func splitList(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, ",")
}

// createToken is the run step of parley tokens create. The body carries
// paths only when --paths was given, so that a token without them has no
// path limit; scope names, globs and lifetimes are the hub's to judge.
func createToken(c *call) error {
	fs := c.cmd.Flags()
	name, _ := fs.GetString("name")
	permissions, _ := fs.GetString("permissions")
	expires, _ := fs.GetInt("expires")
	body := map[string]any{"name": name, "permissions": splitList(permissions), "expires": expires}
	if fs.Changed("paths") {
		values, _ := fs.GetStringArray("paths")
		paths := []string{}
		for _, v := range values {
			paths = append(paths, splitList(v)...)
		}
		body["paths"] = paths
	}
	return sendWith(c, nil, body)
}

// changePermissions builds parley tokens replace, add or remove: method to
// the token's permissions with the --permissions list. A command whose
// change can leave the token without scopes, which revokes it, notes the
// revocation on stderr after the printed token.
func changePermissions(name, description, method string, notesRevocation bool) *cobra.Command {
	return declare(command{
		Name:        name,
		Description: description,
		Method:      method,
		Path:        "/user/tokens/:token_id/permissions",
		Args:        []argSpec{tokenIDArg},
		Auth:        authUser,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("permissions", "", "scopes, separated by commas; an empty value is the empty list")
			// It fails only for a flag the set does not hold.
			_ = cobra.MarkFlagRequired(fs, "permissions")
		},
		Run: func(c *call) error {
			permissions, _ := c.cmd.Flags().GetString("permissions")
			resp, err := c.client.request(c.ctx, c.decl.Method, routePath(c.decl.Path, c.args), nil,
				map[string][]string{"permissions": splitList(permissions)})
			if err != nil {
				return err
			}
			if err := c.printAnswer(resp); err != nil {
				return err
			}
			if notesRevocation && revoked(resp.body) {
				warn(c.stderr, "the token was revoked: it has no permissions left")
			}
			return nil
		},
	})
}

// revoked tells whether the Token body has a revoked_at that is not null.
func revoked(body []byte) bool {
	var token struct {
		RevokedAt json.RawMessage `json:"revoked_at"`
	}
	if json.Unmarshal(body, &token) != nil {
		return false
	}
	return len(token.RevokedAt) > 0 && string(token.RevokedAt) != "null"
}
