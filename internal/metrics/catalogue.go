// Package metrics holds the hub's metric catalogue and the registry every
// package reports to. The catalogue is fixed: a report or a source outside
// it is refused, and every metric in it is exported from start-up, a closed
// label with every one of its values at 0, so that a documented metric is
// never absent. The package writes no exposition of its own beyond what
// Gather returns; it is written on the standard library alone, because the
// catalogue is small and the "present at 0 from start" rule is simpler to
// keep in one table than through a client library.
package metrics

// Type is a metric's type, written as the text exposition names it.
type Type string

// The three metric types of the catalogue.
const (
	Counter   Type = "counter"
	Gauge     Type = "gauge"
	Histogram Type = "histogram"
)

// Label is one label of a metric. Values lists a closed label's every value;
// it is nil for an open label, whose values are whatever is reported.
type Label struct {
	Name   string
	Values []string
}

// Metric is one entry of the catalogue: its name, help text, type, labels in
// their documented order and, for a histogram, the upper bounds of its
// buckets in ascending order (+Inf is implied).
type Metric struct {
	Name    string
	Help    string
	Type    Type
	Labels  []Label
	Buckets []float64
}

// The names of the catalogue's metrics.
const (
	HTTPRequestsTotal             = "parley_http_requests_total"
	HTTPRequestDurationSeconds    = "parley_http_request_duration_seconds"
	GitRequestsTotal              = "parley_git_requests_total"
	SCMRequestsTotal              = "parley_scm_requests_total"
	ChangesOpen                   = "parley_changes_open"
	LandsTotal                    = "parley_lands_total"
	LandAttempts                  = "parley_land_attempts"
	LandDurationSeconds           = "parley_land_duration_seconds"
	ForksActive                   = "parley_forks_active"
	EventsSeq                     = "parley_events_seq"
	SSEConnections                = "parley_sse_connections"
	WebhookDeliveriesTotal        = "parley_webhook_deliveries_total"
	WebhookQueueDepth             = "parley_webhook_queue_depth"
	AuditEventsTotal              = "parley_audit_events_total"
	AgentSessionsActive           = "parley_agent_sessions_active"
	AgentTokensTotal              = "parley_agent_tokens_total"
	AuditRecords                  = "parley_audit_records"
	JobqueueDepth                 = "parley_jobqueue_depth"
	RetentionErrorsTotal          = "parley_retention_errors_total"
	RetentionLastRunTimestamp     = "parley_retention_last_run_timestamp_seconds"
	Sandboxes                     = "parley_sandboxes"
	SandboxCreatesTotal           = "parley_sandbox_creates_total"
	SandboxSeconds                = "parley_sandbox_seconds"
	SandboxExecTotal              = "parley_sandbox_exec_total"
	SandboxToolCallsTotal         = "parley_sandbox_tool_calls_total"
	SandboxToolCallDuration       = "parley_sandbox_tool_call_duration_seconds"
	SandboxDenialsTotal           = "parley_sandbox_denials_total"
	SandboxCheckpointsTotal       = "parley_sandbox_checkpoints_total"
	SandboxRuntimeUp              = "parley_sandbox_runtime_up"
	CodeIndexBuildsTotal          = "parley_code_index_builds_total"
	CodeIndexBuildDurationSeconds = "parley_code_index_build_duration_seconds"
	CodeIndexBytes                = "parley_code_index_bytes"
	CodeIndexWaitSeconds          = "parley_code_index_wait_seconds"
)

func closed(name string, values ...string) Label { return Label{Name: name, Values: values} }
func open(name string) Label                     { return Label{Name: name} }

// catalogue is every metric the hub exports, in exposition order. Nothing
// changes it after start-up; registries share it.
var catalogue = []Metric{
	{Name: HTTPRequestsTotal, Type: Counter,
		Help:   "HTTP requests answered, API, git and webhook routes included.",
		Labels: []Label{open("method"), open("path"), open("status")}},
	{Name: HTTPRequestDurationSeconds, Type: Histogram,
		Help:    "Time to answer an HTTP request, in seconds.",
		Labels:  []Label{open("method"), open("path")},
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}},
	{Name: GitRequestsTotal, Type: Counter,
		Help: "Git protocol requests.",
		Labels: []Label{closed("service", "info-refs", "upload-pack", "receive-pack"),
			closed("result", "ok", "refused", "error")}},
	{Name: SCMRequestsTotal, Type: Counter,
		Help: "Source control API requests.",
		Labels: []Label{closed("op", "refs", "commits", "tree", "file", "diff", "compare", "blame", "search", "outline", "declaration",
			"code_index", "code_search", "symbols", "commit", "ref_create", "ref_move", "ref_delete", "rebase"),
			closed("result", "ok", "ref_moved", "refused", "error")}},
	{Name: ChangesOpen, Type: Gauge,
		Help:   "Changes in state draft or open.",
		Labels: []Label{open("workspace")}},
	{Name: LandsTotal, Type: Counter,
		Help:   "Land requests by final result.",
		Labels: []Label{closed("result", "landed", "conflict", "blocked", "failed")}},
	{Name: LandAttempts, Type: Histogram,
		Help:    "Compare-and-swap attempts per land.",
		Buckets: []float64{1, 2, 3, 5, 8, 13}},
	{Name: LandDurationSeconds, Type: Histogram,
		Help:    "Time from a land request to its final result, in seconds.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}},
	{Name: ForksActive, Type: Gauge, Help: "Task forks that exist."},
	{Name: EventsSeq, Type: Gauge, Help: "The last hub event seq written."},
	{Name: SSEConnections, Type: Gauge, Help: "Open event streams."},
	{Name: WebhookDeliveriesTotal, Type: Counter,
		Help:   "Event webhook delivery attempts.",
		Labels: []Label{closed("result", "ok", "retry", "failed")}},
	{Name: WebhookQueueDepth, Type: Gauge, Help: "Events written and not yet delivered or given up."},
	{Name: AuditEventsTotal, Type: Counter,
		Help:   "Hub events written and run events ingested.",
		Labels: []Label{closed("source", "hub", "agent"), open("type")}},
	{Name: AgentSessionsActive, Type: Gauge,
		Help:   "Agent sessions in state active.",
		Labels: []Label{open("workspace")}},
	{Name: AgentTokensTotal, Type: Counter,
		Help:   "Tokens reported in agent usage records.",
		Labels: []Label{open("workspace"), open("model"), closed("direction", "input", "output", "cache_read")}},
	{Name: AuditRecords, Type: Gauge,
		Help: "Stored audit and session records, sampled by retention.",
		Labels: []Label{closed("kind", "hub_event", "run_event", "outcome", "tool_call", "tool_error", "trace",
			"postmortem", "session", "usage")}},
	{Name: JobqueueDepth, Type: Gauge,
		Help: "Durable jobs by type and status.",
		Labels: []Label{closed("type", "rebuild"),
			closed("status", "queued", "running", "failed", "completed", "dead_letter", "cancelled")}},
	{Name: RetentionErrorsTotal, Type: Counter,
		Help: "Failed retention steps.",
		Labels: []Label{closed("step", "stale_sessions", "run_records", "max_runs", "traces", "postmortems",
			"sessions", "event_log", "webhook_deliveries", "hub_events", "orphans", "gauges")}},
	{Name: RetentionLastRunTimestamp, Type: Gauge, Help: "Unix time of the last fully successful retention run."},
	{Name: Sandboxes, Type: Gauge,
		Help:   "Sandboxes by state.",
		Labels: []Label{closed("state", "provisioning", "ready", "stopped", "completed", "failed")}},
	{Name: SandboxCreatesTotal, Type: Counter,
		Help:   "Sandbox creations by outcome.",
		Labels: []Label{closed("result", "ready", "failed")}},
	{Name: SandboxSeconds, Type: Histogram,
		Help:    "Time from a sandbox's ready to its stop, exit or deletion, in seconds.",
		Buckets: []float64{60, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800, 86400}},
	{Name: SandboxExecTotal, Type: Counter,
		Help:   "Commands run through sandbox exec.",
		Labels: []Label{closed("result", "ok", "nonzero", "timeout", "error")}},
	{Name: SandboxToolCallsTotal, Type: Counter,
		Help:   "Tool calls run in sandboxes, by tool and outcome.",
		Labels: []Label{open("tool"), closed("outcome", "ok", "tool_error", "timeout", "failed")}},
	{Name: SandboxToolCallDuration, Type: Histogram,
		Help:    "Time a tool call ran in its sandbox, in seconds.",
		Labels:  []Label{open("tool")},
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300, 600}},
	{Name: SandboxDenialsTotal, Type: Counter, Help: "Connections and requests the sandbox runtime blocked."},
	{Name: SandboxCheckpointsTotal, Type: Counter,
		Help:   "Sandbox checkpoints attempted.",
		Labels: []Label{closed("result", "ok", "empty", "failed")}},
	{Name: SandboxRuntimeUp, Type: Gauge, Help: "1 while the sandbox runtime is reachable."},
	{Name: CodeIndexBuildsTotal, Type: Counter,
		Help:   "Code index builds that ended, by outcome.",
		Labels: []Label{closed("outcome", "ok", "partial", "failed")}},
	{Name: CodeIndexBuildDurationSeconds, Type: Histogram,
		Help:    "Time a code index build ran, in seconds.",
		Buckets: []float64{0.1, 0.5, 1, 5, 10, 30, 60, 120, 300, 600}},
	{Name: CodeIndexBytes, Type: Gauge, Help: "Bytes of the code index trees and indexes on disk."},
	{Name: CodeIndexWaitSeconds, Type: Histogram,
		Help:    "Time a code search or symbol lookup waited for the index of the head, in seconds.",
		Labels:  []Label{closed("outcome", "ready", "timeout", "failed")},
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60}},
}
