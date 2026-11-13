// Package openshell is the runtime client over the OpenShell Go SDK
// (github.com/ai-openshell/sdk-go, package openshell/v1). It is the only
// package of the hub that imports the SDK.
//
// Every operation acts in the client's OpenShell workspace, which it passes
// to every SDK call. An operation the SDK has no call for answers with an
// error of kind runtime.ErrUnsupported and is not imitated with other calls:
// ValidatePolicy, GetPolicy, WatchSandboxes, RunMain, MakeDir, Stat, Walk and
// Logs, and Exec and ExecStream with input on stdin. What the SDK cannot
// carry of a request it otherwise has a call for is listed on the operation.
package openshell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
// workspace. Closing the SDK client makes every later operation fail as
// unavailable.
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
// after the profile, whose placeholder goes into each of its variables. The
// SDK's profile endpoint is a host, a port and a protocol, which takes the
// endpoint's scheme; it has no path, so an endpoint's paths are not sent and
// the credential is authorized on every path of the endpoint's host.
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
		pp.Endpoints = append(pp.Endpoints, v1.NetworkEndpoint{Host: ep.Host, Port: uint32(ep.Port), Protocol: ep.Scheme})
	}
	for _, prog := range p.Programs {
		pp.Binaries = append(pp.Binaries, v1.NetworkBinary{Path: prog})
	}
	return v1.ProfileImportItem{Profile: pp, Source: "parley"}
}

func providerProfile(pp *v1.ProviderProfile) runtime.ProviderProfile {
	p := runtime.ProviderProfile{ID: pp.ID}
	for _, cred := range pp.Credentials {
		p.Variables = append(p.Variables, cred.EnvVars...)
	}
	for _, ep := range pp.Endpoints {
		p.Endpoints = append(p.Endpoints, runtime.Endpoint{Scheme: ep.Protocol, Host: ep.Host, Port: int(ep.Port)})
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
		_, err = c.sdk.Providers().Ensure(ctx, c.ws, &v1.Provider{
			Name:   p.Name,
			Type:   p.Profile,
			Labels: cloneMap(p.Labels),
			Spec:   v1.ProviderSpec{Credentials: map[string]string{pp.Credentials[0].Name: p.Value}},
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
		_, err := c.sdk.Sandboxes().AttachProvider(ctx, c.ws, sandbox, provider, 0)
		return err
	})
}

func (c *client) DetachProvider(ctx context.Context, sandbox, provider string) error {
	return c.do(ctx, runtime.OpDetachProvider, func(ctx context.Context) error {
		_, err := c.sdk.Sandboxes().DetachProvider(ctx, c.ws, sandbox, provider, 0)
		return err
	})
}

func (c *client) SandboxProviders(ctx context.Context, sandbox string) ([]string, error) {
	return call(c, ctx, runtime.OpSandboxProviders, func(ctx context.Context) ([]string, error) {
		list, err := c.sdk.Sandboxes().ListAllProviders(ctx, c.ws, sandbox)
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
			Name:            sandbox,
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
		slices.SortFunc(sandboxes, func(a, b *v1.Sandbox) int { return strings.Compare(a.Name, b.Name) })
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
					out = append(out, proposal(sb.Name, ch))
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
	draft, err := c.sdk.Policy().GetDraft(ctx, c.ws, sandbox)
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
		_, err = c.sdk.Policy().ApproveDraftChunk(ctx, c.ws, sandbox, id, ch.ReviewToken)
		return err
	})
}

func (c *client) RejectProposal(ctx context.Context, sandbox, id, reason string) error {
	return c.do(ctx, runtime.OpRejectProposal, func(ctx context.Context) error {
		if _, err := c.pendingChunk(ctx, runtime.OpRejectProposal, sandbox, id); err != nil {
			return err
		}
		return c.sdk.Policy().RejectDraftChunk(ctx, c.ws, sandbox, id, reason)
	})
}

// sdkPolicy is the SDK policy of a policy value: its protected and added
// rules as named network rules, nil when it has none. The SDK takes a typed
// policy, not a document, so the base document is not sent; a rule the
// SDK's network rule cannot hold is unsupported, and an added rule named
// like an earlier rule, which would replace it, is invalid.
func sdkPolicy(op string, p runtime.Policy) (*v1.SandboxPolicy, error) {
	rules := slices.Concat(p.Protected, p.Added)
	if len(rules) == 0 {
		return nil, nil
	}
	out := &v1.SandboxPolicy{NetworkPolicies: make(map[string]v1.NetworkPolicyRule, len(rules))}
	for _, rule := range rules {
		if _, dup := out.NetworkPolicies[rule.Name]; dup {
			return nil, invalid(op, "policy rule "+rule.Name+" is given twice")
		}
		r, err := networkRule(op, rule)
		if err != nil {
			return nil, err
		}
		out.NetworkPolicies[rule.Name] = r
	}
	return out, nil
}

// networkRule decodes a rule's body into the SDK's network rule, refusing
// any member the SDK's rule has no field for.
func networkRule(op string, rule runtime.Rule) (v1.NetworkPolicyRule, error) {
	var r v1.NetworkPolicyRule
	body := rule.Body
	if len(body) == 0 {
		body = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return v1.NetworkPolicyRule{}, unsupported(op, "policy rule "+rule.Name+" is not a network rule the SDK can carry: "+err.Error())
	}
	r.Name = rule.Name
	return r, nil
}

// Sandboxes.

// CreateSandbox creates the sandbox with its image, resources, environment,
// providers, labels and the rules of its policy value. The SDK has no
// working directory at creation and takes no policy document, so neither
// spec.WorkDir nor spec.Policy.Base is carried: the gateway's defaults
// apply.
func (c *client) CreateSandbox(ctx context.Context, spec runtime.SandboxSpec) (runtime.SandboxStatus, error) {
	return call(c, ctx, runtime.OpCreateSandbox, func(ctx context.Context) (runtime.SandboxStatus, error) {
		policy, err := sdkPolicy(runtime.OpCreateSandbox, spec.Policy)
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
		sb, err := c.sdk.Sandboxes().Create(ctx, c.ws, spec.Name, sp, opts...)
		if err != nil {
			return runtime.SandboxStatus{}, err
		}
		return sandboxStatus(sb), nil
	})
}

func (c *client) GetSandbox(ctx context.Context, name string) (runtime.SandboxStatus, error) {
	return call(c, ctx, runtime.OpGetSandbox, func(ctx context.Context) (runtime.SandboxStatus, error) {
		sb, err := c.sdk.Sandboxes().Get(ctx, c.ws, name)
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
		_, err := c.sdk.Sandboxes().Delete(ctx, c.ws, name)
		return err
	})
}

func (c *client) StopSandbox(ctx context.Context, name string) error {
	return c.do(ctx, runtime.OpStopSandbox, func(ctx context.Context) error {
		_, err := c.sdk.Sandboxes().Stop(ctx, c.ws, name)
		return err
	})
}

// StartSandbox starts a stopped sandbox. The SDK's start takes neither a
// policy nor an environment, so the sandbox keeps the policy and the
// environment the gateway holds for it, and spec is not carried.
func (c *client) StartSandbox(ctx context.Context, name string, _ runtime.StartSpec) error {
	return c.do(ctx, runtime.OpStartSandbox, func(ctx context.Context) error {
		_, err := c.sdk.Sandboxes().Start(ctx, c.ws, name)
		return err
	})
}

func (c *client) WaitReady(ctx context.Context, name string) (runtime.SandboxStatus, error) {
	return call(c, ctx, runtime.OpWaitReady, func(ctx context.Context) (runtime.SandboxStatus, error) {
		sb, err := c.sdk.Sandboxes().WaitReady(ctx, c.ws, name)
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

// sandboxStatus is a sandbox as the SDK reports it. The SDK reports nothing
// of the working directory, which is taken as present while the sandbox
// exists.
func sandboxStatus(sb *v1.Sandbox) runtime.SandboxStatus {
	st := runtime.SandboxStatus{Name: sb.Name, Labels: cloneMap(sb.Labels), State: state(sb.Status), WorkDirPresent: true}
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
		r, err := c.sdk.Exec().Run(ctx, c.ws, sandbox, req.Command, execOptions(req))
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
	s, err := c.sdk.Exec().Stream(ctx, c.ws, sandbox, req.Command, execOptions(req))
	if err := c.result(ctx, runtime.OpExecStream, err); err != nil {
		return nil, err
	}
	return &execStream{c: c, ctx: ctx, s: s}, nil
}

func (c *client) ExecInteractive(ctx context.Context, sandbox string, req runtime.ExecRequest, size runtime.TerminalSize) (runtime.Terminal, error) {
	s, err := c.sdk.Exec().Interactive(ctx, c.ws, sandbox, req.Command, size.Cols, size.Rows, execOptions(req))
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
	return c.result(ctx, runtime.OpUpload, c.sdk.Files().Upload(ctx, c.ws, sandbox, local, path))
}

// Download downloads a file to a temporary file, because the SDK downloads
// to a local path, and returns it; closing it removes it.
func (c *client) Download(ctx context.Context, sandbox, path string) (io.ReadCloser, error) {
	dir, err := os.MkdirTemp("", "parley-download-")
	if err != nil {
		return nil, c.result(ctx, runtime.OpDownload, err)
	}
	local := filepath.Join(dir, "file")
	if err := c.sdk.Files().Download(ctx, c.ws, sandbox, path, local); err != nil {
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
