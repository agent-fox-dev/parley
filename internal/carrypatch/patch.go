package carrypatch

import (
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The statuses of a patch. Only a deleted patch has no position.
const (
	StatusActive         = "active"
	StatusConflict       = "conflict"
	StatusDisabled       = "disabled"
	StatusMergedUpstream = "merged_upstream"
	StatusDeleted        = "deleted"
)

// Patch is one entry of a carry-patch workspace's patch list, as the store
// holds it. A pointer field is nil where the field is unset.
type Patch struct {
	// ID is the patch's UUID.
	ID         string
	Workspace  string
	BranchName string
	// Position is the patch's place in the order, from 1; nil for a deleted
	// patch. The patches of a workspace that are not deleted hold 1..N.
	Position *int
	Status   string
	// ConflictFiles are the files of the last conflict, empty unless the
	// status is conflict.
	ConflictFiles   []string
	UpstreamPRURL   *string
	Description     *string
	AddedAt         time.Time
	UpdatedAt       time.Time
	DeletedAt       *time.Time
	OriginSyncState *string
	OriginSHA       *string
	OriginSyncedAt  *time.Time
}

// Object is a patch as the API shows it: every member present, null where
// it is unset, conflict_files never null, and replaced_sha only when the
// patch's backup ref exists.
type Object struct {
	ID              string        `json:"id"`
	Workspace       string        `json:"workspace_slug"`
	BranchName      string        `json:"branch_name"`
	Position        *int          `json:"position"`
	Status          string        `json:"status"`
	ConflictFiles   []string      `json:"conflict_files"`
	UpstreamPRURL   *string       `json:"upstream_pr_url"`
	Description     *string       `json:"description"`
	AddedAt         timefmt.Time  `json:"added_at"`
	UpdatedAt       timefmt.Time  `json:"updated_at"`
	DeletedAt       *timefmt.Time `json:"deleted_at"`
	OriginSyncState *string       `json:"origin_sync_state"`
	OriginSHA       *string       `json:"origin_sha"`
	OriginSyncedAt  *timefmt.Time `json:"origin_synced_at"`
	ReplacedSHA     *string       `json:"replaced_sha,omitempty"`
}

// ObjectOf is p as the API shows it, with replaced_sha set to replacedSHA,
// the commit of the patch's backup ref; "" leaves the member out.
func ObjectOf(p Patch, replacedSHA string) Object {
	o := Object{
		ID: p.ID, Workspace: p.Workspace, BranchName: p.BranchName, Position: p.Position, Status: p.Status,
		ConflictFiles: p.ConflictFiles, UpstreamPRURL: p.UpstreamPRURL, Description: p.Description,
		AddedAt: timefmt.Time(p.AddedAt), UpdatedAt: timefmt.Time(p.UpdatedAt), DeletedAt: timeOf(p.DeletedAt),
		OriginSyncState: p.OriginSyncState, OriginSHA: p.OriginSHA, OriginSyncedAt: timeOf(p.OriginSyncedAt),
	}
	if o.ConflictFiles == nil {
		o.ConflictFiles = []string{}
	}
	if replacedSHA != "" {
		o.ReplacedSHA = &replacedSHA
	}
	return o
}

// timeOf is *t in the API's form, nil for nil.
func timeOf(t *time.Time) *timefmt.Time {
	if t == nil {
		return nil
	}
	v := timefmt.Time(*t)
	return &v
}
