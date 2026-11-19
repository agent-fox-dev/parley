package sandbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// The events a provisioning ends with.
const (
	// EventReady is written with the move of a provisioned sandbox to ready.
	EventReady = "parley.sandbox.ready"
	// EventFail is written with the move of a sandbox whose provisioning
	// failed to failed.
	EventFail = "parley.sandbox.fail"
)

// The reasons a provisioning fails with, the failed sandbox's state_reason.
const (
	// ReasonCredentialMissing: a required secret no longer resolves.
	ReasonCredentialMissing = "credential_missing"
	// ReasonCheckoutFailed: a git command of the checkout failed.
	ReasonCheckoutFailed = "checkout_failed"
	// ReasonProvisionFailed: every other step failed, or the provisioning
	// did not end within ProvisionBound.
	ReasonProvisionFailed = "provision_failed"
)

const (
	// ProvisionBound is how long a provisioning may take on the hub's clock;
	// one that has not ended by then is abandoned as failed, since every
	// step calls the runtime, which may never answer.
	ProvisionBound = 10 * time.Minute
	// ProvisionInterval is the time between the provisioner's passes, which
	// find the sandboxes that were created in a caller's transaction.
	ProvisionInterval = time.Second
	// MaxProvisionings is how many sandboxes the provisioner brings up at
	// once; the others wait for a later pass.
	MaxProvisionings = 8
	// cleanupBudget is how long the clean-up and the writes that end a
	// provisioning may take, on a context of their own, so that they run
	// also after the provisioning's own context ended.
	cleanupBudget = 30 * time.Second
)

// LabelSandbox is the label the runtime's sandbox and providers of a sandbox
// carry, with the sandbox id as its value, so that what a provisioning left
// at the runtime is found again after a restart.
const LabelSandbox = "parley.sandbox"

// hubProvider is the name, after the sandbox id, of the provider of a
// sandbox's hub credential.
const hubProvider = "hub"

// providerName is the runtime's name of the provider name of the sandbox id:
// providers are shared by every sandbox of the OpenShell workspace, so the
// name starts with the sandbox's.
func providerName(id, name string) string { return id + "-" + name }

// errProvisionTimeout ends a provisioning that did not end within
// ProvisionBound.
var errProvisionTimeout = fmt.Errorf("the provisioning did not end within %s", ProvisionBound)

// Timer is a timer of the hub's clock: C delivers once when it fires, and
// Stop stops it, reporting whether it had not fired yet.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// realTimer is a Timer on the real clock.
type realTimer struct{ t *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.t.C }
func (t realTimer) Stop() bool          { return t.t.Stop() }

// newTimer returns a timer of d on the clock Deps.NewTimer gives, the real
// clock without one.
func (s *Service) newTimer(d time.Duration) Timer {
	if s.deps.NewTimer != nil {
		return s.deps.NewTimer(d)
	}
	return realTimer{time.NewTimer(d)}
}

// WorkspaceRuleProvider gives the workspace sandbox rules of a repository:
// the rules approved for it and its forks, which every sandbox created later
// on them gets on top of its profile's policy. The network-policy feature
// registers it on the service.
type WorkspaceRuleProvider interface {
	// WorkspaceRules returns the rules of the repository workspace, oldest
	// first, each with its id and body, read in tx, the transaction that
	// stores the sandbox.
	WorkspaceRules(ctx context.Context, tx *sql.Tx, workspace string) ([]WorkspaceRule, error)
}

// PolicyComposer makes the policy value a sandbox is created or started
// with. The network-policy feature registers one on the service; without
// one the default gives the snapshot's base policy with the protected rules
// (ProtectedRules) and no added rule, and a refusal's text unchanged.
type PolicyComposer interface {
	// Compose returns the policy value of the sandbox rec; an error fails
	// the provisioning before the runtime is asked to create it.
	Compose(ctx context.Context, rec Record) (runtime.Policy, error)
	// Explain returns the text a refusal of the runtime to create or start
	// the sandbox rec with its policy value is reported with.
	Explain(rec Record, refusal error) string
}

// RegisterWorkspaceRuleProvider makes p the workspace rule provider a create
// asks; nil removes it. It is safe to call while the hub serves; a create
// takes the provider once.
func (s *Service) RegisterWorkspaceRuleProvider(p WorkspaceRuleProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ruleProvider = p
}

// RegisterPolicyComposer makes c the policy composer of every provisioning;
// nil restores the default. It is safe to call while the hub serves; a
// provisioning takes the composer once.
func (s *Service) RegisterPolicyComposer(c PolicyComposer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.composer = c
}

// policyComposer is the registered policy composer, or the default.
func (s *Service) policyComposer() PolicyComposer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.composer == nil {
		return defaultComposer{s}
	}
	return s.composer
}

// workspaceRules asks the registered workspace rule provider, in tx, for
// the rules of the repository familyRoot; none without a provider.
func (s *Service) workspaceRules(ctx context.Context, tx *sql.Tx, familyRoot string) ([]WorkspaceRule, error) {
	s.mu.Lock()
	p := s.ruleProvider
	s.mu.Unlock()
	if p == nil {
		return nil, nil
	}
	rules, err := p.WorkspaceRules(ctx, tx, familyRoot)
	if err != nil {
		return nil, fmt.Errorf("the workspace sandbox rules of %s: %w", familyRoot, err)
	}
	out := make([]WorkspaceRule, len(rules))
	for i, r := range rules {
		out[i] = WorkspaceRule{ID: r.ID, Body: bytes.Clone(r.Body)}
	}
	return out, nil
}

// defaultComposer is the policy composer of a hub that registered none.
type defaultComposer struct{ s *Service }

func (c defaultComposer) Compose(ctx context.Context, rec Record) (runtime.Policy, error) {
	rules, err := c.s.ProtectedRules(ctx, rec)
	if err != nil {
		return runtime.Policy{}, err
	}
	return runtime.Policy{Base: bytes.Clone(rec.Snapshot.BasePolicy), Protected: rules}, nil
}

func (defaultComposer) Explain(_ Record, refusal error) string { return refusal.Error() }

// ruleBody is the body of a protected rule, in OpenShell's policy schema:
// the providers whose credentials the rule lets through and, for the hub
// rule, the endpoints it lets them reach. The hub writes it and does not
// otherwise interpret it.
type ruleBody struct {
	Providers []string       `json:"providers"`
	Endpoints []ruleEndpoint `json:"endpoints,omitempty"`
}

type ruleEndpoint struct {
	Scheme string   `json:"scheme"`
	Host   string   `json:"host"`
	Port   int      `json:"port"`
	Paths  []string `json:"paths"`
}

// ProtectedRules returns the rules the hub keeps in the policy of the
// sandbox rec, whatever else the policy holds: with a hub credential the hub
// rule, which lets its provider reach the API and git routes of the
// sandbox's workspace on the scheme, host and port of
// [server].external_url, and nothing else of the hub, then one rule naming
// the provider of each attached credential, in the order of the sandbox's
// credentials.
func (s *Service) ProtectedRules(ctx context.Context, rec Record) ([]runtime.Rule, error) {
	var out []runtime.Rule
	if rec.HubAccess != nil {
		r, err := s.hubRule(ctx, rec)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	for _, name := range rec.Credentials {
		out = append(out, rule("credential-"+name, ruleBody{Providers: []string{providerName(rec.ID, name)}}))
	}
	return out, nil
}

// hubRule is the protected rule of the hub credential of the sandbox rec.
func (s *Service) hubRule(ctx context.Context, rec Record) (runtime.Rule, error) {
	u, err := url.Parse(s.deps.ExternalURL)
	if err != nil || u.Host == "" {
		return runtime.Rule{}, fmt.Errorf("the hub rule of sandbox %s: [server].external_url %q is not a URL", rec.ID, s.deps.ExternalURL)
	}
	port := 443
	if u.Scheme == "http" {
		port = 80
	}
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil {
			return runtime.Rule{}, fmt.Errorf("the hub rule of sandbox %s: the port of [server].external_url: %w", rec.ID, err)
		}
	}
	org, err := s.orgSlug(ctx, rec.OrgID)
	if err != nil {
		return runtime.Rule{}, err
	}
	prefix := strings.TrimSuffix(u.Path, "/")
	api := prefix + "/api/v1/workspaces/" + rec.Workspace
	git := prefix + "/git/" + org + "/" + rec.Workspace + ".git"
	return rule("parley-hub", ruleBody{
		Providers: []string{providerName(rec.ID, hubProvider)},
		Endpoints: []ruleEndpoint{{Scheme: u.Scheme, Host: u.Hostname(), Port: port, Paths: []string{api, api + "/**", git, git + "/**"}}},
	}), nil
}

func rule(name string, body ruleBody) runtime.Rule {
	b, _ := json.Marshal(body) // a struct of strings and ints always encodes
	return runtime.Rule{Name: name, Body: b}
}

// WakeProvisioner tells the provisioner that a sandbox waits for it, without
// waiting for it to listen. A create in its own transaction calls it once it
// has committed; a caller that created a sandbox in its own transaction may
// call it after its commit, or leave the sandbox to the next pass.
func (s *Service) WakeProvisioner() {
	select {
	case s.wakeup <- struct{}{}:
	default:
	}
}

// ResumeProvisioning reports parley_sandboxes from the store and wakes the
// provisioner, so that its first pass takes up the provisionings an earlier
// process did not finish. The hub calls it in a start hook, before
// RunProvisioner starts.
func (s *Service) ResumeProvisioning(ctx context.Context) error {
	if err := s.ReportStates(ctx, s.deps.Store.Main()); err != nil {
		return err
	}
	s.WakeProvisioner()
	return nil
}

// RunProvisioner brings the sandboxes that carry the create or the start
// marker from provisioning to ready or failed, until ctx ends. It passes
// over the store when it is woken (WakeProvisioner) and every
// ProvisionInterval on the hub's clock, and starts a provisioning (see
// provisionOne) for each marked sandbox it is not already working, at most
// MaxProvisionings at once.
// When ctx ends it cancels the provisionings, which change no record then,
// and returns once they have returned: the marker makes the next start take
// them up again. It returns at once while sandboxes are off and when
// Deps.HoldProvisioning is set.
func (s *Service) RunProvisioner(ctx context.Context) {
	if s.client == nil || s.deps.Store == nil || s.deps.HoldProvisioning {
		return
	}
	p := &provisioner{s: s, busy: map[string]bool{}}
	defer p.wg.Wait()
	for {
		t := s.newTimer(ProvisionInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.wakeup:
		case <-t.C():
		}
		t.Stop()
		p.pass(ctx)
	}
}

// provisioner is the state of RunProvisioner: the sandboxes it works.
type provisioner struct {
	s  *Service
	wg sync.WaitGroup
	mu sync.Mutex
	// busy holds the ids of the sandboxes being provisioned.
	busy map[string]bool
	// waiting is set when a pass left a sandbox for want of a free place:
	// the end of a provisioning then wakes the provisioner.
	waiting bool
}

// pass starts a provisioning for each marked sandbox that is not being
// provisioned, while fewer than MaxProvisionings run.
func (p *provisioner) pass(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	p.s.dropUnstored(ctx)
	ids, err := markedSandboxes(ctx, p.s.deps.Store.Main())
	if err != nil {
		if ctx.Err() == nil {
			p.s.logger().Warn("the sandbox provisioner could not read its work", "error", err)
		}
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range ids {
		if p.busy[id] {
			continue
		}
		if len(p.busy) >= MaxProvisionings {
			p.waiting = true
			return
		}
		p.busy[id] = true
		p.wg.Go(func() {
			defer p.done(id)
			p.s.provisionOne(ctx, id)
		})
	}
}

// done ends the work on the sandbox id and wakes the provisioner when a
// sandbox waits for the place it frees.
func (p *provisioner) done(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.busy, id)
	if p.waiting {
		p.waiting = false
		p.s.WakeProvisioner()
	}
}

// markedSandboxes reads through q the ids of the sandboxes in provisioning
// with the create or the start marker, oldest first.
func markedSandboxes(ctx context.Context, q access.Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM sandboxes WHERE marker IN (?, ?) AND state = ? ORDER BY created_at, id`,
		MarkerCreate, MarkerStart, StateProvisioning)
	if err != nil {
		return nil, fmt.Errorf("list the sandboxes to provision: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list the sandboxes to provision: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// provisionOne provisions the sandbox id, when it is still in provisioning
// with the create or the start marker, under a context that ends after
// ProvisionBound on the hub's clock, and ends it: ready when every step
// succeeded, failed otherwise. A created sandbox is created at the runtime
// (steps), a started one started there again (startSteps). When parent
// ends first, as when the hub stops, it changes nothing, so that the next
// start provisions the sandbox again from the first step. A sandbox whose
// record is gone has its values forgotten.
func (s *Service) provisionOne(parent context.Context, id string) {
	rec, err := Get(parent, s.deps.Store.Main(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		// Removed meanwhile: nothing is left to provision or scrub.
		s.ForgetValues(id)
		return
	case err != nil:
		if parent.Err() == nil {
			s.logger().Warn("the sandbox provisioner could not read a sandbox", "sandbox", id, "error", err)
		}
		return
	}
	if rec.State != StateProvisioning || (rec.Marker != MarkerCreate && rec.Marker != MarkerStart) {
		return
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	bound := s.newTimer(ProvisionBound)
	defer bound.Stop()
	go func() {
		select {
		case <-bound.C():
			cancel(errProvisionTimeout)
		case <-ctx.Done():
		}
	}()
	p := &provisioning{s: s, rec: rec, client: s.client, composer: s.policyComposer(), start: rec.Marker == MarkerStart}
	if p.start {
		err = p.startSteps(ctx)
	} else {
		err = p.steps(ctx)
	}
	if err != nil && parent.Err() != nil {
		return
	}
	if err != nil && errors.Is(context.Cause(ctx), errProvisionTimeout) {
		err = &stepError{reason: ReasonProvisionFailed, err: fmt.Errorf("%w: %v", errProvisionTimeout, err)}
	}
	end, endCancel := context.WithTimeout(context.WithoutCancel(parent), cleanupBudget)
	defer endCancel()
	if err != nil {
		p.fail(end, err)
		return
	}
	p.ready(end)
}

// stepError is a failed step of a provisioning, with the state_reason it
// fails the sandbox with.
type stepError struct {
	reason string
	err    error
}

func (e *stepError) Error() string { return e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

// failed is a step that failed with reason; what names the step.
func failed(reason, what string, err error) error {
	return &stepError{reason: reason, err: fmt.Errorf("%s: %w", what, err)}
}

// provisioning is one provisioning of a sandbox.
type provisioning struct {
	s        *Service
	rec      Record
	client   runtime.Client
	composer PolicyComposer
	// values are the sandbox's secret values, once resolved.
	values Values
	// providers are the names of the providers registered for the sandbox,
	// which the runtime's sandbox is created with.
	providers []string
	// start is set for the provisioning of a start: the runtime holds the
	// sandbox, stopped, with the working directory it keeps.
	start bool
}

// steps runs the steps of the provisioning in order and returns the first
// that failed, as a *stepError. Before the first it deletes what the runtime
// holds of the sandbox: an earlier provisioning that the hub stopped in may
// have left the runtime's sandbox and its providers, which hold the secret
// values, whether or not it had marked the sandbox created.
func (p *provisioning) steps(ctx context.Context) error {
	s, id := p.s, p.rec.ID
	if err := p.clearRuntime(ctx); err != nil {
		return failed(ReasonProvisionFailed, "delete what an earlier provisioning left at the runtime", err)
	}
	if p.rec.RuntimeCreated {
		if err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return Update(ctx, tx, id, SetRuntimeCreated(false)) }); err != nil {
			return failed(ReasonProvisionFailed, "clear the mark of the runtime sandbox", err)
		}
		p.rec.RuntimeCreated = false
	}
	env, policy, err := p.configure(ctx)
	if err != nil {
		return err
	}
	_, err = p.client.CreateSandbox(ctx, runtime.SandboxSpec{
		Name: id, Image: p.rec.Image, CPU: p.rec.CPU, Memory: p.rec.Memory, GPU: p.rec.GPU, WorkDir: WorkDir,
		Env: env, Policy: policy, Providers: slices.Clone(p.providers), Labels: map[string]string{LabelSandbox: id},
	})
	switch {
	case errors.Is(err, runtime.ErrInvalid):
		// The runtime refused the sandbox or its policy value: the composer
		// knows what in its value that refers to.
		return &stepError{reason: ReasonProvisionFailed, err: errors.New(p.composer.Explain(p.rec, err))}
	case err != nil:
		return failed(ReasonProvisionFailed, "create the runtime sandbox", err)
	}
	if err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return Update(ctx, tx, id, SetRuntimeCreated(true)) }); err != nil {
		return failed(ReasonProvisionFailed, "mark the runtime sandbox created", err)
	}
	if _, err := p.client.WaitReady(ctx, id); err != nil {
		return failed(ReasonProvisionFailed, "wait for the runtime sandbox", err)
	}
	if err := p.prepare(ctx); err != nil {
		return err
	}
	if len(p.rec.Command) > 0 {
		if err := p.client.RunMain(ctx, id, p.rec.Command); err != nil {
			return failed(ReasonProvisionFailed, "start the main process", err)
		}
	}
	return nil
}

// startSteps runs the steps of the provisioning of a start in order and
// returns the first that failed, as a *stepError. The runtime holds the
// stopped sandbox with its working directory, so nothing is deleted first:
// the secrets are resolved again and their providers registered again, so
// that a value rotated while the sandbox was stopped is the one it gets,
// then the runtime starts the sandbox with the policy value of the composer
// and the environment. A working directory the runtime kept is used as it
// is; one it lost is rebuilt (rebuild). Then the main process is started
// again when the sandbox has a command.
func (p *provisioning) startSteps(ctx context.Context) error {
	id := p.rec.ID
	env, policy, err := p.configure(ctx)
	if err != nil {
		return err
	}
	err = p.client.StartSandbox(ctx, id, runtime.StartSpec{Policy: policy, Env: env})
	switch {
	case errors.Is(err, runtime.ErrInvalid):
		return &stepError{reason: ReasonProvisionFailed, err: errors.New(p.composer.Explain(p.rec, err))}
	case err != nil:
		// Unsupported included: a runtime that cannot start a sandbox
		// again fails the start.
		return failed(ReasonProvisionFailed, "start the runtime sandbox", err)
	}
	if _, err := p.client.WaitReady(ctx, id); err != nil {
		return failed(ReasonProvisionFailed, "wait for the runtime sandbox", err)
	}
	st, err := p.client.GetSandbox(ctx, id)
	if err != nil {
		return failed(ReasonProvisionFailed, "read the runtime sandbox", err)
	}
	if !st.WorkDirPresent {
		if err := p.rebuild(ctx); err != nil {
			return err
		}
	}
	if len(p.rec.Command) > 0 {
		if err := p.client.RunMain(ctx, id, p.rec.Command); err != nil {
			return failed(ReasonProvisionFailed, "start the main process", err)
		}
	}
	return nil
}

// configure readies what the runtime's sandbox is created or started with:
// it resolves the sandbox's credentials and exposed secrets, takes its hub
// credential (hubCredential), keeps both as the values its text is
// scrubbed of, registers their providers, and returns the environment and
// the policy value of the composer.
func (p *provisioning) configure(ctx context.Context) (map[string]string, runtime.Policy, error) {
	s, id := p.s, p.rec.ID
	res, err := s.Resolve(ctx, p.rec.Workspace, attachedSnapshot(p.rec))
	if err != nil {
		return nil, runtime.Policy{}, failed(ReasonProvisionFailed, "resolve the secrets", err)
	}
	if err := res.Missing(); err != nil {
		return nil, runtime.Policy{}, &stepError{reason: ReasonCredentialMissing, err: err}
	}
	p.values = res.Values()
	hub, err := p.hubCredential(ctx)
	if err != nil {
		return nil, runtime.Policy{}, err
	}
	p.values.HubCredential = hub
	s.SetValues(id, p.values)
	if err := p.registerProviders(ctx, res, hub); err != nil {
		return nil, runtime.Policy{}, err
	}
	env, err := s.environment(ctx, p.client, p.rec, res)
	if err != nil {
		return nil, runtime.Policy{}, failed(ReasonProvisionFailed, "build the environment", err)
	}
	policy, err := p.composer.Compose(ctx, p.rec)
	if err != nil {
		return nil, runtime.Policy{}, failed(ReasonProvisionFailed, "compose the policy", err)
	}
	return env, policy, nil
}

// attachedSnapshot is the part of rec's snapshot a provisioning resolves:
// the credentials and exposed secrets the create attached, required as the
// profile required them, so that a required one that no longer resolves
// fails the sandbox and an optional one is left out.
func attachedSnapshot(rec Record) Snapshot {
	var snap Snapshot
	for _, name := range rec.Credentials {
		if slices.Contains(rec.Snapshot.Credentials, name) {
			snap.Credentials = append(snap.Credentials, name)
		} else {
			snap.OptionalCredentials = append(snap.OptionalCredentials, name)
		}
	}
	for _, name := range rec.ExposedSecrets {
		if slices.Contains(rec.Snapshot.ExposeSecrets, name) {
			snap.ExposeSecrets = append(snap.ExposeSecrets, name)
		} else {
			snap.OptionalExposeSecrets = append(snap.OptionalExposeSecrets, name)
		}
	}
	return snap
}

// hubCredential returns the value of the sandbox's hub credential, "" for a
// sandbox without one. A provisioning this process did not start from the
// create, as after a restart, finds the value gone, since it is never
// stored: it revokes the stored token and mints a replacement with the
// stored scopes, paths and expiry.
func (p *provisioning) hubCredential(ctx context.Context) (string, error) {
	s, rec := p.s, p.rec
	value, ok := s.HubCredential(rec.ID)
	if ok || rec.HubAccess == nil {
		return value, nil
	}
	var pat identity.PAT
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		if err := s.RevokeHubCredential(ctx, tx, rec); err != nil {
			return err
		}
		if pat, value, err = s.MintHubCredential(ctx, tx, rec, rec.HubAccess.ExpiresAt); err != nil {
			return err
		}
		return Update(ctx, tx, rec.ID, SetHubTokenID(pat.TokenID))
	})
	if err != nil {
		return "", failed(ReasonProvisionFailed, "replace the hub credential", err)
	}
	p.rec.HubTokenID = pat.TokenID
	s.SetHubCredential(rec.ID, value)
	return value, nil
}

// clearRuntime deletes what the runtime holds of the sandbox: the sandboxes
// and providers labelled with its id or named after it (the sandbox by its
// id, each provider <id>-<name>). When the runtime cannot list them it
// deletes by name the sandbox and the providers of its attached credentials
// and its hub credential, and returns the listing's error with any other.
func (p *provisioning) clearRuntime(ctx context.Context) error {
	id := p.rec.ID
	owned := func(name string, labels map[string]string) bool {
		return labels[LabelSandbox] == id || name == id || strings.HasPrefix(name, id+"-")
	}
	var (
		errs                []error
		sandboxes, provided []string
	)
	if list, err := p.client.ListSandboxes(ctx); err != nil {
		errs = append(errs, err)
		sandboxes = []string{id}
	} else {
		for _, sb := range list {
			if owned(sb.Name, sb.Labels) {
				sandboxes = append(sandboxes, sb.Name)
			}
		}
	}
	if list, err := p.client.ListProviders(ctx); err != nil {
		errs = append(errs, err)
		for _, name := range p.rec.Credentials {
			provided = append(provided, providerName(id, name))
		}
		if p.rec.HubAccess != nil {
			provided = append(provided, providerName(id, hubProvider))
		}
	} else {
		for _, pr := range list {
			if owned(pr.Name, pr.Labels) {
				provided = append(provided, pr.Name)
			}
		}
	}
	for _, name := range sandboxes {
		if err := p.client.DeleteSandbox(ctx, name); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	for _, name := range provided {
		if err := p.client.DeleteProvider(ctx, name); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// registerProviders registers one provider for each attached credential,
// with its provider profile and the value res resolved, then one for the
// hub credential, with the hub provider profile and the value hub, each
// labelled with the sandbox id.
func (p *provisioning) registerProviders(ctx context.Context, res Resolution, hub string) error {
	rec := p.rec
	register := func(name, profile, value string) error {
		pn := providerName(rec.ID, name)
		p.providers = append(p.providers, pn)
		err := p.client.EnsureProvider(ctx, runtime.Provider{Name: pn, Profile: profile, Value: value, Labels: map[string]string{LabelSandbox: rec.ID}})
		if err != nil {
			return failed(ReasonProvisionFailed, "register the provider "+pn, err)
		}
		return nil
	}
	for _, name := range rec.Credentials {
		if err := register(name, rec.Snapshot.ProviderProfiles[name], res.Credentials[name]); err != nil {
			return err
		}
	}
	if rec.HubAccess != nil {
		return register(hubProvider, HubProviderProfile, hub)
	}
	return nil
}

// ready ends a provisioning whose steps all succeeded: in one transaction
// it moves the sandbox to ready with EventReady, by the system, stores the
// checkout with the sha the sandbox reported and clears the marker, and
// once that has committed it reports the transition and counts the create.
// A sandbox another component moved or removed meanwhile is left as it is
// (abandon).
func (p *provisioning) ready(ctx context.Context) {
	s, id := p.s, p.rec.ID
	ev := &event.Input{Type: EventReady, ActorType: event.ActorSystem, Metadata: map[string]any{"checkout": p.rec.Checkout}}
	var (
		applied bool
		report  func()
	)
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		applied, report, err = s.TransitionTx(ctx, tx, id, StateProvisioning, StateReady, "", ev)
		if errors.Is(err, ErrNotFound) {
			applied, err = false, nil
		}
		if err != nil || !applied {
			return err
		}
		if err := storeCheckout(ctx, tx, p.rec); err != nil {
			return err
		}
		return Update(ctx, tx, id, SetMarker(""))
	})
	switch {
	case err != nil:
		s.logger().Warn("the sandbox provisioner could not record a ready sandbox", "sandbox", id, "error", p.scrub(err.Error()))
	case !applied:
		p.abandon(ctx)
	default:
		report()
		if !p.start {
			s.countCreate(StateReady)
		}
	}
}

// fail ends a provisioning whose step err failed: it deletes what the
// runtime holds of the sandbox (clearRuntime), or for a start asks the
// runtime to release the sandbox's compute and keeps its working
// directory (release), then in one transaction
// moves the sandbox to failed with the step's reason and EventFail, by the
// system, with the reason and the scrubbed error, clears the marker, and
// the mark that the runtime's sandbox was created once the runtime no
// longer holds it, and revokes the hub credential. Once that has committed
// it reports the transition, counts the create and forgets the sandbox's
// values; a transaction that fails leaves the sandbox to the next pass. A
// sandbox another component moved or removed meanwhile is left as it is,
// and its hub credential is revoked all the same.
func (p *provisioning) fail(ctx context.Context, err error) {
	s, id := p.s, p.rec.ID
	reason := ReasonProvisionFailed
	var step *stepError
	if errors.As(err, &step) {
		reason = step.reason
	}
	text := p.scrub(err.Error())
	cleared := false
	if p.start {
		p.release(ctx)
	} else {
		cleared = p.clear(ctx)
	}
	ev := &event.Input{Type: EventFail, ActorType: event.ActorSystem, Metadata: map[string]any{"reason": reason, "error": text}}
	var (
		applied bool
		report  func()
	)
	terr := s.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		applied, report, err = s.TransitionTx(ctx, tx, id, StateProvisioning, StateFailed, reason, ev)
		switch {
		case errors.Is(err, ErrNotFound):
			applied = false
		case err != nil:
			return err
		default:
			var fields []Field
			if applied {
				fields = append(fields, SetMarker(""))
			}
			if cleared {
				fields = append(fields, SetRuntimeCreated(false))
			}
			if err := Update(ctx, tx, id, fields...); err != nil {
				return err
			}
		}
		return s.RevokeHubCredential(ctx, tx, p.rec)
	})
	if terr != nil {
		s.logger().Warn("the sandbox provisioner could not record a failed sandbox", "sandbox", id, "error", p.scrub(terr.Error()))
		return
	}
	report()
	if applied && !p.start {
		s.countCreate(StateFailed)
	}
	s.ForgetValues(id)
}

// abandon is the end of a provisioning whose sandbox another component
// moved out of provisioning, or removed, meanwhile: the record stays as it
// is but for the mark that the runtime's sandbox was created, which is
// cleared once the runtime no longer holds it; what the runtime holds of
// the sandbox is deleted, the hub credential it used is revoked and the
// sandbox's values are forgotten. The runtime sandbox of a start, which
// holds the working directory, is left to the component that moved it.
func (p *provisioning) abandon(ctx context.Context) {
	s, id := p.s, p.rec.ID
	cleared := !p.start && p.clear(ctx)
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if cleared {
			if err := Update(ctx, tx, id, SetRuntimeCreated(false)); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		return s.RevokeHubCredential(ctx, tx, p.rec)
	})
	if err != nil {
		s.logger().Warn("the sandbox provisioner could not revoke a hub credential", "sandbox", id, "error", p.scrub(err.Error()))
	}
	s.ForgetValues(id)
}

// clear is clearRuntime at the end of a provisioning: what cannot be
// deleted is logged and left to the runtime's reconciliation. It reports
// whether the runtime no longer holds the sandbox.
func (p *provisioning) clear(ctx context.Context) bool {
	if err := p.clearRuntime(ctx); err != nil {
		p.s.logger().Warn("the sandbox provisioner could not delete what the runtime holds of a sandbox", "sandbox", p.rec.ID, "error", p.scrub(err.Error()))
		return false
	}
	return true
}

// release asks the runtime to release the compute of the sandbox of a
// start that failed, keeping its working directory for a later start. A
// runtime that does not hold the sandbox has nothing to release; any other
// failure is logged and left to the runtime's reconciliation.
func (p *provisioning) release(ctx context.Context) {
	if err := p.client.StopSandbox(ctx, p.rec.ID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		p.s.logger().Warn("the sandbox provisioner could not stop a sandbox whose start failed", "sandbox", p.rec.ID, "error", p.scrub(err.Error()))
	}
}

// scrub is Scrub with the sandbox's values as the provisioning knows them.
func (p *provisioning) scrub(text string) string {
	v := p.values
	if v.HubCredential == "" {
		v.HubCredential, _ = p.s.HubCredential(p.rec.ID)
	}
	return Scrub(v, text)
}
