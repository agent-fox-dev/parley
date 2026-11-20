package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The error types of the routes that serve a sandbox's tools.
const (
	// errorTypeToolsUnavailable answers a tool route of a sandbox whose
	// profile named no tool server, with 404.
	errorTypeToolsUnavailable = "tools_unavailable"
	// errorTypeToolServerFailed answers a listing or a call the tool server
	// did not answer as a tool server does, with 502.
	errorTypeToolServerFailed = "tool_server_failed"
)

// The tool sets: what a caller may list and call of a sandbox's tools.
const (
	// toolSetFull is every tool the tool server serves.
	toolSetFull = "full"
	// toolSetReadOnly is what the tool server serves under --read-only: the
	// tools that change nothing.
	toolSetReadOnly = "read_only"
)

// maxToolOutput is the most a tool server may print for one listing or one
// call.
const maxToolOutput = 4 << 20

// toolDefinition is one tool as the tool server defines it. The schemas are
// kept as the tool server printed them; OutputSchema is null for a tool
// without one.
type toolDefinition struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
}

// hasOutputSchema reports whether the tool has an output schema.
func (d toolDefinition) hasOutputSchema() bool {
	return len(d.OutputSchema) > 0 && !bytes.Equal(d.OutputSchema, []byte("null"))
}

// toolsUnavailable is the answer of a tool route of a sandbox without a
// tool server.
func toolsUnavailable() *server.Error {
	return server.NewError(errorTypeToolsUnavailable, http.StatusNotFound, "the sandbox's profile names no tool server")
}

// toolServerFailed is the answer to a listing or a call the tool server of
// the sandbox id failed, with what went wrong cleared of the sandbox's
// secret values, cut to 1,024 bytes, and cleared of the hub's.
func (s *Service) toolServerFailed(ctx context.Context, id, text string) *server.Error {
	// The caller may have left; the values are still needed for the log.
	text = s.scrub(s.ScrubSandbox(context.WithoutCancel(ctx), id, text))
	return server.NewError(errorTypeToolServerFailed, http.StatusBadGateway, text)
}

// toolSetOf decides the tool set of the caller of r on the sandbox rec, for
// this request alone. A caller who would be allowed to exec in the sandbox
// gets the full set: a PAT holds sandboxes:write, the caller may write the
// workspace, and its credential covers the sandbox's hub credential (see
// Covers). Every other caller the route's own checks let through may read
// the sandbox's files and logs already, and gets the read-only set instead
// of a refusal.
func toolSetOf(r *http.Request, level access.Level, rec Record) string {
	p, ok := identity.PrincipalOf(r.Context())
	switch {
	case !ok:
	case p.Type == identity.TypePAT && !identity.Satisfies(p.Permissions, scopeWrite):
	case level != access.LevelAdmin && level != access.LevelOwner:
	case !Covers(p, rec.HubAccess):
	default:
		return toolSetFull
	}
	return toolSetReadOnly
}

// toolDefs keeps the tool definitions the hub read from the tool servers,
// per sandbox and set, in memory: a restart of the hub reads them again.
type toolDefs struct {
	mu   sync.Mutex
	kept map[toolDefsKey]*toolDefsEntry
}

type toolDefsKey struct{ id, set string }

// toolDefsEntry is the definitions of one sandbox and set. Its lock is held
// while they are read, so that requests that arrive together read once.
type toolDefsEntry struct {
	mu   sync.Mutex
	read bool
	defs []toolDefinition
}

// entry returns the entry of the sandbox id and set, creating it.
func (t *toolDefs) entry(id, set string) *toolDefsEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.kept == nil {
		t.kept = map[toolDefsKey]*toolDefsEntry{}
	}
	k := toolDefsKey{id, set}
	e := t.kept[k]
	if e == nil {
		e = &toolDefsEntry{}
		t.kept[k] = e
	}
	return e
}

// forget drops what is kept of the sandbox id, which has left ready: its
// tool server may not be the same when it is ready again.
func (t *toolDefs) forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.kept {
		if k.id == id {
			delete(t.kept, k)
		}
	}
}

// toolDefinitions returns the definitions of set for the ready sandbox rec:
// those kept since the sandbox became ready, or else what its tool server
// lists now, which is kept unless the listing failed.
func (s *Service) toolDefinitions(ctx context.Context, rec Record, set string) ([]toolDefinition, error) {
	e := s.toolDefs.entry(rec.ID, set)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.read {
		return e.defs, nil
	}
	defs, err := s.readToolDefinitions(ctx, rec, set)
	if err != nil {
		return nil, err
	}
	e.defs, e.read = defs, true
	return defs, nil
}

// readToolDefinitions runs the tool server of rec once to list the tools of
// set, in the working directory and without input, as an exec runs a
// command. A runtime failure is returned as it is; anything else that is not
// a list of definitions is the tool-server-failed answer.
func (s *Service) readToolDefinitions(ctx context.Context, rec Record, set string) ([]toolDefinition, error) {
	command := []string{rec.Snapshot.ToolServer, "tools"}
	if set == toolSetReadOnly {
		command = append(command, "--read-only")
	}
	res, err := s.Client().Exec(ctx, rec.ID, runtime.ExecRequest{Command: command, WorkDir: WorkDir})
	if err != nil {
		return nil, err
	}
	fail := func(format string, args ...any) ([]toolDefinition, error) {
		return nil, s.toolServerFailed(ctx, rec.ID, "the tool server did not list its tools: "+fmt.Sprintf(format, args...))
	}
	if res.ExitCode != 0 {
		return fail("it exited with status %d: %s", res.ExitCode, bytes.TrimSpace(res.Stderr))
	}
	if len(res.Stdout) > maxToolOutput {
		return fail("it printed more than %d bytes", maxToolOutput)
	}
	var defs []toolDefinition
	if err := decodeOne(res.Stdout, '[', &defs); err != nil {
		return fail("it printed something that is not one JSON array of definitions")
	}
	for i := range defs {
		d := &defs[i]
		if d.Name == "" {
			return fail("definition %d has no name", i+1)
		}
		if schema := bytes.TrimSpace(d.InputSchema); len(schema) == 0 || schema[0] != '{' {
			return fail("the input schema of %q is not an object", d.Name)
		}
		if !d.hasOutputSchema() {
			d.OutputSchema = json.RawMessage("null")
		}
	}
	if defs == nil {
		defs = []toolDefinition{}
	}
	return defs, nil
}

// errNotOne is the error of output that is not exactly one JSON value of
// the kind asked for.
var errNotOne = errors.New("not one JSON value of the expected kind")

// decodeOne decodes data, which must be exactly one JSON value that begins
// with the byte open ('[' or '{'), into v.
func decodeOne(data []byte, open byte, v any) error {
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != open {
		return errNotOne
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return errNotOne
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errNotOne
	}
	return nil
}

// toolListAnswer is the answer of GET …/tools.
type toolListAnswer struct {
	Set   string           `json:"set"`
	Tools []toolDefinition `json:"tools"`
}

// listTools serves GET …/tools: the caller's set and its definitions in the
// tool server's order. A listing changes nothing, so it starts no activity,
// takes no place among the sandbox's commands and is told to nobody.
func (s *Service) listTools(w http.ResponseWriter, r *http.Request, u useRequest) error {
	defs, err := s.toolDefinitions(r.Context(), u.rec, u.set)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, toolListAnswer{Set: u.set, Tools: defs})
	return nil
}
