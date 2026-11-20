package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// WorkDir is the working directory of every sandbox: the runtime is given it
// at creation, the checkout clones into it, and exec, files and checkpoints
// work in it.
const WorkDir = "/workspace"

// The states of a sandbox. Deleted is the only state a sandbox never leaves.
const (
	StateProvisioning = "provisioning"
	StateReady        = "ready"
	StateStopped      = "stopped"
	StateCompleted    = "completed"
	StateFailed       = "failed"
	StateDeleted      = "deleted"
)

// The provisioning markers: what put a record in the provisioner's work.
const (
	// MarkerCreate marks a sandbox the create operation stored.
	MarkerCreate = "create"
	// MarkerStart marks a sandbox a start moved back to provisioning.
	MarkerStart = "start"
)

// idPrefix begins every sandbox id; 32 lowercase hexadecimal characters
// follow it.
const idPrefix = "sbx_"

// NewID returns a new sandbox id: sbx_ and 32 lowercase hexadecimal
// characters from a cryptographic random source.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return idPrefix + hex.EncodeToString(b[:])
}

// Record is a stored sandbox: every field of the sandbox object but
// runtime_reachable, which is read from the runtime client when an object is
// rendered, and what the sandbox packages keep with it and never show.
type Record struct {
	ID        string
	Workspace string
	// SessionID is the agent session the sandbox belongs to; nil for none.
	SessionID *string
	Profile   string
	Purpose   string
	State     string
	// StateReason is why the sandbox is in its state; "" for none.
	StateReason string
	// Command is the main process's arguments; nil for an idle sandbox.
	Command []string
	// Env is the environment the creator asked for.
	Env      map[string]string
	ExitCode *int
	// Checkout is nil for a sandbox without one.
	Checkout *Checkout
	// Image, CPU, Memory and GPU are the profile's values at creation.
	Image  string
	CPU    string
	Memory string
	GPU    int
	// Credentials and ExposedSecrets are the attached names, sorted.
	Credentials    []string
	ExposedSecrets []string
	// WorkspaceRules are the workspace sandbox rules recorded at creation,
	// oldest first, each with a copy of its body.
	WorkspaceRules []WorkspaceRule
	// HubAccess is the sandbox's hub credential; nil for none.
	HubAccess      *HubAccess
	PolicyRevision int
	ExpiresAt      time.Time
	// IdleTimeout is 0 when the idle stop is off.
	IdleTimeout    time.Duration
	LastActivityAt time.Time
	// Checkpoint is the latest checkpoint as a JSON object; nil for none.
	Checkpoint   json.RawMessage
	RestoredFrom *string
	Labels       map[string]string
	CreatedBy    Creator
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// EndedAt is nil until the sandbox ends.
	EndedAt *time.Time

	// Snapshot is the profile as it was at creation.
	Snapshot Snapshot
	// HubTokenID is the id of the hub credential's token; "" for none.
	HubTokenID string
	// UserID is the id of the creating user, who owns the sandbox.
	UserID string
	// OrgID is the id of the workspace's organization; "" for none.
	OrgID string
	// FamilyRoot is the slug of the repository the workspace belongs to: the
	// parent of a fork, the workspace itself otherwise.
	FamilyRoot string
	// Marker is MarkerCreate or MarkerStart while the provisioner has the
	// sandbox to bring up; "" otherwise.
	Marker string
	// RuntimeCreated reports that the sandbox was created at the runtime.
	RuntimeCreated bool
	// RuntimeProvisioning reports that the runtime, not a request, put the
	// sandbox in provisioning.
	RuntimeProvisioning bool
	// ReadyAt is when the sandbox became ready, kept through the runtime's
	// pause (ready to provisioning and back); nil before it is ready and
	// once it has ended.
	ReadyAt *time.Time
}

// Checkout is where and what the sandbox has checked out.
type Checkout struct {
	Path string `json:"path"`
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	// Branch is the work branch; nil on a repository, where none is created.
	Branch *string `json:"branch"`
}

// WorkspaceRule is a workspace sandbox rule recorded at creation: its id,
// which the object shows, and a copy of its body, which it does not.
type WorkspaceRule struct {
	ID   string          `json:"id"`
	Body json.RawMessage `json:"body"`
}

// Creator is the actor that created a sandbox.
type Creator struct {
	ActorType string `json:"actor_type"`
	ActorID   string `json:"actor_id"`
	Username  string `json:"username"`
}

// Snapshot is the part of a profile a sandbox keeps from its creation on, so
// that it outlives a change or the removal of the profile.
type Snapshot struct {
	TTL            time.Duration     `json:"ttl"`
	IdleTimeout    time.Duration     `json:"idle_timeout"`
	Checkpoint     bool              `json:"checkpoint"`
	AllowProposals bool              `json:"allow_proposals"`
	HubAccess      string            `json:"hub_access"`
	Env            map[string]string `json:"env"`
	// BasePolicy is the content of the profile's policy file; nil for the
	// built-in policy.
	BasePolicy            []byte   `json:"base_policy"`
	Credentials           []string `json:"credentials"`
	OptionalCredentials   []string `json:"optional_credentials"`
	ExposeSecrets         []string `json:"expose_secrets"`
	OptionalExposeSecrets []string `json:"optional_expose_secrets"`
	// ProviderProfiles maps the name of each credential of the lists above to
	// its provider profile.
	ProviderProfiles map[string]string `json:"provider_profiles"`
	// ToolServer is the absolute path of the tool server in the sandbox's
	// image; "" for a sandbox without tools, one stored before the profiles
	// could name a tool server included.
	ToolServer string `json:"tool_server,omitempty"`
}
