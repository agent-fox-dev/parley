package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// EventExit is written when the reconciliation finds that the main process
// of a ready sandbox has ended, with its exit code as its metadata.
const EventExit = "parley.sandbox.exit"

// The reasons the reconciliation ends a sandbox with.
const (
	// ReasonExit: the main process ended with a code other than 0.
	ReasonExit = "exit"
	// ReasonRuntimeError: the runtime no longer holds the sandbox.
	ReasonRuntimeError = "runtime_error"
)

// errLost is the error of a sandbox the runtime no longer holds.
var errLost = errors.New("the runtime no longer holds the sandbox")

// Reconcile brings the records back in line with the runtime, in one pass.
// It reads the ready and provisioning records, then lists the sandboxes and
// providers the runtime holds; a runtime sandbox is a record's when it is
// labelled with the record's id or named after it. For each record, while
// it is still as read:
//
//   - a ready sandbox whose main process ended gets a checkpoint when its
//     profile asked for one, its compute released at the runtime, which keeps
//     the working directory, and its hub credential revoked, and moves to
//     completed for exit code 0 or to failed with ReasonExit for another,
//     with the exit code and EventExit by the system;
//   - a ready sandbox the runtime restarts moves to provisioning with the
//     mark that the runtime put it there, and such a sandbox moves back to
//     ready once the runtime runs it again, with no event either way and its
//     hub credential left live; a provisioning sandbox without that mark is
//     the provisioning work's and is left alone;
//   - a ready sandbox, or a provisioning one already created at the runtime,
//     that the runtime does not hold moves to failed with ReasonRuntimeError,
//     its hub credential revoked and EventFail by the system.
//
// Then it deletes every sandbox and provider labelled with the id of a
// sandbox that has no record or a deleted one, and nothing else. It ends
// with the idle pass and the expiry pass, so that what came due while the
// hub was down is done.
//
// When the runtime cannot be listed, the pass logs one line, makes no
// runtime call and changes no record but by the expiry pass, and the next
// pass tries again. The error is that of a record that could not be read or
// written, or of the expiry pass; it stops nothing.
func (s *Service) Reconcile(ctx context.Context) error {
	if !s.Enabled() || s.client == nil || s.deps.Store == nil {
		return nil
	}
	// The records are read before the listing, so that a sandbox the
	// runtime held when it was read is in the list unless it is gone.
	recs, err := ListByState(ctx, s.deps.Store.Main(), StateReady, StateProvisioning)
	if err != nil {
		return err
	}
	sandboxes, err := s.client.ListSandboxes(ctx)
	var providers []runtime.Provider
	if err == nil {
		providers, err = s.client.ListProviders(ctx)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		s.logger().Warn("the reconciliation could not list what the runtime holds; it leaves the sandboxes as they are until the next pass",
			"error", err.Error())
		return s.ExpiryPass(ctx)
	}
	held := map[string]runtime.SandboxStatus{}
	for _, sb := range sandboxes {
		held[sb.Name] = sb
		if id := sb.Labels[LabelSandbox]; id != "" {
			held[id] = sb
		}
	}
	var errs []error
	for _, rec := range recs {
		if ctx.Err() != nil {
			return nil
		}
		if err := s.reconcileOne(ctx, rec, held); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.deleteLeftovers(ctx, sandboxes, providers); err != nil {
		errs = append(errs, err)
	}
	if err := s.IdlePass(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.ExpiryPass(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// reconcileOne brings the record rec, as the pass read it, in line with
// held, the runtime's sandboxes by id and name. It takes the sandbox's turn
// among the stops and deletions and leaves a sandbox that another action
// changed since the pass read it.
func (s *Service) reconcileOne(ctx context.Context, rec Record, held map[string]runtime.SandboxStatus) error {
	st, ok := held[rec.ID]
	var due bool
	switch {
	case !ok:
		// A provisioning sandbox not yet created at the runtime is not
		// expected there.
		due = rec.State == StateReady || rec.RuntimeCreated
	case rec.State == StateReady:
		due = st.MainExited || st.State == runtime.StateRestarting
	default:
		due = rec.RuntimeProvisioning && st.State == runtime.StateRunning
	}
	if !due {
		return nil
	}
	defer s.lifecycle.lock(rec.ID)()
	cur, err := Get(ctx, s.deps.Store.Main(), rec.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case cur.State != rec.State || cur.RuntimeCreated != rec.RuntimeCreated || cur.RuntimeProvisioning != rec.RuntimeProvisioning:
		return nil
	}
	switch {
	case !ok:
		return s.failLost(ctx, cur)
	case cur.State == StateReady && st.MainExited:
		return s.recordExit(ctx, cur, st.ExitCode)
	case cur.State == StateReady:
		return s.pause(ctx, cur, StateReady, StateProvisioning)
	default:
		return s.pause(ctx, cur, StateProvisioning, StateReady)
	}
}

// recordExit ends the ready sandbox rec whose main process ended with code:
// a checkpoint when its profile asked for one, the compute released at the
// runtime, then completed or failed with ReasonExit, the exit code, the hub
// credential revoked and EventExit, in one transaction. When ctx ends
// before the runtime is asked to release the compute, it calls and writes
// nothing more.
func (s *Service) recordExit(ctx context.Context, rec Record, code int) error {
	if rec.Snapshot.Checkpoint {
		// A failure is recorded on the sandbox by Checkpoint.
		_, _ = s.Checkpoint(ctx, rec, SystemActor)
	}
	if ctx.Err() != nil {
		// The hub stops: the exit is recorded by the pass of its next start.
		return nil
	}
	if err := s.client.StopSandbox(ctx, rec.ID); err != nil && !errors.Is(err, runtime.ErrNotFound) && ctx.Err() == nil {
		// The main process has ended all the same; what the runtime still
		// runs goes with the sandbox's deletion.
		s.logger().Warn("the runtime could not release the compute of a sandbox whose main process ended", "sandbox", rec.ID,
			"error", s.ScrubSandbox(context.WithoutCancel(ctx), rec.ID, err.Error()))
	}
	to, reason := StateCompleted, ""
	if code != 0 {
		to, reason = StateFailed, ReasonExit
	}
	ev := &event.Input{Type: EventExit, ActorType: event.ActorSystem, Metadata: map[string]any{"exit_code": code}}
	return s.end(ctx, rec, to, reason, ev, SetExitCode(&code))
}

// failLost fails the sandbox rec, which the runtime no longer holds, with
// ReasonRuntimeError, its hub credential revoked and EventFail, in one
// transaction.
func (s *Service) failLost(ctx context.Context, rec Record) error {
	text := s.ScrubSandbox(ctx, rec.ID, errLost.Error())
	ev := &event.Input{Type: EventFail, ActorType: event.ActorSystem, Metadata: map[string]any{"reason": ReasonRuntimeError, "error": text}}
	return s.end(ctx, rec, StateFailed, ReasonRuntimeError, ev, SetMarker(""), SetRuntimeProvisioning(false))
}

// end moves the sandbox rec from its state to the ended state to with
// reason and ev, sets fields and revokes its hub credential, in one
// transaction; then it reports the transition and forgets the sandbox's
// values. A sandbox that left its state meanwhile is left as it is.
func (s *Service) end(ctx context.Context, rec Record, to, reason string, ev *event.Input, fields ...Field) error {
	var (
		applied bool
		report  func()
	)
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		applied, report, err = s.TransitionTx(ctx, tx, rec.ID, rec.State, to, reason, ev)
		if err != nil || !applied {
			return err
		}
		if err := Update(ctx, tx, rec.ID, fields...); err != nil {
			return err
		}
		return s.RevokeHubCredential(ctx, tx, rec)
	})
	if err != nil {
		return fmt.Errorf("end the sandbox %s: %w", rec.ID, err)
	}
	report()
	if applied {
		s.ForgetValues(rec.ID)
	}
	return nil
}

// pause moves the sandbox rec from the state from to the state to for the
// runtime's pause, with no event: to provisioning with the mark that the
// runtime put it there, back to ready with the mark cleared.
func (s *Service) pause(ctx context.Context, rec Record, from, to string) error {
	var (
		applied bool
		report  func()
	)
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		applied, report, err = s.TransitionTx(ctx, tx, rec.ID, from, to, "", nil)
		if err != nil || !applied {
			return err
		}
		return Update(ctx, tx, rec.ID, SetRuntimeProvisioning(to == StateProvisioning))
	})
	if err != nil {
		return fmt.Errorf("follow the runtime's pause of the sandbox %s: %w", rec.ID, err)
	}
	report()
	return nil
}

// deleteLeftovers deletes, of the runtime's sandboxes and providers, those
// labelled with the id of a sandbox that has no record or a deleted one,
// with everything else named after that id; what carries no label, or that
// of a live sandbox, stays. The records are read after the listing, so that
// a sandbox created since is seen with its record.
func (s *Service) deleteLeftovers(ctx context.Context, sandboxes []runtime.SandboxStatus, providers []runtime.Provider) error {
	var ids []string
	seen := map[string]bool{}
	note := func(labels map[string]string) {
		if id := labels[LabelSandbox]; id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, sb := range sandboxes {
		note(sb.Labels)
	}
	for _, pr := range providers {
		note(pr.Labels)
	}
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			return nil
		}
		rec, err := Get(ctx, s.deps.Store.Main(), id)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			errs = append(errs, err)
			continue
		case rec.State != StateDeleted:
			continue
		}
		var sbs, prs []string
		for _, sb := range sandboxes {
			if ownedBy(id, sb.Name, sb.Labels) {
				sbs = append(sbs, sb.Name)
			}
		}
		for _, pr := range providers {
			if ownedBy(id, pr.Name, pr.Labels) {
				prs = append(prs, pr.Name)
			}
		}
		if err := s.deleteAtRuntime(ctx, sbs, prs); err != nil && ctx.Err() == nil {
			// The next pass tries again.
			s.logger().Warn("the reconciliation could not delete what the runtime holds of a deleted sandbox", "sandbox", id, "error", err.Error())
		}
	}
	return errors.Join(errs...)
}

// ReconcileAtStart runs the first pass of the reconciliation, before the hub
// serves, while sandboxes are on. What the pass could not do is logged and
// left to the next pass: it never keeps the hub from starting, so it
// returns nil.
func (s *Service) ReconcileAtStart(ctx context.Context) error {
	s.reconcileLogged(ctx)
	return nil
}

// RunReconcile runs a pass of the reconciliation every
// [sandbox].reconcile_interval on the hub's clock, until ctx ends. It
// returns at once while sandboxes are off.
func (s *Service) RunReconcile(ctx context.Context) {
	if !s.Enabled() || s.client == nil || s.deps.Store == nil {
		return
	}
	for {
		t := s.newTimer(s.settings.ReconcileInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
		s.reconcileLogged(ctx)
	}
}

// reconcileLogged runs a pass and logs its error.
func (s *Service) reconcileLogged(ctx context.Context) {
	if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
		s.logger().Warn("the reconciliation did not finish its pass; the next pass tries again", "error", err.Error())
	}
}
