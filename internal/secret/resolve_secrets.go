package secret

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/agent-fox-dev/parley/internal/access"
)

// ResolvedSecret is the answer for one name a resolution was asked: the
// value of the first step of the chain that holds a secret of that name
// ignoring case, and that step's origin, or not found.
type ResolvedSecret struct {
	// Name is the name as it was first asked.
	Name string
	// Found reports whether a step holds the name; a secret whose value is
	// empty is found.
	Found bool
	// Value is the secret's value; "" when not found.
	Value string
	// Origin is the step the value was found in; "" when not found.
	Origin Origin
}

// SecretResolution is what ResolveSecrets answers: one entry per distinct
// name, in the order first asked, and the steps searched, in chain order.
type SecretResolution struct {
	Secrets  []ResolvedSecret
	Searched []Step
}

// secretReader reads one secret of one scope by name, as Get does.
type secretReader func(ctx context.Context, scope Scope, scopeID, name string) (string, bool, error)

// reservedNames are the secret names the hub itself reads from a
// workspace's own scope for its origin, upstream and forge credentials,
// lowered. A fork never takes them from its parent, whoever owns it.
var reservedNames = map[string]bool{
	"git_pat":               true,
	"git_username":          true,
	"git_password":          true,
	"upstream_git_pat":      true,
	"upstream_git_username": true,
	"upstream_git_password": true,
	"forge_webhook_secret":  true,
}

// errWriteAccessNotBound is returned by ResolveSecrets for a fork before the
// component is bound to the question of write access.
var errWriteAccessNotBound = errors.New("the secret component is not bound to the write access question")

// ResolveSecrets returns, for each distinct name in names compared ignoring
// ASCII case, in the order first asked and spelled as first asked, the
// secret of the first step of the workspace slug's Chain that holds it,
// with that step's origin, or not found; beside them the steps searched.
// For a task fork, the parent's step is searched only while the fork's
// owner, never the caller, has write access to the parent: otherwise it is
// left out before any read, so that a fork of someone who cannot write to
// the parent never receives what belongs to it. The reserved names are
// never read from the parent, whoever owns the fork. The chain, the write
// access and every value are read again on each call; nothing is kept.
//
// A step that holds a name but cannot be read or decrypted fails the whole
// call, with no lower step read for that name: a credential that cannot be
// opened is not one that is missing. The error names the key and the scope
// and never a value. A slug with no record is ErrWorkspaceNotFound; any
// other failure is another error. An error returns no value.
func (c *Component) ResolveSecrets(ctx context.Context, slug string, names []string) (SecretResolution, error) {
	return c.resolveSecrets(ctx, c.deps.Records, slug, names)
}

// ResolveSecretsIn is ResolveSecrets with the workspace records of the
// chain read through q, so that a record written in a transaction that has
// not committed yet is seen in that transaction: the fork create asks for
// the sandbox of a fork it has not committed. The memberships, the users,
// the write access to the parent and the secrets are read as ResolveSecrets
// reads them; the fork create writes none of them.
func (c *Component) ResolveSecretsIn(ctx context.Context, q access.Querier, slug string, names []string) (SecretResolution, error) {
	return c.resolveSecrets(ctx, access.StoreRecords(q), slug, names)
}

// resolveSecrets is ResolveSecrets with the chain's records read from
// records.
func (c *Component) resolveSecrets(ctx context.Context, records access.RecordReader, slug string, names []string) (SecretResolution, error) {
	rec, steps, err := c.chain(ctx, records, slug)
	if err != nil {
		return SecretResolution{}, err
	}
	steps, err = c.dropUnwritableParent(ctx, rec.OwnerID, steps)
	if err != nil {
		return SecretResolution{}, fmt.Errorf("resolve the secrets of %s: %w", slug, err)
	}
	read := c.Get
	if r := c.readSecret.Load(); r != nil {
		read = *r
	}
	out := SecretResolution{Secrets: []ResolvedSecret{}, Searched: steps}
	asked := make(map[string]bool, len(names))
	for _, name := range names {
		lower := asciiLower(name)
		if asked[lower] {
			continue
		}
		asked[lower] = true
		answer := ResolvedSecret{Name: name}
		for _, s := range steps {
			if s.Origin == OriginParent && reservedNames[lower] {
				continue
			}
			value, found, err := read(ctx, s.Scope, s.ScopeID, name)
			if err != nil {
				return SecretResolution{}, fmt.Errorf("resolve the secrets of %s, %s scope %s: %w", slug, s.Scope, s.ScopeID, err)
			}
			if found {
				answer = ResolvedSecret{Name: name, Found: true, Value: value, Origin: s.Origin}
				break
			}
		}
		out.Secrets = append(out.Secrets, answer)
	}
	return out, nil
}

// dropUnwritableParent returns steps without the parent's step unless the
// fork's owner has write access to the parent.
func (c *Component) dropUnwritableParent(ctx context.Context, ownerID string, steps []Step) ([]Step, error) {
	i := slices.IndexFunc(steps, func(s Step) bool { return s.Origin == OriginParent })
	if i < 0 {
		return steps, nil
	}
	if c.deps.CanWrite == nil {
		return nil, errWriteAccessNotBound
	}
	ok, err := c.deps.CanWrite(ctx, ownerID, steps[i].ScopeID)
	if err != nil {
		return nil, fmt.Errorf("ask whether the fork's owner can write to %s: %w", steps[i].ScopeID, err)
	}
	if ok {
		return steps, nil
	}
	return slices.Delete(steps, i, i+1), nil
}
