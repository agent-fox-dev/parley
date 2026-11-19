package hub

import (
	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/metrics"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/scm"
)

// PushChanges is the change service of the push processor's mapping stage
// of the composition c, ready once the hub has started.
func PushChanges(c *Composition) gitserver.ChangeService { return pushChanges{c.changes, c.runtime} }

// CopyTree is copyTree: it copies src to dst, leaving out the directories
// below src that deferred reports true for.
var CopyTree = copyTree

// SessionLinkers are the session-linking rule and the session record of
// the composition c as composeSessions registers them with each consumer,
// each written to that consumer's declaration.
type SessionLinkers struct {
	Fork      fork.SessionLinker
	Push      gitserver.SessionLinker
	SCM       scm.SessionLinker
	Change    change.SessionLinker
	Land      land.SessionLinker
	Awareness awareness.Sessions
	Sandbox   sandbox.SessionLinker
}

// SessionLinkersOf are the session linkers of the composition c.
func SessionLinkersOf(c *Composition) SessionLinkers {
	return SessionLinkers{
		Fork: forkSessions{c.sessions}, Push: pushSessions{c.sessions}, SCM: scmSessions{c.sessions},
		Change: changeSessions{c.sessions}, Land: landSessions{c.sessions}, Awareness: awarenessSessions{c.sessions},
		Sandbox: sandboxSessions{c.sessions},
	}
}

// SandboxUseOf is sandboxUseOf: the operation of the sandbox component as
// the telemetry part hands it to the recorder of sandbox use.
var SandboxUseOf = sandboxUseOf

// The sources and reporters connectProducers connects to the registry,
// each built over the read or the recorder a test gives.
var (
	GitRequestsSource   = gitRequests
	ChangesOpenSource   = changesOpen
	ForksActiveSource   = forksActive
	JobqueueDepthSource = jobqueueDepth
	TokenReporter       = tokenReporter
	LandReporter        = landReporter
)

// SCMRecorder is the request recorder of the source control API that
// counts each request in rec.
func SCMRecorder(rec metrics.Recorder) scm.Recorder { return scmRecorder{rec} }
