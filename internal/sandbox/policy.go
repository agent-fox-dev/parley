package sandbox

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// explainTimeout bounds the runtime calls by which Explain finds what in a
// refused policy value the runtime rejects: Explain has no context of its
// own.
const explainTimeout = 30 * time.Second

// RegisterNetworkPolicy registers on s the workspace rule provider and the
// policy composer of the network-policy feature: a create records, in its
// transaction, the rules the sandbox's repository holds then, and every
// provisioning, of a create and of a start, composes the sandbox's policy
// value from its profile's base policy, the protected rules, the copies it
// recorded and the rules approved for it.
func (s *Service) RegisterNetworkPolicy() {
	s.RegisterWorkspaceRuleProvider(ruleProvider{})
	s.RegisterPolicyComposer(networkComposer{s})
}

// ruleProvider gives a create the workspace sandbox rules of the rule
// store. The value of [sandbox].allow_workspace_rules and the profile make
// no difference: the switch decides only whether rules are saved.
type ruleProvider struct{}

func (ruleProvider) WorkspaceRules(ctx context.Context, tx *sql.Tx, workspace string) ([]WorkspaceRule, error) {
	rules, err := ListRules(ctx, tx, workspace)
	if err != nil {
		return nil, err
	}
	out := make([]WorkspaceRule, len(rules))
	for i, r := range rules {
		out[i] = WorkspaceRule{ID: r.ID, Body: r.Rule}
	}
	return out, nil
}

// ComposePolicy returns the policy value of the sandbox rec: the base policy
// of its profile snapshot; the protected rules (ProtectedRules), unchanged;
// and the added rules, which are the copies of the workspace sandbox rules
// it recorded at its creation, in their order and each under its id, then
// the rules approved for it in the order they were decided, each under its
// proposal's id, but for one identical (CanonicalRule) to a recorded copy,
// which the copy already grants. A restored sandbox is composed like any
// other: the rules approved for its source are not its own.
func (s *Service) ComposePolicy(ctx context.Context, rec Record) (runtime.Policy, error) {
	p, copies, err := s.policyParts(ctx, rec)
	if err != nil {
		return runtime.Policy{}, err
	}
	p.Added = append(copies, p.Added...)
	return p, nil
}

// policyParts returns the policy value of rec without its workspace rules,
// and the copies of those, in their order.
func (s *Service) policyParts(ctx context.Context, rec Record) (runtime.Policy, []runtime.Rule, error) {
	protected, err := s.ProtectedRules(ctx, rec)
	if err != nil {
		return runtime.Policy{}, nil, err
	}
	var copies []runtime.Rule
	recorded := map[string]bool{}
	for _, r := range rec.WorkspaceRules {
		copies = append(copies, runtime.Rule{Name: r.ID, Body: bytes.Clone(r.Body)})
		if c, err := CanonicalRule(r.Body); err == nil {
			recorded[c] = true
		}
	}
	approved, err := ApprovedRules(ctx, s.deps.Store.Main(), rec.ID)
	if err != nil {
		return runtime.Policy{}, nil, err
	}
	var added []runtime.Rule
	for _, a := range approved {
		if c, err := CanonicalRule(a.Rule); err == nil && recorded[c] {
			continue
		}
		added = append(added, runtime.Rule{Name: a.ProposalID, Body: a.Rule})
	}
	return runtime.Policy{Base: bytes.Clone(rec.Snapshot.BasePolicy), Protected: protected, Added: added}, copies, nil
}

// networkComposer is the policy composer of the network-policy feature.
type networkComposer struct{ s *Service }

// Compose is ComposePolicy, after a check of the base policy of a profile
// with a policy file: one that refers to the provider of the sandbox's hub
// credential fails the provisioning before the runtime is asked to create
// the sandbox, since only the hub rule may let that provider through.
func (c networkComposer) Compose(ctx context.Context, rec Record) (runtime.Policy, error) {
	if rec.Snapshot.BasePolicy != nil {
		if err := c.s.checkBasePolicy(ctx, rec); err != nil {
			return runtime.Policy{}, err
		}
	}
	return c.s.ComposePolicy(ctx, rec)
}

// checkBasePolicy asks the runtime which providers the base policy of rec's
// profile refers to, and refuses one that names rec's hub credential's.
func (s *Service) checkBasePolicy(ctx context.Context, rec Record) error {
	client := s.Client()
	if client == nil {
		return errors.New("sandboxes are off")
	}
	v, err := client.ValidatePolicy(ctx, runtime.Policy{Base: rec.Snapshot.BasePolicy})
	if err != nil {
		return fmt.Errorf("validate the base policy of profile %q: %w", rec.Profile, err)
	}
	if hub := providerName(rec.ID, hubProvider); slices.Contains(v.Providers, hub) {
		return fmt.Errorf("the base policy of profile %q refers to the provider %s of the sandbox's hub credential, which only the hub rule may grant", rec.Profile, hub)
	}
	return nil
}

// Explain names what the runtime rejected in the policy value of rec: the
// profile, when the runtime rejects its base policy with the protected
// rules alone; else the first workspace sandbox rule with which the policy
// is rejected, found by adding the recorded copies one at a time, in their
// order; else the first rule approved for the sandbox with which it is
// rejected, found by adding those one at a time, in their order, after
// every copy. The text keeps the runtime's reason, scrubbed of the
// sandbox's values and cut to 1,024 bytes.
func (c networkComposer) Explain(rec Record, refusal error) string {
	ctx, cancel := context.WithTimeout(context.Background(), explainTimeout)
	defer cancel()
	return c.s.ScrubSandbox(ctx, rec.ID, c.s.explain(ctx, rec, refusal))
}

func (s *Service) explain(ctx context.Context, rec Record, refusal error) string {
	client := s.Client()
	if client == nil || !errors.Is(refusal, runtime.ErrInvalid) {
		return refusal.Error()
	}
	without, copies, err := s.policyParts(ctx, rec)
	if err != nil {
		return refusal.Error()
	}
	approved := without.Added
	rejects := func(added []runtime.Rule) bool {
		p := without
		p.Added = added
		_, err := client.ValidatePolicy(ctx, p)
		return errors.Is(err, runtime.ErrInvalid)
	}
	if rejects(nil) {
		return fmt.Sprintf("the runtime rejects the policy of profile %q with the protected rules alone: %s", rec.Profile, refusal)
	}
	for i, r := range copies {
		if rejects(slices.Clone(copies[:i+1])) {
			return fmt.Sprintf("the runtime rejects the policy with workspace sandbox rule %s: %s", r.Name, refusal)
		}
	}
	for i, r := range approved {
		if rejects(append(slices.Clone(copies), approved[:i+1]...)) {
			return fmt.Sprintf("the runtime rejects the policy with the rule approved by proposal %s: %s", r.Name, refusal)
		}
	}
	return refusal.Error()
}
