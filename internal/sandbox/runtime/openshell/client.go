// Package openshell is the runtime client over the OpenShell Go SDK
// (github.com/ai-openshell/sdk-go, package openshell/v1). It is the only
// production package of the hub that imports the SDK.
//
// Every operation acts in the client's OpenShell workspace, which it passes
// to every SDK call. An operation the SDK has no call for answers with an
// error of kind runtime.ErrUnsupported and is not imitated with other calls:
// ValidatePolicy, GetPolicy, WatchSandboxes, RunMain, MakeDir, Stat, Walk and
// Logs, and Exec and ExecStream with input on stdin. A request with a part
// the SDK cannot carry is unsupported too, so that no part is silently
// dropped (CreateSandbox with a base document, StartSandbox with a policy
// value or an environment).
//
// The SDK has no working directory at creation: a sandbox is created with
// its working directory writable in its filesystem policy, and every
// command runs in the working directory its request names (an exec without
// one runs in the image's).
package openshell

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/scrub"
)

// callTimeout bounds every operation that does not stream.
const callTimeout = 30 * time.Second

// Deps is what a client is built with besides its configuration.
type Deps struct {
	// Scrubber scrubs every error's text. New registers the gateway token
	// and the OIDC client secret with it. Nil is a scrubber of the client's
	// own.
	Scrubber *scrub.Scrubber
	// Recorder hears every change of the reachability flag. Nil records
	// nothing.
	Recorder runtime.Recorder
	// Dial builds the SDK client from its configuration. Nil is
	// v1.NewClient; a test passes the SDK's in-memory client.
	Dial func(v1.Config) (v1.ClientInterface, error)
}

// New returns the runtime client of a checked [sandbox] section: it
// registers the gateway token and the OIDC client secret with the scrubber,
// builds the SDK configuration (BuildConfig) and the SDK client, which
// connects lazily, and acts in the section's openshell_workspace.
func New(settings *sandbox.Settings, deps Deps) (runtime.Client, error) {
	if deps.Scrubber == nil {
		deps.Scrubber = scrub.New()
	}
	if settings != nil {
		deps.Scrubber.Add(settings.Token())
		deps.Scrubber.Add(settings.OIDCClientSecret())
	}
	cfg, err := BuildConfig(settings)
	if err != nil {
		return nil, errors.New(deps.Scrubber.Scrub(err.Error()))
	}
	dial := deps.Dial
	if dial == nil {
		dial = func(cfg v1.Config) (v1.ClientInterface, error) { return v1.NewClient(cfg) }
	}
	ci, err := dial(cfg)
	if err != nil {
		return nil, runtime.NewError(runtime.ErrUnavailable, "", deps.Scrubber.Scrub("connecting to the gateway: "+err.Error()))
	}
	return NewWithClient(ci, settings.OpenShellWorkspace, deps), nil
}

// NewWithClient returns the runtime client over an SDK client, acting in
// workspace. Closing the SDK client makes every later operation that calls
// it fail as unavailable; an operation the SDK lacks still answers
// unsupported without calling it.
func NewWithClient(ci v1.ClientInterface, workspace string, deps Deps) runtime.Client {
	if deps.Scrubber == nil {
		deps.Scrubber = scrub.New()
	}
	return &client{sdk: ci, ws: workspace, scrub: deps.Scrubber, reach: runtime.NewReachability(deps.Recorder)}
}

type client struct {
	sdk   v1.ClientInterface
	ws    string
	scrub *scrub.Scrubber
	reach *runtime.Reachability
}

// result turns an operation's outcome into its error (nil, or a
// runtime.Error with a scrubbed text) and moves the reachability flag.
func (c *client) result(ctx context.Context, op string, err error) error {
	if err != nil {
		kind, reason := classify(ctx, err)
		err = runtime.NewError(kind, op, c.scrub.Scrub(reason))
	}
	return c.reach.Observe(err)
}

// call runs fn, an operation that does not stream, with a context that ends
// after callTimeout or with the caller's, whichever is sooner, and stops
// waiting for fn when it ends.
func call[T any](c *client, ctx context.Context, op string, fn func(ctx context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	type outcome struct {
		v   T
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		v, err := fn(ctx)
		done <- outcome{v, err}
	}()
	var zero T
	select {
	case o := <-done:
		if o.err != nil {
			return zero, c.result(ctx, op, o.err)
		}
		return o.v, c.result(ctx, op, nil)
	case <-ctx.Done():
		return zero, c.result(ctx, op, ctx.Err())
	}
}

// do is call for an operation that returns only an error.
func (c *client) do(ctx context.Context, op string, fn func(ctx context.Context) error) error {
	_, err := call(c, ctx, op, func(ctx context.Context) (struct{}, error) { return struct{}{}, fn(ctx) })
	return err
}

// Gateway.

func (c *client) Release(ctx context.Context) (runtime.Release, error) {
	return call(c, ctx, runtime.OpRelease, func(ctx context.Context) (runtime.Release, error) {
		h, err := c.sdk.Health().Check(ctx)
		if err != nil {
			return runtime.Release{}, err
		}
		info, err := c.sdk.Health().GetGatewayInfo(ctx)
		if err != nil {
			return runtime.Release{}, err
		}
		return runtime.Release{Healthy: h.Healthy, Version: info.Version}, nil
	})
}

func (c *client) EnsureWorkspace(ctx context.Context) error {
	return c.do(ctx, runtime.OpEnsureWorkspace, func(ctx context.Context) error {
		_, err := c.sdk.Workspaces().Get(ctx, c.ws)
		if !v1.IsNotFound(err) {
			return err
		}
		_, err = c.sdk.Workspaces().Create(ctx, c.ws, nil)
		if v1.IsAlreadyExists(err) {
			return nil
		}
		return err
	})
}

func (c *client) Reachable() bool { return c.reach.Reachable() }

// Provider profiles.

func (c *client) GetProviderProfile(ctx context.Context, id string) (runtime.ProviderProfile, error) {
	return call(c, ctx, runtime.OpGetProviderProfile, func(ctx context.Context) (runtime.ProviderProfile, error) {
		pp, err := c.sdk.Providers().Profiles().Get(ctx, c.ws, id)
		if err != nil {
			return runtime.ProviderProfile{}, err
		}
		return providerProfile(pp), nil
	})
}

// InstallProviderProfile installs a profile with one credential, named
// after the profile, whose placeholder goes into each of its variables, and
// its endpoints as the gateway's HTTP endpoints (httpEndpoints), one per
// path prefix.
func (c *client) InstallProviderProfile(ctx context.Context, p runtime.ProviderProfile) error {
	return c.do(ctx, runtime.OpInstallProviderProfile, func(ctx context.Context) error {
		item := profileItem(p)
		profiles := c.sdk.Providers().Profiles()
		old, err := profiles.Get(ctx, c.ws, p.ID)
		switch {
		case err == nil:
			_, err = profiles.Update(ctx, c.ws, p.ID, old.ResourceVersion, item)
			return err
		case !v1.IsNotFound(err):
			return err
		}
		res, err := profiles.Import(ctx, c.ws, []v1.ProfileImportItem{item})
		if err != nil {
			return err
		}
		if !res.Imported {
			return invalid(runtime.OpInstallProviderProfile, diagnostics(res.Diagnostics))
		}
		return nil
	})
}

func profileItem(p runtime.ProviderProfile) v1.ProfileImportItem {
	pp := v1.ProviderProfile{ID: p.ID, DisplayName: p.ID, Category: v1.ProfileCategoryOther}
	if len(p.Variables) > 0 {
		pp.Credentials = []v1.ProfileCredential{{Name: p.ID, EnvVars: slices.Clone(p.Variables), Required: true}}
	}
	for _, ep := range p.Endpoints {
		globs := make([]string, len(ep.Paths))
		for i, prefix := range ep.Paths {
			globs[i] = prefixGlob(prefix)
		}
		pp.Endpoints = append(pp.Endpoints, httpEndpoints(ep.Host, uint32(ep.Port), globs)...)
	}
	for _, prog := range p.Programs {
		pp.Binaries = append(pp.Binaries, v1.NetworkBinary{Path: prog})
	}
	return v1.ProfileImportItem{Profile: pp, Source: "parley"}
}

// httpEndpoints are the gateway's endpoints for HTTP on host and port: one
// per path glob, or one for every path when globs is empty. The gateway
// takes an L7 protocol, not a scheme: "rest" with every method allowed,
// which inspects HTTP with or without TLS. That inspection is what lets the
// gateway put a credential into a request, and it refuses a credentialed
// endpoint without it.
func httpEndpoints(host string, port uint32, globs []string) []v1.PolicyNetworkEndpoint {
	if len(globs) == 0 {
		globs = []string{""}
	}
	out := make([]v1.PolicyNetworkEndpoint, len(globs))
	for i, glob := range globs {
		out[i] = v1.PolicyNetworkEndpoint{Host: host, Port: port, Protocol: "rest", Access: v1.NetworkAccessPresetFull, Path: glob}
	}
	return out
}

// prefixGlob is the gateway's path glob of a path prefix: every path below
// it.
func prefixGlob(prefix string) string {
	return strings.TrimSuffix(prefix, "/") + "/**"
}

// providerProfile is a profile as the gateway holds it. The endpoints of one
// host and port, in the order installed, are one endpoint with their path
// prefixes; the gateway keeps no scheme, so Scheme is empty.
func providerProfile(pp *v1.ProviderProfile) runtime.ProviderProfile {
	p := runtime.ProviderProfile{ID: pp.ID}
	for _, cred := range pp.Credentials {
		p.Variables = append(p.Variables, cred.EnvVars...)
	}
	at := map[string]int{}
	for _, ep := range pp.Endpoints {
		key := fmt.Sprintf("%s:%d", ep.Host, ep.Port)
		i, seen := at[key]
		if !seen {
			i = len(p.Endpoints)
			at[key] = i
			p.Endpoints = append(p.Endpoints, runtime.Endpoint{Host: ep.Host, Port: int(ep.Port)})
		}
		if ep.Path != "" {
			p.Endpoints[i].Paths = append(p.Endpoints[i].Paths, strings.TrimSuffix(ep.Path, "**"))
		}
	}
	for _, b := range pp.Binaries {
		p.Programs = append(p.Programs, b.Path)
	}
	return p
}

func diagnostics(ds []v1.ProfileDiagnostic) string {
	if len(ds) == 0 {
		return "the gateway did not import the profile"
	}
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = strings.TrimPrefix(d.Field+": "+d.Message, ": ")
	}
	return strings.Join(parts, "; ")
}

// Providers.

// profileScopeWorkspace is the scope the gateway reports for a provider
// profile held in a workspace, such as the ones the client installs.
const profileScopeWorkspace = "workspace"

// EnsureProvider creates or replaces a provider of the profile p.Profile,
// holding p.Value as the profile's one credential. A profile that does not
// declare exactly one credential is refused as invalid.
func (c *client) EnsureProvider(ctx context.Context, p runtime.Provider) error {
	return c.do(ctx, runtime.OpEnsureProvider, func(ctx context.Context) error {
		pp, err := c.sdk.Providers().Profiles().Get(ctx, c.ws, p.Profile)
		if err != nil {
			return err
		}
		if len(pp.Credentials) != 1 {
			return invalid(runtime.OpEnsureProvider, fmt.Sprintf("provider profile %s declares %d credentials, a provider holds one", p.Profile, len(pp.Credentials)))
		}
		// The gateway keys a provider's credential by the variables its
		// profile declares for it, not by the credential's name.
		spec := v1.ProviderSpec{Credentials: map[string]string{}}
		for _, name := range pp.Credentials[0].EnvVars {
			spec.Credentials[name] = p.Value
		}
		if pp.Scope == profileScopeWorkspace {
			// The gateway looks a provider's profile up in the global scope
			// unless the provider names the workspace that holds it.
			spec.ProfileWorkspace = c.ws
		}
		_, err = c.sdk.Providers().Ensure(ctx, c.ws, &v1.Provider{
			Name:   p.Name,
			Type:   p.Profile,
			Labels: cloneMap(p.Labels),
			Spec:   spec,
		})
		return err
	})
}

func (c *client) DeleteProvider(ctx context.Context, name string) error {
	return c.do(ctx, runtime.OpDeleteProvider, func(ctx context.Context) error {
		_, err := c.sdk.Providers().Delete(ctx, c.ws, name)
		return err
	})
}

// AttachProvider attaches a provider of the client's workspace; one that is
// not there is not found.
func (c *client) AttachProvider(ctx context.Context, sandbox, provider string) error {
	return c.do(ctx, runtime.OpAttachProvider, func(ctx context.Context) error {
		if _, err := c.sdk.Providers().Get(ctx, c.ws, provider); err != nil {
			return err
		}
		_, err := c.sdk.Sandboxes().AttachProvider(ctx, c.ws, runtimeName(sandbox), provider, 0)
		return err
	})
}

func (c *client) DetachProvider(ctx context.Context, sandbox, provider string) error {
	return c.do(ctx, runtime.OpDetachProvider, func(ctx context.Context) error {
		_, err := c.sdk.Sandboxes().DetachProvider(ctx, c.ws, runtimeName(sandbox), provider, 0)
		return err
	})
}

func (c *client) SandboxProviders(ctx context.Context, sandbox string) ([]string, error) {
	return call(c, ctx, runtime.OpSandboxProviders, func(ctx context.Context) ([]string, error) {
		list, err := c.sdk.Sandboxes().ListAllProviders(ctx, c.ws, runtimeName(sandbox))
		if err != nil {
			return nil, err
		}
		names := make([]string, len(list))
		for i, p := range list {
			names[i] = p.Name
		}
		slices.Sort(names)
		return names, nil
	})
}

func (c *client) ListProviders(ctx context.Context) ([]runtime.Provider, error) {
	return call(c, ctx, runtime.OpListProviders, func(ctx context.Context) ([]runtime.Provider, error) {
		list, err := c.sdk.Providers().ListAll(ctx, c.ws)
		if err != nil {
			return nil, err
		}
		out := make([]runtime.Provider, len(list))
		for i, p := range list {
			out[i] = runtime.Provider{Name: p.Name, Profile: p.Type, Labels: cloneMap(p.Labels)}
		}
		slices.SortFunc(out, func(a, b runtime.Provider) int { return strings.Compare(a.Name, b.Name) })
		return out, nil
	})
}

// Policy.

// ValidatePolicy is unsupported: the SDK has no call that checks a policy
// value without a sandbox.
func (c *client) ValidatePolicy(context.Context, runtime.Policy) (runtime.PolicyValidation, error) {
	return runtime.PolicyValidation{}, c.reach.Observe(unsupported(runtime.OpValidatePolicy, "the SDK has no call that validates a policy without a sandbox"))
}

// GetPolicy is unsupported: the SDK holds a sandbox's policy as typed
// rules, so the base document a sandbox was created with cannot be read
// back.
func (c *client) GetPolicy(context.Context, string) (runtime.SandboxPolicy, error) {
	return runtime.SandboxPolicy{}, c.reach.Observe(unsupported(runtime.OpGetPolicy, "the SDK cannot return the policy value a sandbox was created with"))
}

// AddPolicyRule adds the rule to the sandbox's policy with the SDK's merge
// operation that adds a named network rule.
func (c *client) AddPolicyRule(ctx context.Context, sandbox string, rule runtime.Rule) error {
	return c.do(ctx, runtime.OpAddPolicyRule, func(ctx context.Context) error {
		r, err := networkRule(runtime.OpAddPolicyRule, rule)
		if err != nil {
			return err
		}
		_, err = c.sdk.Config().Update(ctx, c.ws, &v1.ConfigUpdate{
			Name:            runtimeName(sandbox),
			MergeOperations: []v1.PolicyMergeOperation{{AddRule: &v1.AddNetworkRule{RuleName: rule.Name, Rule: r}}},
		})
		return err
	})
}

// ListProposals returns the pending draft chunks of every sandbox of the
// workspace, by sandbox name.
func (c *client) ListProposals(ctx context.Context) ([]runtime.Proposal, error) {
	return call(c, ctx, runtime.OpListProposals, func(ctx context.Context) ([]runtime.Proposal, error) {
		sandboxes, err := c.sdk.Sandboxes().ListAll(ctx, c.ws)
		if err != nil {
			return nil, err
		}
		slices.SortFunc(sandboxes, func(a, b *v1.Sandbox) int { return strings.Compare(sandboxName(a), sandboxName(b)) })
		var out []runtime.Proposal
		for _, sb := range sandboxes {
			draft, err := c.sdk.Policy().GetDraft(ctx, c.ws, sb.Name, v1.WithStatusFilter("pending"))
			if v1.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			for _, ch := range draft.Chunks {
				if ch.Status == "pending" {
					out = append(out, proposal(sandboxName(sb), ch))
				}
			}
		}
		return out, nil
	})
}

func proposal(sandbox string, ch v1.PolicyChunk) runtime.Proposal {
	body := json.RawMessage(`{}`)
	if ch.ProposedRule != nil {
		if b, err := json.Marshal(ch.ProposedRule); err == nil {
			body = b
		}
	}
	p := runtime.Proposal{ID: ch.ID, Sandbox: sandbox, Rule: runtime.Rule{Name: ch.RuleName, Body: body}, Summary: ch.Rationale}
	if ch.SecurityNotes != "" {
		p.Findings = []runtime.Finding{{Severity: "security", Message: ch.SecurityNotes}}
	}
	return p
}

// pendingChunk returns the pending draft chunk id of a sandbox; one that is
// not pending, decided already included, is not found.
func (c *client) pendingChunk(ctx context.Context, op, sandbox, id string) (v1.PolicyChunk, error) {
	draft, err := c.sdk.Policy().GetDraft(ctx, c.ws, runtimeName(sandbox))
	if err != nil {
		return v1.PolicyChunk{}, err
	}
	for _, ch := range draft.Chunks {
		if ch.ID == id && ch.Status == "pending" {
			return ch, nil
		}
	}
	return v1.PolicyChunk{}, notFound(op, "no pending proposal "+id+" of sandbox "+sandbox)
}

func (c *client) ApproveProposal(ctx context.Context, sandbox, id string) error {
	return c.do(ctx, runtime.OpApproveProposal, func(ctx context.Context) error {
		ch, err := c.pendingChunk(ctx, runtime.OpApproveProposal, sandbox, id)
		if err != nil {
			return err
		}
		_, err = c.sdk.Policy().ApproveDraftChunk(ctx, c.ws, runtimeName(sandbox), id, ch.ReviewToken)
		return err
	})
}

func (c *client) RejectProposal(ctx context.Context, sandbox, id, reason string) error {
	return c.do(ctx, runtime.OpRejectProposal, func(ctx context.Context) error {
		if _, err := c.pendingChunk(ctx, runtime.OpRejectProposal, sandbox, id); err != nil {
			return err
		}
		return c.sdk.Policy().RejectDraftChunk(ctx, c.ws, runtimeName(sandbox), id, reason)
	})
}

// defaultReadOnly and defaultReadWrite are the filesystem part of the
// gateway's built-in policy. A typed policy replaces that policy whole, and
// a sandbox whose policy has no filesystem part cannot start its first
// process, so every policy the client sends has this part.
var (
	defaultReadOnly  = []string{"/bin", "/usr", "/lib", "/proc", "/dev/urandom", "/etc", "/var/log"}
	defaultReadWrite = []string{"/tmp", "/dev/null"}
)

// sdkPolicy is the SDK policy of a sandbox in workDir under a policy value:
// the filesystem part of the gateway's built-in policy with workDir
// writable, and the value's protected and added rules as named network
// rules. It is nil, the gateway's built-in policy, without a working
// directory and without rules. The SDK takes a typed policy, not a
// document, so the caller refuses a base document before; a rule the SDK's
// network rule cannot hold is unsupported, and an added rule named like an
// earlier rule, which would replace it, is invalid. A rule with neither
// endpoints nor binaries allows nothing and is left out.
func sdkPolicy(op, workDir string, p runtime.Policy) (*v1.SandboxPolicy, error) {
	rules := slices.Concat(p.Protected, p.Added)
	if len(rules) == 0 && workDir == "" {
		return nil, nil
	}
	readWrite := slices.Clone(defaultReadWrite)
	if workDir != "" {
		readWrite = append(readWrite, workDir)
	}
	out := &v1.SandboxPolicy{
		Version:    1,
		Filesystem: &v1.FilesystemPolicy{IncludeWorkdir: true, ReadOnly: slices.Clone(defaultReadOnly), ReadWrite: readWrite},
		Landlock:   &v1.LandlockPolicy{Compatibility: "best_effort"},
	}
	names := map[string]bool{}
	for _, rule := range rules {
		if names[rule.Name] {
			return nil, invalid(op, "policy rule "+rule.Name+" is given twice")
		}
		names[rule.Name] = true
		r, err := networkRule(op, rule)
		if err != nil {
			return nil, err
		}
		if len(r.Endpoints) == 0 && len(r.Binaries) == 0 {
			continue
		}
		if out.NetworkPolicies == nil {
			out.NetworkPolicies = map[string]v1.NetworkPolicyRule{}
		}
		out.NetworkPolicies[rule.Name] = r
	}
	return out, nil
}

// ruleDoc is a rule's body: a network rule in OpenShell's schema, with what
// the hub adds to it (ruleBody in internal/sandbox): the providers whose
// credentials the rule lets through, and an endpoint's scheme and path
// globs (endpointDoc).
type ruleDoc struct {
	Name      string                   `json:"name"`
	Providers []string                 `json:"providers"`
	Endpoints []json.RawMessage        `json:"endpoints"`
	Binaries  []v1.PolicyNetworkBinary `json:"binaries"`
}

// anyBinary is the gateway's binary glob that matches every program.
const anyBinary = "/**"

type endpointDoc struct {
	v1.PolicyNetworkEndpoint
	Scheme string   `json:"scheme"`
	Paths  []string `json:"paths"`
}

// networkRule decodes a rule's body into the SDK's network rule, refusing
// any member it has no field for. An endpoint with a scheme is an HTTP
// endpoint (httpEndpoints) for each path glob it lists. The providers are
// not sent: the SDK's rule has no field for them, and the gateway lets an
// attached provider's credential reach only the endpoints of its profile.
func networkRule(op string, rule runtime.Rule) (v1.NetworkPolicyRule, error) {
	refuse := func(err error) (v1.NetworkPolicyRule, error) {
		return v1.NetworkPolicyRule{}, unsupported(op, "policy rule "+rule.Name+" is not a network rule the SDK can carry: "+err.Error())
	}
	body := rule.Body
	if len(body) == 0 {
		body = json.RawMessage(`{}`)
	}
	var doc ruleDoc
	if err := strictDecode(body, &doc); err != nil {
		return refuse(err)
	}
	r := v1.NetworkPolicyRule{Name: rule.Name, Binaries: doc.Binaries}
	if len(r.Binaries) == 0 && len(doc.Endpoints) > 0 {
		// The gateway lets no program through a rule that names none; a
		// rule without binaries is meant for every program.
		r.Binaries = []v1.PolicyNetworkBinary{{Path: anyBinary}}
	}
	for _, raw := range doc.Endpoints {
		var ep endpointDoc
		if err := strictDecode(raw, &ep); err != nil {
			return refuse(err)
		}
		switch {
		case ep.Scheme == "" && len(ep.Paths) == 0:
			r.Endpoints = append(r.Endpoints, ep.PolicyNetworkEndpoint)
		case ep.Scheme != "http" && ep.Scheme != "https":
			return refuse(fmt.Errorf("an endpoint with paths needs the scheme http or https, not %q", ep.Scheme))
		case ep.Protocol != "" || ep.Path != "" || ep.Access != v1.NetworkAccessPresetUnspecified || len(ep.Rules) > 0:
			return refuse(errors.New("an endpoint with a scheme takes no protocol, path, access or rules"))
		default:
			r.Endpoints = append(r.Endpoints, httpEndpoints(ep.Host, ep.Port, ep.Paths)...)
		}
	}
	return r, nil
}

func strictDecode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Sandboxes.

// CreateSandbox creates the sandbox with its image, resources, environment,
// providers, labels and a policy (sdkPolicy) in which its working directory
// is writable and which holds the rules of its policy value. The SDK has no
// working directory at creation: the image holds the directory, and the
// commands run in it name it (see the package comment). The SDK takes no
// policy document, so a request with one is unsupported and does not reach
// the SDK: a sandbox must not run without the policy it was asked for.
func (c *client) CreateSandbox(ctx context.Context, spec runtime.SandboxSpec) (runtime.SandboxStatus, error) {
	if len(spec.Policy.Base) > 0 {
		return runtime.SandboxStatus{}, c.reach.Observe(unsupported(runtime.OpCreateSandbox, "the SDK takes no base policy document at creation"))
	}
	return call(c, ctx, runtime.OpCreateSandbox, func(ctx context.Context) (runtime.SandboxStatus, error) {
		policy, err := sdkPolicy(runtime.OpCreateSandbox, spec.WorkDir, spec.Policy)
		if err != nil {
			return runtime.SandboxStatus{}, err
		}
		sp := &v1.SandboxSpec{Environment: cloneMap(spec.Env), Providers: slices.Clone(spec.Providers), Policy: policy}
		if spec.Image != "" || spec.CPU != "" || spec.Memory != "" {
			sp.Template = &v1.SandboxTemplate{Image: spec.Image}
			limits := map[string]any{}
			if spec.CPU != "" {
				limits["cpu"] = spec.CPU
			}
			if spec.Memory != "" {
				limits["memory"] = spec.Memory
			}
			if len(limits) > 0 {
				sp.Template.Resources = map[string]any{"limits": limits}
			}
		}
		if spec.GPU > 0 {
			n := uint32(spec.GPU)
			sp.GPU, sp.GPUCount = true, &n
		}
		var opts []v1.CreateOption
		if len(spec.Labels) > 0 {
			opts = append(opts, v1.WithLabels(spec.Labels))
		}
		if runtimeName(spec.Name) != spec.Name {
			opts = append(opts, v1.WithAnnotations(map[string]string{nameAnnotation: spec.Name}))
		}
		sb, err := c.sdk.Sandboxes().Create(ctx, c.ws, runtimeName(spec.Name), sp, opts...)
		if err != nil {
			return runtime.SandboxStatus{}, err
		}
		return sandboxStatus(sb), nil
	})
}

func (c *client) GetSandbox(ctx context.Context, name string) (runtime.SandboxStatus, error) {
	return call(c, ctx, runtime.OpGetSandbox, func(ctx context.Context) (runtime.SandboxStatus, error) {
		sb, err := c.sdk.Sandboxes().Get(ctx, c.ws, runtimeName(name))
		if err != nil {
			return runtime.SandboxStatus{}, err
		}
		return sandboxStatus(sb), nil
	})
}

func (c *client) ListSandboxes(ctx context.Context) ([]runtime.SandboxStatus, error) {
	return call(c, ctx, runtime.OpListSandboxes, func(ctx context.Context) ([]runtime.SandboxStatus, error) {
		list, err := c.sdk.Sandboxes().ListAll(ctx, c.ws)
		if err != nil {
			return nil, err
		}
		out := make([]runtime.SandboxStatus, len(list))
		for i, sb := range list {
			out[i] = sandboxStatus(sb)
		}
		slices.SortFunc(out, func(a, b runtime.SandboxStatus) int { return strings.Compare(a.Name, b.Name) })
		return out, nil
	})
}

func (c *client) DeleteSandbox(ctx context.Context, name string) error {
	return c.do(ctx, runtime.OpDeleteSandbox, func(ctx context.Context) error {
		_, err := c.sdk.Sandboxes().Delete(ctx, c.ws, runtimeName(name))
		return err
	})
}

func (c *client) StopSandbox(ctx context.Context, name string) error {
	return c.do(ctx, runtime.OpStopSandbox, func(ctx context.Context) error {
		_, err := c.sdk.Sandboxes().Stop(ctx, c.ws, runtimeName(name))
		return err
	})
}

// StartSandbox starts a stopped sandbox. The SDK's start takes neither a
// policy nor an environment, so a request with a policy value or an
// environment is unsupported and does not reach the SDK: a sandbox must not
// run again without the policy it was asked for.
func (c *client) StartSandbox(ctx context.Context, name string, spec runtime.StartSpec) error {
	if p := spec.Policy; len(p.Base) > 0 || len(p.Protected) > 0 || len(p.Added) > 0 {
		return c.reach.Observe(unsupported(runtime.OpStartSandbox, "the SDK's start takes no policy"))
	}
	if len(spec.Env) > 0 {
		return c.reach.Observe(unsupported(runtime.OpStartSandbox, "the SDK's start takes no environment"))
	}
	return c.do(ctx, runtime.OpStartSandbox, func(ctx context.Context) error {
		_, err := c.sdk.Sandboxes().Start(ctx, c.ws, runtimeName(name))
		return err
	})
}

func (c *client) WaitReady(ctx context.Context, name string) (runtime.SandboxStatus, error) {
	return call(c, ctx, runtime.OpWaitReady, func(ctx context.Context) (runtime.SandboxStatus, error) {
		sb, err := c.sdk.Sandboxes().WaitReady(ctx, c.ws, runtimeName(name))
		if err != nil {
			return runtime.SandboxStatus{}, err
		}
		return sandboxStatus(sb), nil
	})
}

// WatchSandboxes is unsupported: the SDK watches one named sandbox, not
// every sandbox of a workspace.
func (c *client) WatchSandboxes(context.Context) (runtime.SandboxWatch, error) {
	return nil, c.reach.Observe(unsupported(runtime.OpWatchSandboxes, "the SDK watches one named sandbox, not a workspace"))
}

// RunMain is unsupported: the SDK has no call that starts a main process
// after creation.
func (c *client) RunMain(context.Context, string, []string) error {
	return c.reach.Observe(unsupported(runtime.OpRunMain, "the SDK has no call that starts a main process after creation"))
}

// gatewayName is a sandbox name the gateway takes: lowercase letters,
// digits and hyphens, at most 19 of them.
var gatewayName = regexp.MustCompile(`^[a-z0-9-]{1,19}$`)

// nameAnnotation is the annotation of a sandbox whose name the gateway does
// not take, holding that name.
const nameAnnotation = "parley.name"

// runtimeName is the gateway's name of the sandbox name: name itself when
// the gateway takes it, otherwise "p-" and 17 hex digits of its SHA-256
// (68 bits, so that the names of a workspace do not collide in practice).
// The hub names a sandbox by its id, which the gateway does not take.
func runtimeName(name string) string {
	if gatewayName.MatchString(name) {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return "p-" + hex.EncodeToString(sum[:])[:17]
}

// sandboxName is the name the sandbox sb was created with: the one its
// annotation holds, or the gateway's.
func sandboxName(sb *v1.Sandbox) string {
	if name := sb.Annotations[nameAnnotation]; name != "" {
		return name
	}
	return sb.Name
}

// sandboxStatus is a sandbox as the SDK reports it. The SDK reports nothing
// of the working directory, which is taken as present while the sandbox
// exists.
func sandboxStatus(sb *v1.Sandbox) runtime.SandboxStatus {
	st := runtime.SandboxStatus{Name: sandboxName(sb), Labels: cloneMap(sb.Labels), State: state(sb.Status), WorkDirPresent: true}
	if sb.Status.ExitCode != nil {
		st.MainExited, st.ExitCode = true, int(*sb.Status.ExitCode)
	}
	return st
}

func state(s v1.SandboxStatus) runtime.State {
	switch s.Phase {
	case v1.SandboxReady:
		return runtime.StateRunning
	case v1.SandboxStopping, v1.SandboxStopped, v1.SandboxSuspending, v1.SandboxSuspended,
		v1.SandboxCompleted, v1.SandboxError, v1.SandboxDeleting:
		return runtime.StateStopped
	}
	if s.NextRestartAtMs > 0 {
		return runtime.StateRestarting
	}
	return runtime.StatePending
}

// Exec.

// Exec runs a command and collects its output. The SDK's run takes no
// input, so a request with stdin is unsupported.
func (c *client) Exec(ctx context.Context, sandbox string, req runtime.ExecRequest) (runtime.ExecResult, error) {
	if len(req.Stdin) > 0 {
		return runtime.ExecResult{}, c.reach.Observe(unsupported(runtime.OpExec, "the SDK's run takes no input"))
	}
	return call(c, ctx, runtime.OpExec, func(ctx context.Context) (runtime.ExecResult, error) {
		r, err := c.sdk.Exec().Run(ctx, c.ws, runtimeName(sandbox), req.Command, execOptions(req))
		if err != nil {
			return runtime.ExecResult{}, err
		}
		return runtime.ExecResult{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr}, nil
	})
}

// ExecStream runs a command and delivers its output as it comes, until the
// command or the caller's context ends. The SDK's stream takes no input, so
// a request with stdin is unsupported.
func (c *client) ExecStream(ctx context.Context, sandbox string, req runtime.ExecRequest) (runtime.ExecStream, error) {
	if len(req.Stdin) > 0 {
		return nil, c.reach.Observe(unsupported(runtime.OpExecStream, "the SDK's stream takes no input"))
	}
	s, err := c.sdk.Exec().Stream(ctx, c.ws, runtimeName(sandbox), req.Command, execOptions(req))
	if err := c.result(ctx, runtime.OpExecStream, err); err != nil {
		return nil, err
	}
	return &execStream{c: c, ctx: ctx, s: s}, nil
}

func (c *client) ExecInteractive(ctx context.Context, sandbox string, req runtime.ExecRequest, size runtime.TerminalSize) (runtime.Terminal, error) {
	s, err := c.sdk.Exec().Interactive(ctx, c.ws, runtimeName(sandbox), req.Command, size.Cols, size.Rows, execOptions(req))
	if err := c.result(ctx, runtime.OpExecInteractive, err); err != nil {
		return nil, err
	}
	return &terminal{c: c, ctx: ctx, s: s}, nil
}

func execOptions(req runtime.ExecRequest) v1.ExecOptions {
	return v1.ExecOptions{Env: cloneMap(req.Env), WorkDir: req.WorkDir, NoLoginShell: true}
}

// streamError is the error of a stream that broke: io.EOF when the caller's
// context ended, otherwise an error of kind unavailable.
func (c *client) streamError(ctx context.Context, op string, err error) error {
	if err == io.EOF || ctx.Err() != nil {
		return io.EOF
	}
	_, reason := classify(ctx, err)
	return c.reach.Observe(runtime.NewError(runtime.ErrUnavailable, op, c.scrub.Scrub(reason)))
}

type execStream struct {
	c   *client
	ctx context.Context
	s   v1.ExecStream
}

func (e *execStream) Next() (runtime.Output, error) {
	ch, err := e.s.Next()
	if err != nil {
		return runtime.Output{}, e.c.streamError(e.ctx, runtime.OpExecStream, err)
	}
	stream := runtime.Stdout
	if ch.Stream == v1.StreamStderr {
		stream = runtime.Stderr
	}
	return runtime.Output{Stream: stream, Data: ch.Data}, nil
}

func (e *execStream) ExitCode() (int, error) {
	code, err := e.s.ExitCode()
	if err != nil {
		_, reason := classify(e.ctx, err)
		return 0, runtime.NewError(runtime.ErrUnavailable, runtime.OpExecStream, e.c.scrub.Scrub(reason))
	}
	return code, nil
}

func (e *execStream) Close() error { return e.s.Close() }

type terminal struct {
	c   *client
	ctx context.Context
	s   v1.InteractiveSession
}

func (t *terminal) Read(p []byte) (int, error) {
	n, err := t.s.Read(p)
	if err != nil {
		err = t.c.streamError(t.ctx, runtime.OpExecInteractive, err)
	}
	return n, err
}

func (t *terminal) Write(p []byte) (int, error) {
	n, err := t.s.Write(p)
	if err != nil {
		err = t.c.streamError(t.ctx, runtime.OpExecInteractive, err)
	}
	return n, err
}

func (t *terminal) Resize(size runtime.TerminalSize) error {
	if err := t.s.Resize(size.Cols, size.Rows); err != nil {
		return t.c.streamError(t.ctx, runtime.OpExecInteractive, err)
	}
	return nil
}

func (t *terminal) ExitCode() (int, error) {
	code, err := t.s.ExitCode()
	if err != nil {
		_, reason := classify(t.ctx, err)
		return code, runtime.NewError(runtime.ErrUnavailable, runtime.OpExecInteractive, t.c.scrub.Scrub(reason))
	}
	return code, nil
}

func (t *terminal) Close() error { return t.s.Close() }

// Files.

// Upload writes r to a temporary file with mode and uploads it, because the
// SDK uploads from a local path.
func (c *client) Upload(ctx context.Context, sandbox, path string, mode fs.FileMode, r io.Reader) error {
	dir, err := os.MkdirTemp("", "parley-upload-")
	if err != nil {
		return c.result(ctx, runtime.OpUpload, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	local := filepath.Join(dir, "file")
	f, err := os.OpenFile(local, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode.Perm())
	if err != nil {
		return c.result(ctx, runtime.OpUpload, err)
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return c.result(ctx, runtime.OpUpload, err)
	}
	return c.result(ctx, runtime.OpUpload, c.sdk.Files().Upload(ctx, c.ws, runtimeName(sandbox), local, path))
}

// Download downloads a file to a temporary file, because the SDK downloads
// to a local path, and returns it; closing it removes it.
func (c *client) Download(ctx context.Context, sandbox, path string) (io.ReadCloser, error) {
	dir, err := os.MkdirTemp("", "parley-download-")
	if err != nil {
		return nil, c.result(ctx, runtime.OpDownload, err)
	}
	local := filepath.Join(dir, "file")
	if err := c.sdk.Files().Download(ctx, c.ws, runtimeName(sandbox), path, local); err != nil {
		_ = os.RemoveAll(dir)
		return nil, c.result(ctx, runtime.OpDownload, err)
	}
	f, err := os.Open(local)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, c.result(ctx, runtime.OpDownload, err)
	}
	return &tempFile{File: f, dir: dir}, c.result(ctx, runtime.OpDownload, nil)
}

type tempFile struct {
	*os.File
	dir string
}

func (t *tempFile) Close() error {
	err := t.File.Close()
	_ = os.RemoveAll(t.dir)
	return err
}

// MakeDir is unsupported: the SDK has no call that creates a directory.
func (c *client) MakeDir(context.Context, string, string, fs.FileMode) error {
	return c.reach.Observe(unsupported(runtime.OpMakeDir, "the SDK has no call that creates a directory"))
}

// Stat is unsupported: the SDK has no call that describes a file.
func (c *client) Stat(context.Context, string, string) (runtime.FileInfo, error) {
	return runtime.FileInfo{}, c.reach.Observe(unsupported(runtime.OpStat, "the SDK has no call that describes a file"))
}

// Walk is unsupported: the SDK has no call that lists a directory.
func (c *client) Walk(context.Context, string, string) ([]runtime.FileInfo, error) {
	return nil, c.reach.Observe(unsupported(runtime.OpWalk, "the SDK has no call that lists a directory"))
}

// Logs.

// Logs is unsupported: the SDK's sandbox logs are the gateway's and the
// supervisor's log lines, which carry neither the main process's output by
// stream nor the policy log's decisions, and they cannot be followed.
func (c *client) Logs(context.Context, string, runtime.LogQuery) (runtime.LogStream, error) {
	return nil, c.reach.Observe(unsupported(runtime.OpLogs, "the SDK has no call that reads the main process's output or the policy log"))
}

func cloneMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
