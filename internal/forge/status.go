package forge

import (
	"context"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// SecretWebhook is the workspace secret that holds the forge webhook's
// signing secret.
const SecretWebhook = "FORGE_WEBHOOK_SECRET"

// remoteStatus is how the hub reaches the forge of one remote target.
type remoteStatus struct {
	Forge ForgeKind `json:"forge"`
	// APIURL is nil when the forge is unknown.
	APIURL *string `json:"api_url"`
	// Repository is nil when the remote URL names none.
	Repository *repositoryStatus `json:"repository"`
	Credential CredentialKind    `json:"credential"`
}

type repositoryStatus struct {
	Host  string `json:"host"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

type webhookStatus struct {
	Configured     bool          `json:"configured"`
	URL            *string       `json:"url"`
	LastDeliveryAt *timefmt.Time `json:"last_delivery_at"`
}

// status is the diagnostic document: the origin's forge, the upstream's
// on a carry-patch workspace with an upstream_url, and the webhook.
type status struct {
	remoteStatus
	Upstream *remoteStatus `json:"upstream"`
	Webhook  webhookStatus `json:"webhook"`
}

// serveStatus answers the diagnostic document of the workspace whose
// remote is used, a task fork's parent included, without calling the
// forge. A target that cannot be resolved is described, not refused; only
// an API URL setting that applies and fails its check is refused, since a
// forge route of the workspace would be refused for it too.
func (c *Component) serveStatus(w http.ResponseWriter, r *http.Request, call *Call) error {
	ctx, ws := r.Context(), call.Workspace
	targets := []Target{TargetOrigin}
	if tracksUpstream(ws) {
		targets = append(targets, TargetUpstream)
	}
	for _, t := range targets {
		if err := CheckAPIURL(call.Settings.For(t)); err != nil {
			return server.NewError(ErrorTypeAPIURLInvalid, http.StatusUnprocessableEntity,
				fmt.Sprintf("the setting %s %s", t.SettingName(), err))
		}
	}
	var doc status
	for _, t := range targets {
		d, err := c.describe(ctx, ws, t, call.Settings.For(t))
		if err != nil {
			return err
		}
		if t == TargetUpstream {
			doc.Upstream = &d
		} else {
			doc.remoteStatus = d
		}
	}
	var err error
	if doc.Webhook, err = c.webhook(ctx, ws.Slug); err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, doc)
	return nil
}

// describe derives the forge of the target t of ws with its API URL
// setting, whose check has passed. The credential is reported by its kind
// alone.
func (c *Component) describe(ctx context.Context, ws workspace.Workspace, t Target, setting string) (remoteStatus, error) {
	d := remoteStatus{Forge: KindUnknown}
	remote := ws.GitURL
	if t == TargetUpstream {
		remote = ws.UpstreamURL
	}
	if remote != nil {
		if repo, ok := ParseRemote(*remote); ok {
			d.Repository = &repositoryStatus{Host: repo.Host, Owner: repo.Owner, Name: repo.Name}
			if kind := Kind(setting, repo.Host); kind != KindUnknown {
				d.Forge, d.APIURL = kind, new(APIURL(kind, setting, repo.Host))
			}
		}
	}
	cred, err := ResolveCredential(ctx, c.deps.Secrets, ws.Slug, t)
	if err != nil {
		return remoteStatus{}, err
	}
	d.Credential = cred.Kind
	return d, nil
}

// webhook describes the forge webhook of the workspace slug: whether its
// secret is set, the hub's URL for it, and when the newest delivery was
// received. The secret's value is used for nothing else.
func (c *Component) webhook(ctx context.Context, slug string) (webhookStatus, error) {
	value, _, err := c.deps.Secrets.Get(ctx, secret.ScopeWorkspace, slug, SecretWebhook)
	if err != nil {
		return webhookStatus{}, fmt.Errorf("read the webhook secret of %s: %w", slug, err)
	}
	h := webhookStatus{Configured: value != "", URL: c.webhookURL(slug)}
	if p := c.lastDeliveryProvider(); p != nil {
		at, found, err := p(ctx, slug)
		if err != nil {
			return webhookStatus{}, fmt.Errorf("read the newest webhook delivery of %s: %w", slug, err)
		}
		if found {
			h.LastDeliveryAt = new(timefmt.Time(at))
		}
	}
	return h, nil
}
