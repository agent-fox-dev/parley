package secret_test

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/secret"
)

// An org member's fork runs with its parent's variables, read through the
// resolved route, and without its parent's secrets, which only a fork of
// someone who may write to the parent receives.
func TestResolution_AMembersForkGetsTheParentsVariablesAndNotItsSecrets(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addOwnFork(t)
	w.putVars(t, w.ownerKey, "/workspaces/acme/vars", "REGION", "eu")
	if a := w.do(t, w.ownerKey, http.MethodPost, "/workspaces/acme/secrets",
		entries(map[string]any{"key": "DEPLOY_KEY", "value": "s3"})); a.status != http.StatusCreated {
		t.Fatalf("store the secret DEPLOY_KEY in acme = %d %s, want 201", a.status, a.body)
	}

	got := w.resolve(t, w.memberKey, "acme-fork")
	if i := slices.IndexFunc(got, func(r resolved) bool { return r.Key == "REGION" }); i < 0 ||
		got[i].Value != "eu" || got[i].Origin != string(secret.OriginParent) {
		t.Errorf("the member's resolved variables of acme-fork = %+v, want REGION = eu from the parent", got)
	}

	ctx := context.Background()
	member, err := w.secrets.ResolveSecrets(ctx, "acme-fork", []string{"DEPLOY_KEY"})
	if err != nil {
		t.Fatalf("resolve DEPLOY_KEY for acme-fork: %v", err)
	}
	if want := []secret.ResolvedSecret{missing("DEPLOY_KEY")}; !slices.Equal(member.Secrets, want) {
		t.Errorf("DEPLOY_KEY for the member's fork = %+v, want missing", member.Secrets)
	}
	if got, want := origins(member.Searched), []secret.Origin{secret.OriginWorkspace, secret.OriginOrg, secret.OriginUser}; !slices.Equal(got, want) {
		t.Errorf("the member's fork searched %v, want %v", got, want)
	}

	owners, err := w.secrets.ResolveSecrets(ctx, "acme-own", []string{"DEPLOY_KEY"})
	if err != nil {
		t.Fatalf("resolve DEPLOY_KEY for acme-own: %v", err)
	}
	if want := []secret.ResolvedSecret{found("DEPLOY_KEY", "s3", secret.OriginParent)}; !slices.Equal(owners.Secrets, want) {
		t.Errorf("DEPLOY_KEY for the owner's fork = %+v, want s3 from the parent", owners.Secrets)
	}
}
