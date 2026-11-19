package cli

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The routes of a workspace's event webhooks, relative to /api/v1.
const (
	eventWebhooks = "/workspaces/:slug/webhooks"
	eventWebhook  = eventWebhooks + "/:id"
)

// The positional arguments of the event webhook commands besides <slug>.
var (
	webhookIDArg  = argSpec{Name: "id", Required: true}
	deliveryIDArg = argSpec{Name: "delivery-id", Required: true}
)

// The flag pairs of parley webhook update that each set one member; at most
// one flag of a pair is given. A flag of a pair counts only when it is true:
// --enable=false asks for nothing, and must not switch the subscription on.
var (
	forksPair  = [2]string{"forks", "no-forks"}
	activePair = [2]string{"enable", "disable"}
)

// newEventWebhookCommand builds parley webhook: the subscriptions that have
// the hub push a workspace's events to a URL, and what was delivered to
// them. It is not parley forge webhook, the secret of the webhooks a forge
// sends the hub.
func newEventWebhookCommand() *cobra.Command {
	itemArgs := []argSpec{slugArg, webhookIDArg}
	return group("webhook", "Subscribe a URL to a workspace's events, and see and resend what the hub delivered to it",
		declare(command{
			Name:        "create",
			Description: "Subscribe a URL to the events of a workspace and its task forks; prints the signing secret, shown once",
			Method:      http.MethodPost,
			Path:        eventWebhooks,
			Args:        []argSpec{slugArg},
			Auth:        authUser,
			Flags: func(fs *pflag.FlagSet) {
				fs.String("url", "", "the URL the hub posts each event to")
				_ = cobra.MarkFlagRequired(fs, "url")
				fs.String("types", "", "the event types sent, a comma-separated list of exact types and prefixes ending in * (default: every type)")
				fs.Bool("no-forks", false, "leave out the events of the workspace's task forks")
				fs.String("description", "", "a note on what the subscription is for")
			},
			Run: sendShowingSecret(createBody),
		}),
		scmRead{
			command: command{Name: "list", Description: "List the event webhooks of a workspace, oldest first", Path: eventWebhooks, Args: []argSpec{slugArg}},
			query: []queryFlag{
				{name: "state", param: "state", usage: "only the subscriptions in this state: active, failing or disabled"},
				limitFlag, cursorFlag,
			},
		}.build(),
		declare(command{
			Name:        "get",
			Description: "Show an event webhook, with the number of events it has yet to deliver",
			Method:      http.MethodGet,
			Path:        eventWebhook,
			Args:        itemArgs,
			Auth:        authUser,
		}),
		checkFlags(declare(command{
			Name:        "update",
			Description: "Change an event webhook's URL, types or description, or switch it off and on",
			Method:      http.MethodPatch,
			Path:        eventWebhook,
			Args:        itemArgs,
			Auth:        authUser,
			Flags: func(fs *pflag.FlagSet) {
				fs.String("url", "", "the URL the hub posts each event to")
				fs.String("types", "", "the event types sent, a comma-separated list of exact types and prefixes ending in *")
				fs.Bool(forksPair[0], false, "send the events of the workspace's task forks too")
				fs.Bool(forksPair[1], false, "leave out the events of the workspace's task forks")
				fs.String("description", "", "a note on what the subscription is for")
				fs.Bool(activePair[0], false, "switch the subscription on; a disabled one starts with the events written after this")
				fs.Bool(activePair[1], false, "switch the subscription off, ending its pending delivery")
			},
			Run: func(c *call) error { return sendWith(c, nil, updateBody(c.cmd.Flags())) },
		}), checkUpdateFlags),
		declare(command{
			Name:        "delete",
			Description: "Delete an event webhook and its delivery records",
			Method:      http.MethodDelete,
			Path:        eventWebhook,
			Args:        itemArgs,
			Auth:        authUser,
			Confirm: func(c *call) any {
				return struct {
					Status string `json:"status"`
					ID     string `json:"id"`
				}{"deleted", c.args[webhookIDArg.Name]}
			},
		}),
		declare(command{
			Name:        "rotate-secret",
			Description: "Replace an event webhook's signing secret; prints the new one, shown once",
			Method:      http.MethodPost,
			Path:        eventWebhook + "/rotate-secret",
			Args:        itemArgs,
			Auth:        authUser,
			Run:         sendShowingSecret(func(*pflag.FlagSet) any { return nil }),
		}),
		declare(command{
			Name:        "ping",
			Description: "Send an event webhook a test delivery; exits 1 when the receiver does not accept it",
			Method:      http.MethodPost,
			Path:        eventWebhook + "/ping",
			Args:        itemArgs,
			Auth:        authUser,
			Run:         sendDelivery,
		}),
		scmRead{
			command: command{Name: "deliveries", Description: "List an event webhook's deliveries of the last 7 days, newest first", Path: eventWebhook + "/deliveries", Args: itemArgs},
			query: []queryFlag{
				{name: "status", param: "status", usage: "only the deliveries in this status: pending, succeeded or failed"},
				limitFlag, cursorFlag,
			},
		}.build(),
		declare(command{
			Name:        "redeliver",
			Description: "Send a delivered or failed event once more under its delivery id; exits 1 when the receiver does not accept it",
			Method:      http.MethodPost,
			Path:        eventWebhook + "/deliveries/:delivery-id/redeliver",
			Args:        []argSpec{slugArg, webhookIDArg, deliveryIDArg},
			Auth:        authUser,
			Run:         sendDelivery,
		}),
	)
}

// createBody is the body of parley webhook create: url, and a member for
// each other flag given, so that the hub's default applies to the others.
func createBody(fs *pflag.FlagSet) any {
	url, _ := fs.GetString("url")
	body := map[string]any{"url": url}
	if fs.Changed("types") {
		body["types"] = typesOf(fs)
	}
	if noForks, _ := fs.GetBool("no-forks"); noForks {
		body["include_forks"] = false
	}
	givenFields(fs, body, map[string]string{"description": "description"})
	return body
}

// updateBody is the body of parley webhook update: a member for each flag
// given, the pairs as include_forks and active.
func updateBody(fs *pflag.FlagSet) map[string]any {
	body := map[string]any{}
	givenFields(fs, body, map[string]string{"url": "url", "description": "description"})
	if fs.Changed("types") {
		body["types"] = typesOf(fs)
	}
	for member, pair := range map[string][2]string{"include_forks": forksPair, "active": activePair} {
		switch {
		case flagSet(fs, pair[0]):
			body[member] = true
		case flagSet(fs, pair[1]):
			body[member] = false
		}
	}
	return body
}

// flagSet reports whether the bool flag name was given as true.
func flagSet(fs *pflag.FlagSet, name string) bool {
	on, _ := fs.GetBool(name)
	return on
}

// typesOf is --types split at its commas: the hub takes the types as a
// list, and judges each entry itself.
func typesOf(fs *pflag.FlagSet) []string {
	types, _ := fs.GetString("types")
	return strings.Split(types, ",")
}

// checkUpdateFlags refuses an update that would change nothing, and one
// given both flags of a pair, which contradict each other.
func checkUpdateFlags(cmd *cobra.Command) error {
	fs := cmd.Flags()
	for _, pair := range [][2]string{forksPair, activePair} {
		if flagSet(fs, pair[0]) && flagSet(fs, pair[1]) {
			return usageError("%s takes one of --%s and --%s, not both", cmd.CommandPath(), pair[0], pair[1])
		}
	}
	if len(updateBody(fs)) == 0 {
		return usageError("%s needs at least one of --url, --types, --forks, --no-forks, --description, --enable and --disable", cmd.CommandPath())
	}
	return nil
}

// sendShowingSecret is the run step of a command whose answer carries a
// signing secret: it sends the body built from the flags, prints the
// answer, and then the show-once warning, only when the answer holds a
// secret, never the secret itself: an Idempotency-Key replay of a create
// answers without it.
func sendShowingSecret(body func(fs *pflag.FlagSet) any) func(c *call) error {
	return func(c *call) error {
		resp, err := c.client.request(c.ctx, c.decl.Method, routePath(c.decl.Path, c.args), nil, body(c.cmd.Flags()))
		if err != nil {
			return err
		}
		if err := c.printAnswer(resp); err != nil {
			return err
		}
		var answer map[string]json.RawMessage
		if json.Unmarshal(resp.body, &answer) == nil {
			if _, shown := answer["secret"]; shown {
				warn(c.stderr, showOnceWarning)
			}
		}
		return nil
	}
}

// sendDelivery is the run step of parley webhook ping and redeliver: the
// hub answers 200 with the delivery record whatever the receiver answered,
// so the record is printed, and a failed one ends the command with
// delivery_failed, for a script to tell by the exit status whether the
// receiver works.
func sendDelivery(c *call) error {
	resp, err := c.client.request(c.ctx, c.decl.Method, routePath(c.decl.Path, c.args), nil, nil)
	if err != nil {
		return err
	}
	if err := c.printAnswer(resp); err != nil {
		return err
	}
	var record struct {
		Status       string  `json:"status"`
		ResponseCode *int    `json:"response_code"`
		Error        *string `json:"error"`
	}
	if json.Unmarshal(resp.body, &record) != nil || record.Status != "failed" {
		return nil
	}
	switch {
	case record.Error != nil:
		return deliveryFailed("the receiver did not accept the delivery: %s", *record.Error)
	case record.ResponseCode != nil:
		return deliveryFailed("the receiver did not accept the delivery: it answered %d", *record.ResponseCode)
	}
	return deliveryFailed("the receiver did not accept the delivery")
}
