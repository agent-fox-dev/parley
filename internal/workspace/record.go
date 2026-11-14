package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
)

// Status is where a workspace stands in its life: clone work outstanding
// (pending, cloning), able to serve its repository (ready), in need of a
// reclone (failed), or archived. The values are the access rule's, because
// both packages read the same column.
type Status string

// The statuses of a workspace.
const (
	StatusPending  Status = access.StatusPending
	StatusCloning  Status = access.StatusCloning
	StatusReady    Status = access.StatusReady
	StatusFailed   Status = access.StatusFailed
	StatusArchived Status = access.StatusArchived
)

// Marker names the maintenance operation running on a workspace, or is
// MarkerNone. It is kept apart from Status so that a failed or archived
// workspace that is being recloned or deleted keeps the status it will fall
// back to if the operation stops.
type Marker string

// The maintenance markers.
const (
	MarkerNone    Marker = ""
	MarkerReclone Marker = "reclone"
	MarkerArchive Marker = "archive"
	MarkerDelete  Marker = "delete"
)

// The workspace modes: a plain repository, or one that tracks an upstream
// and carries an ordered list of patches on top of it.
const (
	ModeStandard   = "standard"
	ModeCarryPatch = "carry_patch"
)

// ForkPoint is the parent branch a task fork was taken from and that
// branch's head at that moment.
type ForkPoint struct {
	Ref string
	SHA string
}

// Workspace is the whole record of a workspace. A pointer field is nil where
// the field is null; Settings is the stored settings document, holding only
// the keys that were set. InstanceID and Marker are the hub's own and no
// response shows them, nor CreatingTokenID (see access.Record).
type Workspace struct {
	Slug              string
	Kind              string // access.KindRepository or access.KindFork
	Mode              string // ModeStandard or ModeCarryPatch
	GitURL            *string
	DefaultBranch     *string
	OwnerID           string
	OrgID             string
	Status            Status
	DisplayName       string
	Description       string
	HeadSHA           *string
	OriginHeadSHA     *string
	CloneError        *string
	LastSyncAt        *time.Time
	SyncError         *string
	UpstreamURL       *string
	IntegrationBranch *string
	UpstreamHeadSHA   *string
	ParentSlug        *string
	ForkPoint         *ForkPoint
	ExpiresAt         *time.Time
	AutoChange        *bool
	Purpose           *string
	SessionID         *string
	Settings          map[string]json.RawMessage
	CreatedAt         time.Time
	UpdatedAt         time.Time
	// InstanceID tells this workspace's copy on disk from a copy an earlier
	// workspace of the same slug left behind; a finished clone writes it
	// into the copy.
	InstanceID      string
	Marker          Marker
	CreatingTokenID string
}

// NewInstanceID returns a random instance id for a new workspace record:
// 128 bits as 32 hex digits, so that two records never share one.
func NewInstanceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}
