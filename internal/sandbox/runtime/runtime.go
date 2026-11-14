// Package runtime is the hub's one boundary to the sandbox runtime: the
// Client interface every sandbox package calls, the plain data it
// exchanges, the four error kinds a caller tells apart, the reachability
// flag and the metrics recorder the flag reports to.
//
// Two implementations exist: one over the OpenShell Go SDK in
// runtime/openshell, and an in-memory fake in runtime/runtimetest, which
// also holds the contract tests both of them pass. No other part of the hub
// talks to the runtime.
//
// Every Client acts in one OpenShell workspace, the one it was built for, and
// in no other. Every operation takes a context. An operation that does not
// stream is given at most 30 seconds, or less when the caller's context ends
// sooner; the streaming operations (WatchSandboxes, Logs with Follow,
// ExecStream, ExecInteractive, Upload, Download and Walk) end with the
// caller's context. A failure is an error of exactly one of the kinds of
// errors.go, and every result moves the reachability flag (Reachability).
package runtime

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"time"
)

// Client is the runtime client: every operation the hub performs on the
// sandbox runtime, in the client's OpenShell workspace. Packages call these
// operations by these names and add none; an implementation whose runtime
// lacks one answers it with an error of kind ErrUnsupported and does not
// imitate it with other calls.
type Client interface {
	// Release reads the gateway's health and its release.
	Release(ctx context.Context) (Release, error)
	// EnsureWorkspace makes sure the client's OpenShell workspace exists,
	// creating it when it does not.
	EnsureWorkspace(ctx context.Context) error
	// Reachable reports the reachability flag. It makes no call.
	Reachable() bool

	// GetProviderProfile reads a provider profile by its id.
	GetProviderProfile(ctx context.Context, id string) (ProviderProfile, error)
	// InstallProviderProfile creates a provider profile, or replaces the one
	// with the same id.
	InstallProviderProfile(ctx context.Context, profile ProviderProfile) error

	// EnsureProvider creates a provider, or replaces the one with the same
	// name.
	EnsureProvider(ctx context.Context, provider Provider) error
	// DeleteProvider deletes a provider by name.
	DeleteProvider(ctx context.Context, name string) error
	// AttachProvider attaches a provider to a sandbox.
	AttachProvider(ctx context.Context, sandbox, provider string) error
	// DetachProvider detaches a provider from a sandbox.
	DetachProvider(ctx context.Context, sandbox, provider string) error
	// SandboxProviders returns the names of the providers attached to a
	// sandbox, in name order.
	SandboxProviders(ctx context.Context, sandbox string) ([]string, error)
	// ListProviders returns every provider of the workspace, in name order,
	// with their labels and without their values.
	ListProviders(ctx context.Context) ([]Provider, error)

	// ValidatePolicy asks the runtime whether it accepts a policy value, and
	// which hosts it allows and which providers its base document refers to.
	ValidatePolicy(ctx context.Context, policy Policy) (PolicyValidation, error)
	// GetPolicy returns a sandbox's policy value and its revision.
	GetPolicy(ctx context.Context, sandbox string) (SandboxPolicy, error)
	// AddPolicyRule adds one rule to a sandbox's policy, after the rules
	// added before it.
	AddPolicyRule(ctx context.Context, sandbox string, rule Rule) error
	// ListProposals returns the pending proposals of the workspace.
	ListProposals(ctx context.Context) ([]Proposal, error)
	// ApproveProposal approves a pending proposal of a sandbox.
	ApproveProposal(ctx context.Context, sandbox, id string) error
	// RejectProposal rejects a pending proposal of a sandbox, with a reason.
	RejectProposal(ctx context.Context, sandbox, id, reason string) error

	// CreateSandbox creates a sandbox and returns its status.
	CreateSandbox(ctx context.Context, spec SandboxSpec) (SandboxStatus, error)
	// GetSandbox returns the status of a sandbox.
	GetSandbox(ctx context.Context, name string) (SandboxStatus, error)
	// ListSandboxes returns the status of every sandbox of the workspace, in
	// name order.
	ListSandboxes(ctx context.Context) ([]SandboxStatus, error)
	// DeleteSandbox deletes a sandbox.
	DeleteSandbox(ctx context.Context, name string) error
	// StopSandbox releases a sandbox's compute and keeps its working
	// directory.
	StopSandbox(ctx context.Context, name string) error
	// StartSandbox starts a stopped sandbox again with a policy value and an
	// environment.
	StartSandbox(ctx context.Context, name string, spec StartSpec) error
	// WaitReady returns the status of a sandbox once it is running.
	WaitReady(ctx context.Context, name string) (SandboxStatus, error)
	// WatchSandboxes delivers the changes of the workspace's sandboxes until
	// the caller's context ends.
	WatchSandboxes(ctx context.Context) (SandboxWatch, error)
	// RunMain starts the main process of a sandbox from a command.
	RunMain(ctx context.Context, sandbox string, command []string) error

	// Exec runs a command in a sandbox and collects its exit code, stdout
	// and stderr.
	Exec(ctx context.Context, sandbox string, req ExecRequest) (ExecResult, error)
	// ExecStream runs a command in a sandbox and delivers its stdout and
	// stderr as they are produced; the command is killed when the caller's
	// context ends.
	ExecStream(ctx context.Context, sandbox string, req ExecRequest) (ExecStream, error)
	// ExecInteractive runs a command in a sandbox on a terminal of a size.
	ExecInteractive(ctx context.Context, sandbox string, req ExecRequest, size TerminalSize) (Terminal, error)

	// Upload writes what r holds to a file of a sandbox with a mode,
	// creating its parent directories.
	Upload(ctx context.Context, sandbox, path string, mode fs.FileMode, r io.Reader) error
	// MakeDir creates a directory of a sandbox and its parents.
	MakeDir(ctx context.Context, sandbox, path string, mode fs.FileMode) error
	// Stat describes a file or directory of a sandbox.
	Stat(ctx context.Context, sandbox, path string) (FileInfo, error)
	// Download returns a file of a sandbox as a stream; the caller closes it.
	Download(ctx context.Context, sandbox, path string) (io.ReadCloser, error)
	// Walk describes every file and directory below a directory of a
	// sandbox, in path order.
	Walk(ctx context.Context, sandbox, dir string) ([]FileInfo, error)

	// Logs returns the main process's output or the policy log of a
	// sandbox.
	Logs(ctx context.Context, sandbox string, query LogQuery) (LogStream, error)
}

// The names of the operations, as Error.Op and the fake's call list carry
// them.
const (
	OpRelease                = "release"
	OpEnsureWorkspace        = "ensure_workspace"
	OpGetProviderProfile     = "get_provider_profile"
	OpInstallProviderProfile = "install_provider_profile"
	OpEnsureProvider         = "ensure_provider"
	OpDeleteProvider         = "delete_provider"
	OpAttachProvider         = "attach_provider"
	OpDetachProvider         = "detach_provider"
	OpSandboxProviders       = "sandbox_providers"
	OpListProviders          = "list_providers"
	OpValidatePolicy         = "validate_policy"
	OpGetPolicy              = "get_policy"
	OpAddPolicyRule          = "add_policy_rule"
	OpListProposals          = "list_proposals"
	OpApproveProposal        = "approve_proposal"
	OpRejectProposal         = "reject_proposal"
	OpCreateSandbox          = "create_sandbox"
	OpGetSandbox             = "get_sandbox"
	OpListSandboxes          = "list_sandboxes"
	OpDeleteSandbox          = "delete_sandbox"
	OpStopSandbox            = "stop_sandbox"
	OpStartSandbox           = "start_sandbox"
	OpWaitReady              = "wait_ready"
	OpWatchSandboxes         = "watch_sandboxes"
	OpRunMain                = "run_main"
	OpExec                   = "exec"
	OpExecStream             = "exec_stream"
	OpExecInteractive        = "exec_interactive"
	OpUpload                 = "upload"
	OpMakeDir                = "make_dir"
	OpStat                   = "stat"
	OpDownload               = "download"
	OpWalk                   = "walk"
	OpLogs                   = "logs"
)

// Release is the gateway's health and the release it runs.
type Release struct {
	Healthy bool
	// Version is the gateway's release as the gateway states it, for example
	// "0.1.8" or "v0.1.8".
	Version string
}

// ProviderProfile is OpenShell's definition of how a credential may be used:
// the variables that hold its placeholder, and the endpoints and programs it
// is added for.
type ProviderProfile struct {
	ID string
	// Variables are the environment variables the profile declares.
	Variables []string
	// Endpoints are the endpoints the credential is added on requests to.
	Endpoints []Endpoint
	// Programs are the programs whose requests get the credential; empty
	// means any program.
	Programs []string
}

// Endpoint is an endpoint a provider profile authorizes.
type Endpoint struct {
	// Scheme is "https" or "http"; "" leaves it to the runtime.
	Scheme string
	Host   string
	Port   int
	// Paths are the path prefixes authorized on the endpoint; empty means
	// every path.
	Paths []string
}

// Provider is a credential attached to sandboxes through a provider profile.
type Provider struct {
	Name string
	// Profile is the id of the provider profile.
	Profile string
	// Value is the secret value. ListProviders never returns it.
	Value  string
	Labels map[string]string
}

// Policy is a policy value in three parts, which the runtime merges: the
// base document, the protected rules and the added rules. No base document
// and no added rule removes or narrows a protected rule.
type Policy struct {
	// Base is the base document: the content of a profile's policy file or
	// the built-in policy.
	Base []byte
	// Protected are the rules the hub keeps in every sandbox's policy.
	Protected []Rule
	// Added are the rules added to the sandbox's policy, in the order they
	// were added.
	Added []Rule
}

// Rule is one rule of a policy, an object in OpenShell's schema that the hub
// does not interpret, under its name.
type Rule struct {
	Name string
	Body json.RawMessage
}

// PolicyValidation is what the runtime reports of a policy value it accepts.
type PolicyValidation struct {
	// Hosts are the hosts the policy allows.
	Hosts []string
	// Providers are the provider names the base document refers to.
	Providers []string
}

// SandboxPolicy is a sandbox's policy value and its revision, which grows
// with every change.
type SandboxPolicy struct {
	Policy   Policy
	Revision uint64
}

// Proposal is a pending proposal of the runtime: a rule it suggests adding
// to a sandbox's policy.
type Proposal struct {
	// ID is the runtime's id of the proposal.
	ID      string
	Sandbox string
	Rule    Rule
	// Summary is one line.
	Summary  string
	Findings []Finding
}

// Finding is the runtime's remark on a proposal.
type Finding struct {
	Severity string
	Message  string
}

// SandboxSpec is what a sandbox is created with.
type SandboxSpec struct {
	Name string
	// Image, CPU and Memory are "" when they are left to the runtime.
	Image  string
	CPU    string
	Memory string
	GPU    int
	// WorkDir is the sandbox's working directory.
	WorkDir   string
	Env       map[string]string
	Policy    Policy
	Providers []string
	Labels    map[string]string
}

// StartSpec is what a stopped sandbox is started again with.
type StartSpec struct {
	Policy Policy
	Env    map[string]string
}

// State is a sandbox's state as the runtime reports it.
type State string

// The states of a sandbox.
const (
	StatePending    State = "pending"
	StateRunning    State = "running"
	StateRestarting State = "restarting"
	StateStopped    State = "stopped"
)

// SandboxStatus is a sandbox as the runtime reports it.
type SandboxStatus struct {
	Name   string
	Labels map[string]string
	State  State
	// MainExited reports whether the main process has ended, and ExitCode
	// with which code.
	MainExited bool
	ExitCode   int
	// WorkDirPresent reports whether the runtime still holds the working
	// directory.
	WorkDirPresent bool
}

// SandboxEvent is one change WatchSandboxes delivers: the sandbox's status
// after the change, or its last status when it was deleted.
type SandboxEvent struct {
	Sandbox SandboxStatus
	Deleted bool
}

// SandboxWatch delivers sandbox changes.
type SandboxWatch interface {
	// Next returns the next change. It returns io.EOF once the caller's
	// context ended or the watch was closed, and an error of kind
	// ErrUnavailable when the stream broke.
	Next() (SandboxEvent, error)
	Close() error
}

// ExecRequest is a command to run in a sandbox.
type ExecRequest struct {
	Command []string
	// WorkDir is the directory the command runs in; "" is the sandbox's
	// working directory.
	WorkDir string
	Env     map[string]string
	Stdin   []byte
}

// ExecResult is what a command that ended produced.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// OutputStream names a command's output stream.
type OutputStream string

// The output streams.
const (
	Stdout OutputStream = "stdout"
	Stderr OutputStream = "stderr"
)

// Output is a piece of a command's output.
type Output struct {
	Stream OutputStream
	Data   []byte
}

// ExecStream delivers a command's output as it is produced.
type ExecStream interface {
	// Next returns the next piece of output. It returns io.EOF once the
	// command ended or the caller's context ended, and an error of kind
	// ErrUnavailable when the stream broke.
	Next() (Output, error)
	// ExitCode returns the command's exit code once Next returned io.EOF
	// because the command ended; otherwise an error of kind ErrUnavailable.
	ExitCode() (int, error)
	Close() error
}

// TerminalSize is a terminal's size in characters.
type TerminalSize struct {
	Cols uint32
	Rows uint32
}

// Terminal is a command running on a terminal: writes are its input, reads
// its output.
type Terminal interface {
	io.ReadWriter
	Resize(size TerminalSize) error
	// ExitCode waits until the session ends and returns the command's exit
	// code, or an error of kind ErrUnavailable when the session broke.
	ExitCode() (int, error)
	Close() error
}

// FileInfo describes a file or directory of a sandbox.
type FileInfo struct {
	// Path is absolute.
	Path string
	Dir  bool
	// Size is the size of a file in bytes, 0 for a directory.
	Size int64
	// Mode holds the permission bits.
	Mode fs.FileMode
}

// LogSource names the log a LogEntry comes from.
type LogSource string

// The log sources.
const (
	SourceProcess LogSource = "process"
	SourcePolicy  LogSource = "policy"
)

// LogQuery selects the log entries Logs returns.
type LogQuery struct {
	// Source is the log read; "" reads both.
	Source LogSource
	// Since, when not zero, leaves out the entries before it.
	Since time.Time
	// Tail, when above 0, keeps only the last Tail entries already written.
	Tail int
	// Follow keeps the stream open for new entries until the caller's
	// context ends.
	Follow bool
}

// LogEntry is a line of the main process's output or an entry of the policy
// log.
type LogEntry struct {
	Time   time.Time
	Source LogSource
	// Stream and Text are those of a process line.
	Stream OutputStream
	Text   string
	// Action ("allow" or "deny"), Program, Host, Port, Method, Path and
	// Provider are those of a policy entry; Method and Path are "" for a
	// connection, Provider is the provider of a credentialed request.
	Action   string
	Program  string
	Host     string
	Port     int
	Method   string
	Path     string
	Provider string
}

// LogStream delivers log entries.
type LogStream interface {
	// Next returns the next entry. It returns io.EOF after the last entry,
	// or for a followed stream once the caller's context ended, and an error
	// of kind ErrUnavailable when the stream broke.
	Next() (LogEntry, error)
	Close() error
}
