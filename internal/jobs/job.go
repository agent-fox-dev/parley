// Package jobs is the hub's durable job queue: a job is recorded in the
// transactional store in the transaction of the request that asks for it,
// survives a restart, and is run by the handler its type registers. The
// queue knows a job's type, workspace, payload and result as opaque JSON
// documents; the package that owns a type gives them their meaning.
package jobs

import (
	"encoding/json"
	"time"
)

// The statuses of a job. A job moves only along queued → running →
// completed, failed or dead_letter; failed → queued when its next attempt
// is due; queued → cancelled; dead_letter → queued on a requeue. failed is
// never an end state.
const (
	StatusQueued     = "queued"
	StatusRunning    = "running"
	StatusFailed     = "failed"
	StatusCompleted  = "completed"
	StatusDeadLetter = "dead_letter"
	StatusCancelled  = "cancelled"
)

// Statuses are the six statuses of a job.
var Statuses = []string{StatusQueued, StatusRunning, StatusFailed, StatusCompleted, StatusDeadLetter, StatusCancelled}

// Pending reports whether status is one a job still has work ahead in:
// queued, running or failed. A type and workspace have at most one pending
// job, apart from the follow-up of a running one.
func Pending(status string) bool {
	return status == StatusQueued || status == StatusRunning || status == StatusFailed
}

// Final reports whether status is an end state: completed, dead_letter or
// cancelled. Only a requeue takes a job out of dead_letter.
func Final(status string) bool {
	return status == StatusCompleted || status == StatusDeadLetter || status == StatusCancelled
}

// Job is a stored job. A nil document is a JSON null.
type Job struct {
	ID        string
	Type      string
	Workspace string
	Status    string
	// Payload is the document the job was enqueued with; it never changes.
	Payload json.RawMessage
	// Result is what the running or last attempt recorded; a claim empties
	// it.
	Result json.RawMessage
	// Detail is the document the last failure carried.
	Detail     json.RawMessage
	RetryCount int
	// NextAttemptAt is when a failed job is queued again; nil in every other
	// status.
	NextAttemptAt *time.Time
	// Error is the message of the last failure, kept through a retry until
	// the job completes or is requeued.
	Error       *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

// ListPosition is a job's place in the order of List: created_at at the
// whole seconds a record shows it, then the id.
type ListPosition struct {
	Seconds int64  `json:"t"`
	ID      string `json:"id"`
}

// Position is the job's place in the order of List, for the page after it.
func (j Job) Position() ListPosition {
	return ListPosition{Seconds: j.CreatedAt.Unix(), ID: j.ID}
}
