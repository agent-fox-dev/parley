package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// unstoredGrace is how long the values a create in the fork create's
// transaction keeps in memory wait for the sandbox's record to be
// committed: the first provisioner pass after it that finds no record drops
// them, since that transaction was rolled back.
const unstoredGrace = time.Minute

// CreateSandbox creates in tx, the fork create's transaction, the sandbox
// req asks for on the new fork and returns its sandbox object as JSON: it is
// the sandbox component of the fork create (fork.SandboxCreator), which the
// hub registers with sandboxes on.
//
// The sandbox member is the body of a create on the fork (Create), in tx,
// with the fork's session and purpose where the member names none; a
// purpose taken from the fork is cut to a sandbox's 200 characters. A PAT
// must hold sandboxes:write, as on the create route, which is judged first:
// 403 insufficient_scope. The route's workspace access check is not run,
// since the caller is the owner of the fork it is creating. A refusal of the
// create is returned unchanged, with its status, error type and extra
// members, and writes nothing in tx; the fork create rolls the fork back
// with it.
//
// The provisioner is not woken: the record cannot be seen outside tx before
// the fork create commits, after which the provisioner's next pass, within
// ProvisionInterval, finds it. The values Create keeps in memory for the
// provisioning (the hub credential and the resolved secrets) are dropped
// when tx is rolled back, by the first pass unstoredGrace after the return
// that finds no record of the sandbox.
func (s *Service) CreateSandbox(ctx context.Context, tx *sql.Tx, req fork.SandboxRequest) (json.RawMessage, error) {
	p := req.Caller
	if p.Type == identity.TypePAT && !identity.Satisfies(p.Permissions, scopeWrite) {
		return nil, server.InsufficientScope("sandbox: creating a sandbox needs the scope " + scopeWrite)
	}
	rec, err := s.Create(ctx, tx, CreateInput{
		Caller: p, Workspace: req.Fork.Workspace, Body: req.Sandbox, SessionID: req.SessionID, Purpose: req.Purpose,
	})
	if err != nil {
		return nil, err
	}
	s.watchUnstored(rec.ID)
	return json.Marshal(s.object(rec))
}

// watchUnstored has the provisioner check, unstoredGrace from now, that the
// sandbox id created in a caller's transaction has a record.
func (s *Service) watchUnstored(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unstored == nil {
		s.unstored = map[string]time.Time{}
	}
	s.unstored[id] = s.deps.Now().Add(unstoredGrace)
}

// dropUnstored drops what the service keeps in memory of each watched
// sandbox whose grace has passed and that has no record, reports
// SandboxesMetric again without it, and stops watching it; one with a
// record is no longer watched either. A read that fails
// leaves the sandbox watched for the next pass. A transaction that
// commits only after the grace leaves a sandbox without its hub credential's
// value, which its provisioning then replaces as after a restart.
func (s *Service) dropUnstored(ctx context.Context) {
	now := s.deps.Now()
	var due []string
	s.mu.Lock()
	for id, at := range s.unstored {
		if !now.Before(at) {
			due = append(due, id)
		}
	}
	s.mu.Unlock()
	for _, id := range due {
		_, err := Get(ctx, s.deps.Store.Main(), id)
		switch {
		case errors.Is(err, ErrNotFound):
			s.ForgetValues(id)
			// The create reported the states counting the sandbox in the
			// transaction that was rolled back.
			s.reportStates(ctx, s.deps.Store.Main())
		case err != nil:
			continue
		}
		s.mu.Lock()
		delete(s.unstored, id)
		s.mu.Unlock()
	}
}
