// Package openshelltest stands in for the OpenShell gateway in tests of the
// OpenShell runtime client (internal/sandbox/runtime/openshell), of the hub
// that builds that client, and end to end. It is test support, imported by
// test code only.
//
// Gateway is the SDK's in-memory client (fake.NewClient) with what it lacks
// for those tests: it keeps provider profiles per workspace, and it can be
// made unreachable and reachable again, which closing the in-memory client
// cannot undo.
//
// It is not a double of the runtime: runtimetest.Fake
// (internal/sandbox/runtime/runtimetest) stays the in-memory runtime that
// the other sandbox packages test against.
package openshelltest

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/fake"
)

// Gateway is a v1.ClientInterface over the SDK's in-memory client that also
// keeps provider profiles per workspace. Unreachable, every call of its
// sub-clients fails with a v1.StatusError of code v1.ErrorUnavailable. It
// is safe for concurrent use.
type Gateway struct {
	live *fake.Client
	dead *fake.Client
	prof *profiles

	mu   sync.RWMutex
	down bool
}

var _ v1.ClientInterface = (*Gateway)(nil)

// New returns a reachable gateway over fake.NewClient(opts...), with no
// provider profiles.
func New(opts ...fake.ClientOption) *Gateway {
	dead := fake.NewClient()
	_ = dead.Close()
	live := fake.NewClient(opts...)
	return &Gateway{live: live, dead: dead, prof: &profiles{inner: live.Providers().Profiles(), items: map[string]v1.ProviderProfile{}}}
}

// Fake returns the SDK's in-memory client the gateway runs over, for what
// the gateway adds nothing to, such as seeding workspaces, sandboxes or
// providers, or reading them back.
func (g *Gateway) Fake() *fake.Client { return g.live }

// SetUnreachable makes every later call fail as unavailable (true) or
// answer again (false). It is safe to call while calls run.
func (g *Gateway) SetUnreachable(unreachable bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.down = unreachable
}

// Unreachable reports what SetUnreachable last set.
func (g *Gateway) Unreachable() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.down
}

// AddProfile adds or replaces a provider profile of a workspace, as if it
// had been installed there.
func (g *Gateway) AddProfile(workspace string, p v1.ProviderProfile) {
	g.prof.put(workspace, p)
}

// Profile returns a provider profile of a workspace as it was installed.
func (g *Gateway) Profile(workspace, id string) (v1.ProviderProfile, bool) {
	return g.prof.get(workspace, id)
}

// client is the in-memory client calls go to: the closed one while the
// gateway is unreachable.
func (g *Gateway) client() *fake.Client {
	if g.Unreachable() {
		return g.dead
	}
	return g.live
}

func (g *Gateway) Sandboxes() v1.SandboxInterface { return g.client().Sandboxes() }

func (g *Gateway) SandboxTemplates() v1.SandboxTemplateInterface {
	return g.client().SandboxTemplates()
}

func (g *Gateway) CreateSandboxFromTemplate(ctx context.Context, workspace, name, templateName string, spec *v1.SandboxSpec, opts ...v1.CreateOption) (*v1.Sandbox, error) {
	return g.client().CreateSandboxFromTemplate(ctx, workspace, name, templateName, spec, opts...)
}

func (g *Gateway) SandboxSuspendResume() v1.SandboxSuspendResumeInterface {
	return g.client().SandboxSuspendResume()
}

// Providers is the in-memory client's providers, with the gateway's
// provider profiles.
func (g *Gateway) Providers() v1.ProviderInterface {
	if g.Unreachable() {
		return g.dead.Providers()
	}
	return providers{ProviderInterface: g.live.Providers(), profiles: g.prof}
}

func (g *Gateway) Services() v1.ServiceInterface { return g.client().Services() }
func (g *Gateway) Exec() v1.ExecInterface        { return g.client().Exec() }
func (g *Gateway) Files() v1.FileInterface       { return g.client().Files() }
func (g *Gateway) Health() v1.HealthInterface    { return g.client().Health() }
func (g *Gateway) SSH() v1.SSHInterface          { return g.client().SSH() }
func (g *Gateway) TCP() v1.TCPInterface          { return g.client().TCP() }
func (g *Gateway) Config() v1.ConfigInterface    { return g.client().Config() }
func (g *Gateway) Policy() v1.PolicyInterface    { return g.client().Policy() }

func (g *Gateway) Workspaces() v1.WorkspaceInterface { return g.client().Workspaces() }

// Close closes the in-memory client: every later call fails as
// unavailable, for good.
func (g *Gateway) Close() error { return g.live.Close() }

type providers struct {
	v1.ProviderInterface
	profiles *profiles
}

func (p providers) Profiles() v1.ProfileInterface { return p.profiles }

// profiles keeps provider profiles by workspace and id. Lint is the
// in-memory client's, which does not implement it.
type profiles struct {
	inner v1.ProfileInterface

	mu    sync.Mutex
	items map[string]v1.ProviderProfile // by workspace + "/" + id
}

var _ v1.ProfileInterface = (*profiles)(nil)

func (p *profiles) put(workspace string, pp v1.ProviderProfile) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pp.ResourceVersion == 0 {
		pp.ResourceVersion = 1
	}
	p.items[workspace+"/"+pp.ID] = copyProfile(pp)
}

func (p *profiles) get(workspace, id string) (v1.ProviderProfile, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pp, ok := p.items[workspace+"/"+id]
	return copyProfile(pp), ok
}

func notFound(id string) error {
	return &v1.StatusError{Code: v1.ErrorNotFound, Message: id + " not found"}
}

func (p *profiles) List(workspace string, _ ...v1.ListOptions) (*v1.Pager[*v1.ProviderProfile], error) {
	return v1.NewPager("", func(_ context.Context, _ string) (*v1.Page[*v1.ProviderProfile], error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		var items []*v1.ProviderProfile
		for key, pp := range p.items {
			if key == workspace+"/"+pp.ID {
				cp := copyProfile(pp)
				items = append(items, &cp)
			}
		}
		slices.SortFunc(items, func(a, b *v1.ProviderProfile) int { return cmp.Compare(a.ID, b.ID) })
		return &v1.Page[*v1.ProviderProfile]{Items: items}, nil
	}), nil
}

func (p *profiles) ListAll(ctx context.Context, workspace string, opts ...v1.ListOptions) ([]*v1.ProviderProfile, error) {
	pager, err := p.List(workspace, opts...)
	if err != nil {
		return nil, err
	}
	return pager.All(ctx)
}

func (p *profiles) Get(_ context.Context, workspace, id string) (*v1.ProviderProfile, error) {
	pp, ok := p.get(workspace, id)
	if !ok {
		return nil, notFound(id)
	}
	return &pp, nil
}

// Import adds the profiles; one whose id the workspace holds already fails
// the whole import as already existing.
func (p *profiles) Import(_ context.Context, workspace string, items []v1.ProfileImportItem) (*v1.ImportResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, it := range items {
		if _, ok := p.items[workspace+"/"+it.Profile.ID]; ok {
			return nil, &v1.StatusError{Code: v1.ErrorAlreadyExists, Message: it.Profile.ID + " already exists"}
		}
	}
	res := &v1.ImportResult{Imported: true}
	for _, it := range items {
		pp := copyProfile(it.Profile)
		pp.ResourceVersion = 1
		p.items[workspace+"/"+pp.ID] = pp
		res.Profiles = append(res.Profiles, copyProfile(pp))
	}
	return res, nil
}

// Update replaces a profile whose resource version is version.
func (p *profiles) Update(_ context.Context, workspace, id string, version uint64, item v1.ProfileImportItem) (*v1.UpdateResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old, ok := p.items[workspace+"/"+id]
	if !ok {
		return nil, notFound(id)
	}
	if old.ResourceVersion != version {
		return nil, &v1.StatusError{Code: v1.ErrorConflict, Message: "resource version mismatch"}
	}
	pp := copyProfile(item.Profile)
	pp.ID = id
	pp.ResourceVersion = version + 1
	p.items[workspace+"/"+id] = pp
	return &v1.UpdateResult{}, nil
}

func (p *profiles) Lint(ctx context.Context, workspace string, items []v1.ProfileImportItem) (*v1.LintResult, error) {
	return p.inner.Lint(ctx, workspace, items)
}

func (p *profiles) Delete(_ context.Context, workspace, id string, opts ...v1.DeleteOptions) (*v1.DeletionResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.items[workspace+"/"+id]; !ok {
		if len(opts) > 0 && opts[0].AllowMissing {
			return &v1.DeletionResult{Outcome: v1.DeletionAlreadyAbsent}, nil
		}
		return nil, notFound(id)
	}
	delete(p.items, workspace+"/"+id)
	return &v1.DeletionResult{Outcome: v1.DeletionCompleted}, nil
}

// copyProfile copies the slices and maps of a profile that callers might
// change.
func copyProfile(pp v1.ProviderProfile) v1.ProviderProfile {
	pp.Credentials = slices.Clone(pp.Credentials)
	for i := range pp.Credentials {
		pp.Credentials[i].EnvVars = slices.Clone(pp.Credentials[i].EnvVars)
	}
	pp.Files = slices.Clone(pp.Files)
	pp.Endpoints = slices.Clone(pp.Endpoints)
	pp.Binaries = slices.Clone(pp.Binaries)
	pp.Discovery.Credentials = slices.Clone(pp.Discovery.Credentials)
	pp.Annotations = maps.Clone(pp.Annotations)
	return pp
}
