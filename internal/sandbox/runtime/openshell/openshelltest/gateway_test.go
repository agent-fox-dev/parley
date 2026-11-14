package openshelltest_test

import (
	"context"
	"testing"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/openshell/openshelltest"
)

func TestGateway_KeepsProviderProfilesPerWorkspace(t *testing.T) {
	t.Parallel()
	g := openshelltest.New()
	ctx := t.Context()
	g.AddProfile("parley", v1.ProviderProfile{ID: "anthropic", Credentials: []v1.ProfileCredential{{Name: "api_key", EnvVars: []string{"ANTHROPIC_API_KEY"}}}})
	profiles := g.Providers().Profiles()
	if _, err := profiles.Import(ctx, "parley", []v1.ProfileImportItem{{Profile: v1.ProviderProfile{ID: "hub"}}}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if _, err := profiles.Import(ctx, "parley", []v1.ProfileImportItem{{Profile: v1.ProviderProfile{ID: "hub"}}}); !v1.IsAlreadyExists(err) {
		t.Fatalf("Import of an existing id = %v, want already exists", err)
	}
	if _, err := profiles.Get(ctx, "other", "anthropic"); !v1.IsNotFound(err) {
		t.Fatalf("Get in another workspace = %v, want not found", err)
	}
	list, err := profiles.ListAll(ctx, "parley")
	if err != nil || len(list) != 2 || list[0].ID != "anthropic" || list[1].ID != "hub" {
		t.Fatalf("ListAll = %v, %v; want anthropic and hub", list, err)
	}
	old, _ := profiles.Get(ctx, "parley", "hub")
	if _, err := profiles.Update(ctx, "parley", "hub", old.ResourceVersion, v1.ProfileImportItem{Profile: v1.ProviderProfile{ID: "hub", DisplayName: "Hub"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := profiles.Update(ctx, "parley", "hub", old.ResourceVersion, v1.ProfileImportItem{Profile: v1.ProviderProfile{ID: "hub"}}); !v1.IsConflict(err) {
		t.Fatalf("Update with a stale version = %v, want a conflict", err)
	}
	if got, ok := g.Profile("parley", "hub"); !ok || got.DisplayName != "Hub" {
		t.Fatalf("Profile = %+v, %v; want the updated profile", got, ok)
	}
}

func TestGateway_IsUnreachableUntilToldOtherwise(t *testing.T) {
	t.Parallel()
	g := openshelltest.New()
	ctx := context.Background()
	calls := map[string]func() error{
		"sandboxes": func() error { _, err := g.Sandboxes().ListAll(ctx, "parley"); return err },
		"profiles":  func() error { _, err := g.Providers().Profiles().ListAll(ctx, "parley"); return err },
		"health":    func() error { _, err := g.Health().Check(ctx); return err },
	}
	for _, down := range []bool{true, false, true, false} {
		g.SetUnreachable(down)
		for name, call := range calls {
			if err := call(); v1.IsUnavailable(err) != down {
				t.Fatalf("unreachable %v: %s = %v", down, name, err)
			}
		}
	}
}
