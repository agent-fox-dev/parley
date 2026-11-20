package runtimetest

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// ToolDefinition is one tool as a tool server prints it in its list.
type ToolDefinition struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
}

// writingTools are the tools a tool server leaves out under --read-only.
var writingTools = []string{"write_file", "edit_file", "execute", "run_command"}

// toolNames are the tools of the stand-in, in the order it lists them.
var toolNames = []string{
	"read_file", "write_file", "edit_file", "list_files", "find_files", "search_files", "file_outline",
	"find_symbol", "find_references", "execute", "run_command", "code_search", "code_mode",
}

// ToolDefinitions returns what the stand-in tool server lists: thirteen
// tools, or the nine that change nothing when readOnly. Every tool has an
// input schema of its own; code_mode has no output schema.
func ToolDefinitions(readOnly bool) []ToolDefinition {
	var defs []ToolDefinition
	for _, name := range toolNames {
		if readOnly && slices.Contains(writingTools, name) {
			continue
		}
		d := ToolDefinition{
			Name:        name,
			Description: "The stand-in's " + name + ".",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"` + name + `_argument":{"type":"string"}},"additionalProperties":true}`),
		}
		if name == "code_mode" {
			d.OutputSchema = json.RawMessage(`null`)
			if readOnly {
				d.Description += " Bound to the tools that change nothing."
			}
		} else {
			d.OutputSchema = json.RawMessage(`{"type":"object","properties":{"` + name + `_result":{"type":"string"}}}`)
		}
		defs = append(defs, d)
	}
	return defs
}

// ToolCall is one run of the stand-in's call form, as the hub started it.
type ToolCall struct {
	// Sandbox is the sandbox the command ran in.
	Sandbox string
	// Command is the whole command, and WorkDir, Env and Stdin the rest of
	// its request; Env holds the sandbox's environment and the request's.
	Command []string
	WorkDir string
	Env     map[string]string
	Stdin   []byte
	// Request, Dir, IndexDir and Timeout are the values of --request, --dir,
	// --index-dir and --timeout, and ReadOnly whether --read-only was given.
	Request, Dir, IndexDir, Timeout string
	ReadOnly                        bool
	// RequestFound reports whether the request file existed; RequestMode is
	// its mode and RequestBody its content.
	RequestFound bool
	RequestMode  fs.FileMode
	RequestBody  []byte
	// Name and Input are the request's members.
	Name  string
	Input json.RawMessage
}

// ToolListing is one run of the stand-in's tools form.
type ToolListing struct {
	Sandbox  string
	Command  []string
	WorkDir  string
	Stdin    []byte
	ReadOnly bool
}

// ToolServer is a stand-in for the tool server of a sandbox image: Run
// answers the commands the hub runs for a sandbox's tools as the real
// program prints them. `<path> tools [--read-only]` prints ToolDefinitions,
// and `<path> call --request <file> …` reads the request file the hub
// uploaded and prints {"ok": true, "data": {"tool", "input"}}; `rm -f` of a
// file removes it from the sandbox. A test replaces either answer with
// SetTools and SetCall.
type ToolServer struct {
	f    *Fake
	path string

	mu       sync.Mutex
	tools    func(ctx context.Context, l ToolListing, stdout, stderr io.Writer) int
	call     func(ctx context.Context, c ToolCall, stdout, stderr io.Writer) int
	listings []ToolListing
	calls    []ToolCall
	removed  []string
}

// NewToolServer returns a stand-in for the tool server at path in the
// sandboxes of f.
func NewToolServer(f *Fake, path string) *ToolServer {
	return &ToolServer{f: f, path: path}
}

// Path is the path the stand-in answers at.
func (s *ToolServer) Path() string { return s.path }

// SetTools makes fn answer every later run of the tools form; nil restores
// the stand-in's own answer.
func (s *ToolServer) SetTools(fn func(ctx context.Context, l ToolListing, stdout, stderr io.Writer) int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = fn
}

// SetCall makes fn answer every later run of the call form; nil restores
// the stand-in's own answer.
func (s *ToolServer) SetCall(fn func(ctx context.Context, c ToolCall, stdout, stderr io.Writer) int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.call = fn
}

// Listings returns the runs of the tools form so far, in order.
func (s *ToolServer) Listings() []ToolListing {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.listings)
}

// Calls returns the runs of the call form so far, in order.
func (s *ToolServer) Calls() []ToolCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Removed returns the paths `rm -f` was run on so far, in order.
func (s *ToolServer) Removed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.removed)
}

// Handles reports whether Run answers the command itself: one of the tool
// server's, or an rm -f.
func (s *ToolServer) Handles(command []string) bool {
	if len(command) == 0 {
		return false
	}
	return command[0] == s.path || (len(command) == 3 && command[0] == "rm" && command[1] == "-f")
}

// Run is the stand-in as the script of a Fake (SetExec), which answers the
// commands Handles reports and exits 0 without output for any other.
func (s *ToolServer) Run(ctx context.Context, sandbox string, req runtime.ExecRequest, stdout, stderr io.Writer) int {
	return s.Or(nil)(ctx, sandbox, req, stdout, stderr)
}

// Or returns a script that answers the commands Handles reports and leaves
// every other command to next; a nil next exits 0 without output.
func (s *ToolServer) Or(next ExecFunc) ExecFunc {
	return func(ctx context.Context, sandbox string, req runtime.ExecRequest, stdout, stderr io.Writer) int {
		cmd := req.Command
		switch {
		case !s.Handles(cmd):
			if next == nil {
				return 0
			}
			return next(ctx, sandbox, req, stdout, stderr)
		case cmd[0] == "rm":
			s.mu.Lock()
			s.removed = append(s.removed, cmd[2])
			s.mu.Unlock()
			s.f.RemoveSandboxFile(sandbox, cmd[2])
			return 0
		case len(cmd) >= 2 && cmd[1] == "tools":
			return s.runTools(ctx, sandbox, req, stdout, stderr)
		case len(cmd) >= 2 && cmd[1] == "call":
			return s.runCall(ctx, sandbox, req, stdout, stderr)
		}
		_, _ = io.WriteString(stderr, "usage: "+s.path+" tools|call\n")
		return 2
	}
}

func (s *ToolServer) runTools(ctx context.Context, sandbox string, req runtime.ExecRequest, stdout, stderr io.Writer) int {
	l := ToolListing{
		Sandbox: sandbox, Command: slices.Clone(req.Command), WorkDir: req.WorkDir, Stdin: req.Stdin,
		ReadOnly: slices.Contains(req.Command[2:], "--read-only"),
	}
	s.mu.Lock()
	s.listings = append(s.listings, l)
	fn := s.tools
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, l, stdout, stderr)
	}
	_ = json.NewEncoder(stdout).Encode(ToolDefinitions(l.ReadOnly))
	return 0
}

func (s *ToolServer) runCall(ctx context.Context, sandbox string, req runtime.ExecRequest, stdout, stderr io.Writer) int {
	c := ToolCall{Sandbox: sandbox, Command: slices.Clone(req.Command), WorkDir: req.WorkDir, Env: req.Env, Stdin: req.Stdin}
	args := req.Command[2:]
	for i := 0; i < len(args); i++ {
		value := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch args[i] {
		case "--request":
			c.Request = value()
		case "--dir":
			c.Dir = value()
		case "--index-dir":
			c.IndexDir = value()
		case "--timeout":
			c.Timeout = value()
		case "--read-only":
			c.ReadOnly = true
		}
	}
	c.RequestBody, c.RequestMode, c.RequestFound = s.f.SandboxFile(sandbox, c.Request)
	var request struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	readable := c.RequestFound && json.Unmarshal(c.RequestBody, &request) == nil
	c.Name, c.Input = request.Name, request.Input
	s.mu.Lock()
	s.calls = append(s.calls, c)
	fn := s.call
	s.mu.Unlock()
	if !readable {
		_, _ = io.WriteString(stderr, "cannot read the request "+c.Request+"\n")
		return 2
	}
	if fn != nil {
		return fn(ctx, c, stdout, stderr)
	}
	input := c.Input
	if len(input) == 0 {
		input = json.RawMessage(`null`)
	}
	_ = json.NewEncoder(stdout).Encode(map[string]any{
		"ok": true, "data": map[string]any{"tool": c.Name, "input": input},
	})
	return 0
}

// find returns the sandbox called name in whichever workspace holds it.
// The caller holds f.mu.
func (f *Fake) find(name string) *sandbox {
	for _, sp := range f.spaces {
		if sb := sp.sandboxes[name]; sb != nil {
			return sb
		}
	}
	return nil
}

// SandboxFile returns the content and the mode of the file at the path p of
// the sandbox called name, in whichever workspace holds it; ok is false when
// there is no such sandbox or file.
func (f *Fake) SandboxFile(name, p string) (data []byte, mode fs.FileMode, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := f.find(name)
	if sb == nil || !strings.HasPrefix(p, "/") {
		return nil, 0, false
	}
	p = cleanPath(p)
	if sb.host == "" {
		e := sb.files[p]
		if e == nil || e.dir {
			return nil, 0, false
		}
		return slices.Clone(e.data), e.mode, true
	}
	hp := hostPath(sb.host, p)
	fi, err := os.Stat(hp)
	if err != nil || fi.IsDir() {
		return nil, 0, false
	}
	data, err = os.ReadFile(hp)
	if err != nil {
		return nil, 0, false
	}
	return data, fi.Mode().Perm(), true
}

// RemoveSandboxFile removes the file at the path p of the sandbox called
// name, as rm -f does: a file that is not there is not an error.
func (f *Fake) RemoveSandboxFile(name, p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := f.find(name)
	if sb == nil || !strings.HasPrefix(p, "/") {
		return
	}
	p = cleanPath(p)
	if sb.host == "" {
		if e := sb.files[p]; e != nil && !e.dir {
			delete(sb.files, p)
		}
		return
	}
	_ = os.Remove(hostPath(sb.host, p))
}

// SandboxFiles returns the paths of the files, not the directories, below
// the directory dir of the sandbox called name, sorted.
func (f *Fake) SandboxFiles(name, dir string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := f.find(name)
	if sb == nil {
		return nil
	}
	dir = cleanPath(dir)
	prefix := strings.TrimSuffix(dir, "/") + "/"
	var out []string
	if sb.host == "" {
		for p, e := range sb.files {
			if !e.dir && strings.HasPrefix(p, prefix) {
				out = append(out, p)
			}
		}
		slices.Sort(out)
		return out
	}
	root := hostPath(sb.host, dir)
	_ = fs.WalkDir(os.DirFS(root), ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, prefix+p)
		}
		return nil
	})
	slices.Sort(out)
	return out
}
