package land

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/server"
)

// run runs the running attempt a to its final status: it asks the policy
// gate about each change it covers, once, replays the changes onto the
// head of the target (replay.go), and moves the target to the result and
// records the land as one step (commit.go). Every final status is written
// by finish.
//
// A target that moved between the replay and the compare-and-swap has the
// changes built again on its new head, the target guards asked again about
// the head built, and the swap tried again, each try counted in attempts,
// until Deps.MaxAttempts tries lost, which ends the attempt failed
// contention.
//
// An error that leaves the attempt without a final status is returned:
// one after the target moved leaves it running, since only the record of
// the land may end it then.
func (c *Component) run(ctx context.Context, a Attempt) error {
	ws, found, err := c.deps.Workspaces.Workspace(ctx, c.deps.Store.Main(), a.Workspace)
	if err == nil && !found {
		err = fmt.Errorf("workspace %s not found", a.Workspace)
	}
	if err != nil {
		return c.fail(ctx, a, err)
	}
	repo, err := c.deps.Repos(ctx, a.Workspace)
	if err != nil {
		return c.fail(ctx, a, err)
	}
	revision, blocked, overridden, err := c.judge(ctx, a)
	if err != nil {
		return c.fail(ctx, a, err)
	}
	a.PolicyRevision = &revision
	if overridden {
		a.Override = a.RequestedOverride
	}
	if blocked != nil {
		a.Status, a.Blocked = StatusBlocked, blocked
		return c.end(ctx, a)
	}
	// judged is the head the target guards last allowed the move to.
	for judged := ""; ; {
		a.Attempts++
		onto, found, err := repo.ResolveBranch(ctx, a.TargetBranch)
		if err == nil && !found {
			// A land never creates its target: a branch deleted since the
			// change was opened ends the attempt.
			err = fmt.Errorf("target branch %s of %s not found", a.TargetBranch, a.Workspace)
		}
		if err != nil {
			return c.fail(ctx, a, err)
		}
		b, report, err := c.build(ctx, repo, a, onto)
		switch {
		case err != nil:
			return c.fail(ctx, a, err)
		case report != nil:
			a.Status, a.Conflict = StatusConflict, report
			return c.end(ctx, a)
		}
		if b.head != judged {
			// The target guards judge every head the land would swap in:
			// a guard may act on the head it allows, as carry-patch push
			// control forwards it to origin, so a head rebuilt on a target
			// that moved since is a move they were not asked about.
			stop, err := c.judgeTarget(ctx, a, b)
			if err != nil {
				return c.fail(ctx, a, err)
			}
			if stop != nil {
				return c.end(ctx, *stop)
			}
			judged = b.head
		}
		if c.seams.beforeSwap != nil {
			c.seams.beforeSwap(ctx, a.ID, a.Attempts)
		}
		moved, err := c.commit(ctx, repo, ws, a, b)
		switch {
		case errors.Is(err, errStopped):
			return err
		case errors.Is(err, errTargetMoved):
			// Another writer moved the target since it was read: the
			// changes are built again on its new head, up to the limit.
			if a.Attempts >= c.deps.MaxAttempts {
				a.Status, a.Error = StatusFailed, new(ErrorContention)
				return c.end(ctx, a)
			}
		case err != nil && !moved:
			return c.fail(ctx, a, err)
		default:
			return err
		}
	}
}

// judge asks the policy gate about each change a covers, once and bottom
// first, and returns the revision of the policy that answered, the
// highest when the answers differ, the first change whose answer was not
// ready, nil when every change may land, and whether the request's
// override let the answer of a change pass. An answer that is not ready
// on a request with an override is put to the override rule, and one the
// rule lets pass counts as ready. The run asks it before its first try
// only, so that a retry of the compare-and-swap judges nothing again. A
// gate or rule that fails or panics is the error.
func (c *Component) judge(ctx context.Context, a Attempt) (revision int64, blocked *Blocked, overridden bool, err error) {
	gate, rule := c.policy()
	caller := callerOf(a)
	for _, cov := range a.Covered {
		ref := change.Ref(a.Workspace, cov.Number)
		ch, err := c.deps.Changes.ReadChange(ctx, nil, a.Workspace, cov.Number)
		if err != nil {
			return 0, nil, false, err
		}
		answer, err := recovered("the policy gate", func() (GateAnswer, error) {
			return gate.Evaluate(ctx, GateInput{Workspace: a.Workspace, Change: ch, Version: cov.Version,
				Head: cov.Head, Base: cov.Base, Caller: caller})
		})
		if err != nil {
			return 0, nil, false, fmt.Errorf("the policy gate on %s: %w", ref, err)
		}
		revision = max(revision, answer.Revision)
		if answer.Status == change.LandingReady {
			continue
		}
		if a.RequestedOverride != nil {
			passes, err := recovered("the override rule", func() (bool, error) {
				return rule.Passes(ctx, *a.RequestedOverride, answer)
			})
			if err != nil {
				return 0, nil, false, fmt.Errorf("the override rule on %s: %w", ref, err)
			}
			if passes {
				overridden = true
				continue
			}
		}
		return revision, &Blocked{Change: cov.Number, Status: answer.Status, Requirements: answer.Requirements,
			reason: answer.Reason}, overridden, nil
	}
	return revision, nil, overridden, nil
}

// judgeTarget asks each target guard about the move of the target the
// attempt a would make with what b built, and returns a ended as the first
// guard that does not allow it says: blocked, the guard's reason the
// reason of its parley.change.land_fail event, for a refusal, and failed
// origin_rejected for a rejection by origin. A guard that cannot decide,
// or panics, is the error; nil and nil let the land go on.
func (c *Component) judgeTarget(ctx context.Context, a Attempt, b built) (*Attempt, error) {
	in := TargetInput{Workspace: a.Workspace, Branch: a.TargetBranch, Old: b.onto, New: b.head}
	for _, g := range c.registrations().targetGuards {
		answer, err := askTarget(ctx, g, in)
		switch {
		case err != nil:
			return nil, fmt.Errorf("a target guard on %s of %s: %w", a.TargetBranch, a.Workspace, err)
		case answer.Refuse != "":
			reason := answer.Refuse
			a.Status = StatusBlocked
			a.Blocked = &Blocked{Change: a.Covered[len(a.Covered)-1].Number, Status: change.LandingBlocked, reason: &reason}
			return &a, nil
		case answer.OriginRejected != "":
			c.deps.Logger.Warn("land attempt: origin rejected the land", "workspace", a.Workspace, "land_id", a.ID,
				"message", answer.OriginRejected)
			a.Status, a.Error = StatusFailed, new(ErrorOriginRejected)
			return &a, nil
		}
	}
	return nil, nil
}

// askTarget asks the target guard g about in, a panic of the guard as its
// error.
func askTarget(ctx context.Context, g TargetGuard, in TargetInput) (answer TargetAnswer, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = errGuardPanicked(p)
		}
	}()
	return g.Check(ctx, in)
}

// recovered is what fn answers, a panic of it, the registered collaborator
// what, as its error: a gate or rule that panics fails the attempt, not
// the hub.
func recovered[T any](what string, fn func() (T, error)) (v T, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%s panicked: %v", what, p)
		}
	}()
	return fn()
}

// callerOf is who asked for the attempt a, as a change names its caller.
func callerOf(a Attempt) change.Caller {
	caller := change.Caller{Credential: change.Credential{Type: a.RequestedBy.ActorType, ID: a.CredentialID}}
	if a.RequestedBy.ActorID != nil {
		caller.UserID = *a.RequestedBy.ActorID
	}
	if a.RequestedBy.Username != nil {
		caller.Username = *a.RequestedBy.Username
	}
	return caller
}

// fail ends the attempt a failed, as internal, for the reason err, which
// it returns with any error of its own.
func (c *Component) fail(ctx context.Context, a Attempt, err error) error {
	a.Status, a.Error = StatusFailed, new(ErrorInternal)
	return errors.Join(err, c.end(ctx, a))
}

// end writes the final status a holds, in a store transaction of its own,
// and then tells the final observers of it. An attempt whose context ended
// by its deadline ends failed timeout whatever a holds, once: the pool may
// have ended it so already, and told of it then. One whose context ended
// by the stop of the hub is not ended: it stays running for the next start
// to run it again.
func (c *Component) end(ctx context.Context, a Attempt) error {
	timedOut := false
	if ctx.Err() != nil {
		cause := context.Cause(ctx)
		if !errors.Is(cause, errTimedOut) {
			return fmt.Errorf("land attempt %s left running: %w", a.ID, cause)
		}
		timedOut = true
		a.Status, a.Error, a.FinishedAt = StatusFailed, new(ErrorTimeout), nil
		a.OntoSHA, a.LandedSHA, a.Noop, a.Commits, a.Conflict, a.Blocked = nil, nil, false, nil, nil, nil
	}
	ctx = context.WithoutCancel(ctx)
	if a.FinishedAt == nil {
		a.FinishedAt = new(c.now())
	}
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return c.finish(ctx, tx, a) })
	if err == nil {
		c.concluded(ctx, Workspace{}, a)
	}
	if timedOut && errors.Is(err, errFinal) {
		return nil
	}
	return err
}

// finish writes, in tx, the final status of the attempt a with its result
// members, finished now unless a says when. It is the one place an attempt
// becomes final.
//
// The attempt's entry in the land journal goes with it: a final attempt
// has nothing left to complete at the next start. An attempt that did not
// land is told in the same transaction (stopped).
func (c *Component) finish(ctx context.Context, tx *sql.Tx, a Attempt) error {
	if a.FinishedAt == nil {
		a.FinishedAt = new(c.now())
	}
	if err := finishAttempt(ctx, tx, a); err != nil {
		return err
	}
	if err := unjournal(ctx, tx, a.ID); err != nil {
		return err
	}
	if a.Status == StatusLanded {
		return nil
	}
	return c.stopped(ctx, tx, a)
}

// stopped writes, in tx, what an attempt a that ended without landing
// leaves: one parley.change.land_fail event on the change that stopped
// it, the one its conflict report or its block names, else the top
// change, with the reason: conflict, the gate's reason or policy when it
// gave none, or the error of a failure. A conflict is also the
// mergeability of that change, with the same report, so that the change
// reads the conflict the land found.
func (c *Component) stopped(ctx context.Context, tx *sql.Tx, a Attempt) error {
	if len(a.Covered) == 0 {
		return nil
	}
	by := a.Covered[len(a.Covered)-1]
	meta := map[string]any{"land_id": a.ID, "status": a.Status}
	switch {
	case a.Status == StatusConflict && a.Conflict != nil:
		if n, ok := a.Conflict.Change.(int64); ok {
			by = a.covering(n, by)
		}
		files := make([]string, len(a.Conflict.Files))
		for i, f := range a.Conflict.Files {
			files[i] = f.Path
		}
		meta["reason"] = "conflict"
		event.PathList(meta, "files", files)
		if err := c.deps.Changes.StoreMergeability(ctx, tx, a.Workspace, by.Number, by.Head, a.Conflict.TargetSHA, a.Conflict); err != nil {
			return err
		}
	case a.Status == StatusBlocked && a.Blocked != nil:
		by, meta["reason"] = a.covering(a.Blocked.Change, by), "policy"
		if a.Blocked.reason != nil {
			meta["reason"] = *a.Blocked.reason
		}
	case a.Error != nil:
		meta["reason"] = *a.Error
	}
	return c.emit(ctx, tx, a, EventLandFail, by.Number, meta)
}

// covering is the change number of those a covers, or, for a number it
// does not cover, otherwise.
func (a Attempt) covering(number int64, otherwise CoveredChange) CoveredChange {
	for _, cov := range a.Covered {
		if cov.Number == number {
			return cov
		}
	}
	return otherwise
}

// emit writes, in tx, the land event typ of the attempt a on the change
// number, by whoever asked for the land, in the session they named, with
// meta and the change's number and ref. The events of an automatic land
// are by system: the hub started it, while the attempt's requested_by
// keeps who set the flag, whose permissions it ran with.
func (c *Component) emit(ctx context.Context, tx *sql.Tx, a Attempt, typ string, number int64, meta map[string]any) error {
	meta["number"], meta["ref"] = number, change.Ref(a.Workspace, number)
	actorType, actorID := a.RequestedBy.ActorType, a.RequestedBy.ActorID
	if a.Trigger == TriggerAutoLand {
		actorType, actorID = event.ActorSystem, nil
	}
	_, err := c.deps.Events.Emit(ctx, tx, event.Input{
		Type: typ, Workspace: &a.Workspace, ActorType: actorType, ActorID: actorID,
		ResourceType: "change", ResourceID: strconv.FormatInt(number, 10), SessionID: a.SessionID, Metadata: meta,
	})
	return err
}

// fault is asked at each point of the atomic step whether the engine is
// to stop there, as a stop of the process would stop it; it is nil but in
// tests.
func (c *Component) fault(point string) error {
	if c.seams.fault == nil {
		return nil
	}
	if err := c.seams.fault(point); err != nil {
		return fmt.Errorf("%w %s: %w", errStopped, point, err)
	}
	return nil
}

// errStopped is the error of an engine stopped at a point of the atomic
// step: it writes nothing more, as a hub process that stops there.
var errStopped = errors.New("stopped")

// now is the component's clock, in UTC.
func (c *Component) now() time.Time { return c.deps.Now().UTC() }

// StartInput is a land to start: the change, how it was asked for and by
// whom.
type StartInput struct {
	// Workspace is the slug of the workspace whose change Number is to
	// land on its target branch; Stack lands it with every change below
	// it that is not landed.
	Workspace string
	Number    int64
	Stack     bool
	// ExpectedVersion is the version the change must be at, nil for any.
	ExpectedVersion *int64
	// Trigger is TriggerManual or TriggerAutoLand.
	Trigger string
	// RequestedBy is who the attempt shows as having asked for it, and
	// the actor of its events. Caller is who it runs as: the session
	// linker and the policy gate judge it, and its credential is stored
	// with the attempt.
	RequestedBy change.Actor
	Caller      change.Caller
	// SessionID is the session the land is asked in, nil for none.
	SessionID *string
	// Override is the override the land is asked with, nil for none.
	Override *Override

	// judged are the changes, each at its version, that the land route
	// judged before the attempt is created, nil when nothing judged them.
	judged []CoveredChange
}

// Start starts the land in, as the land route does once its caller is
// admitted, for a land no request asks for, such as the automatic landing
// of a change: it checks that the workspace takes a land and the session,
// creates the running attempt and hands it to the pool, which runs it to
// its end, once the land's preconditions hold (admit). It asks no
// request guard. The attempt returned is as it was created.
func (c *Component) Start(ctx context.Context, in StartInput) (Attempt, error) {
	ws, err := c.workspace(ctx, in.Workspace)
	if err != nil {
		return Attempt{}, err
	}
	if ws.Ready != nil {
		return Attempt{}, ws.Ready
	}
	session, err := c.session(ctx, in)
	if err != nil {
		return Attempt{}, err
	}
	return c.create(ctx, ws, in, session)
}

// workspace is the workspace record slug, 404 not_found for none.
func (c *Component) workspace(ctx context.Context, slug string) (Workspace, error) {
	ws, found, err := c.deps.Workspaces.Workspace(ctx, c.deps.Store.Main(), slug)
	if err == nil && !found {
		err = server.NotFound(fmt.Sprintf("no workspace %q", slug))
	}
	return ws, err
}

// linked is a session the session linker accepted, with the linker that
// records the land's link to it.
type linked struct {
	id     string
	linker SessionLinker
}

// session is the session the land in is asked in, as the session linker
// accepts it; nil for none. A session the linker refuses, and every
// session while none is registered, is 400 unknown_session.
func (c *Component) session(ctx context.Context, in StartInput) (*linked, error) {
	if in.SessionID == nil {
		return nil, nil
	}
	unknown := server.NewError("unknown_session", http.StatusBadRequest, fmt.Sprintf("session_id: no session %q", *in.SessionID))
	linker := c.sessionLinker()
	if linker == nil {
		return nil, unknown
	}
	id, err := linker.Check(ctx, in.Caller, *in.SessionID, in.Workspace)
	switch {
	case errors.Is(err, change.ErrUnknownSession):
		return nil, unknown
	case err != nil:
		return nil, err
	}
	return &linked{id, linker}, nil
}

// create creates the running attempt of the land in on the workspace ws,
// in the session (nil for none), and hands it to the pool. The attempt is
// created in one store transaction from what that transaction reads of
// its changes, each at its current version, with the session's link to
// it, once the land's preconditions hold there (admit), so that what is
// judged of the changes holds for the attempt stored, and of simultaneous
// lands of one change exactly one creates an attempt; the strategy is the
// workspace's land.strategy as read before. A land whose changes the route
// judged before (in.judged) is refused 409 version_mismatch when the
// transaction reads other changes, or other versions of them.
// Nothing is held beyond that transaction: the attempts of different
// changes run side by side, also on one target, and meet only in the
// compare-and-swap of the target.
func (c *Component) create(ctx context.Context, ws Workspace, in StartInput, session *linked) (Attempt, error) {
	a := Attempt{
		ID: uuid.New().String(), Workspace: ws.Slug, Status: StatusRunning, Trigger: in.Trigger, Strategy: ws.Strategy,
		RequestedBy: in.RequestedBy, CredentialID: in.Caller.Credential.ID, RequestedOverride: in.Override,
	}
	if a.Strategy == "" {
		a.Strategy = StrategyRebase
	}
	if session != nil {
		a.SessionID = &session.id
	}
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		ch, err := c.deps.Changes.ReadChange(ctx, tx, ws.Slug, in.Number)
		if err != nil {
			return err
		}
		chain, err := c.admit(ctx, tx, ch, in.ExpectedVersion, in.Stack, "")
		if err != nil {
			return err
		}
		a.TargetBranch, a.Covered, a.CreatedAt = ch.TargetBranch, covered(chain), c.now()
		if in.judged != nil && !sameVersions(in.judged, a.Covered) {
			// Who may land, the confinement of a bound token and the request
			// guards judged the changes as read before this transaction: a
			// land of other changes, or of other versions, was not judged.
			return server.NewError("version_mismatch", http.StatusConflict, fmt.Sprintf(
				"the changes of the land of %s moved on while it was judged: ask again", ch.Ref))
		}
		if err := insertAttempt(ctx, tx, a); err != nil {
			return err
		}
		if session != nil {
			return session.linker.Record(ctx, tx, session.id, ws.Slug, a.ID)
		}
		return nil
	})
	if err != nil {
		return Attempt{}, err
	}
	c.submit(a, a.CreatedAt)
	return a, nil
}

// sameVersions reports whether a and b are the same changes, each at the
// same version, in the same order.
func sameVersions(a, b []CoveredChange) bool {
	return slices.EqualFunc(a, b, func(x, y CoveredChange) bool { return x.Number == y.Number && x.Version == y.Version })
}

// admit checks the preconditions of a land of the change ch, as read
// through q, and returns the changes the land covers, bottom first
// (collect). The first that fails is the answer, in this order: every
// covered change is open (409 change_not_open); expected, when not nil, is
// ch's current version (409 version_mismatch); no running attempt other
// than self covers a covered change (409 land_in_progress, with that
// attempt as the error's details); and, unless stack, every change down
// ch's depends_on is landed (409 depends_on_unlanded).
//
// A land request asks it in the transaction that creates the attempt, with
// self "", so that the running attempt it finds none of is not created by
// a simultaneous request in between; the run of an attempt again after a
// restart asks it with self the attempt's id.
func (c *Component) admit(ctx context.Context, q Querier, ch change.Change, expected *int64, stack bool, self string) ([]change.Change, error) {
	below, err := c.collect(ctx, q, ch, true)
	if err != nil {
		return nil, err
	}
	chain := below
	if !stack {
		chain = below[len(below)-1:]
	}
	for _, cov := range slices.Backward(chain) {
		if cov.State != change.StateOpen {
			return nil, server.NewError("change_not_open", http.StatusConflict,
				fmt.Sprintf("%s is %s: only an open change lands", cov.Ref, cov.State))
		}
	}
	if expected != nil && *expected != ch.Version {
		return nil, server.NewError("version_mismatch", http.StatusConflict,
			fmt.Sprintf("%s is at version %d, not at the expected version %d", ch.Ref, ch.Version, *expected))
	}
	numbers := make([]int64, len(chain))
	for i, cov := range chain {
		numbers[i] = cov.Number
	}
	running, found, err := runningCovering(ctx, q, ch.WorkspaceSlug, numbers, self)
	if err != nil {
		return nil, err
	}
	if found {
		return nil, server.NewError("land_in_progress", http.StatusConflict,
			fmt.Sprintf("the land attempt %s of %s is running", running.ID, ch.Ref)).WithDetails(running)
	}
	if !stack && len(below) > 1 {
		return nil, server.NewError("depends_on_unlanded", http.StatusConflict,
			fmt.Sprintf("%s depends on %s, which is not landed: land it first, or the stack with stack true",
				ch.Ref, below[len(below)-2].Ref))
	}
	return chain, nil
}

// collect is the changes a land of ch covers, bottom first, read through
// q (nil for the store): ch alone, or for a stack ch and the changes below
// it down its depends_on, up to the first that is landed or does not
// exist. A depends_on that leads back to a change already collected ends
// the walk, so that a cycle cannot hold it.
func (c *Component) collect(ctx context.Context, q Querier, ch change.Change, stack bool) ([]change.Change, error) {
	chain := []change.Change{ch}
	for cur := ch; stack && cur.DependsOn != nil; {
		next := *cur.DependsOn
		if slices.ContainsFunc(chain, func(x change.Change) bool { return x.Number == next }) {
			break
		}
		below, err := c.deps.Changes.ReadChange(ctx, q, ch.WorkspaceSlug, next)
		if errors.Is(err, change.ErrChangeNotFound) {
			break
		}
		if err != nil {
			return nil, err
		}
		if below.State == change.StateLanded {
			break
		}
		chain, cur = append(chain, below), below
	}
	slices.Reverse(chain)
	return chain, nil
}

// covered is each change of chain as an attempt covers it: at its current
// version, with that version's head and base and the change's source.
func covered(chain []change.Change) []CoveredChange {
	out := make([]CoveredChange, len(chain))
	for i, ch := range chain {
		out[i] = CoveredChange{Number: ch.Number, Version: ch.Version, Head: ch.HeadSHA,
			SourceWorkspace: ch.Source.WorkspaceSlug, SourceBranch: ch.Source.Branch}
		if ch.BaseSHA != nil {
			out[i].Base = *ch.BaseSHA
		}
	}
	return out
}
