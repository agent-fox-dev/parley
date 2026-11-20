# Serve a sandbox's tools over MCP and REST

## Intent

An agent or script outside a sandbox can use the file, search and shell
tools on the sandbox's working directory through the hub, with nothing but a
Parley credential: a harness that speaks MCP connects to one URL, and a
script calls two REST routes. The tools run inside the sandbox, so what they
can read, write and reach is what the sandbox allows, and the hub decides who
may call which of them.

## Goals

1. An MCP client given the URL `…/workspaces/:slug/sandboxes/:id/mcp` and a
   Parley credential lists the sandbox's tools and calls them, with no other
   configuration.
2. A script lists the tools with one `GET` and calls one with one `POST`.
3. A caller who may run commands in the sandbox gets every tool. A caller
   who may only read the workspace gets the tools that change nothing, and
   cannot reach the others by either route.
4. A second `code_search` call on the same sandbox does not build the index
   again.
5. Every tool call that ran leaves one hub event naming the tool, its
   outcome and its duration, and never its arguments or its result.
6. A sandbox whose profile names no tool server answers
   `404 tools_unavailable` on all three routes, and nothing else about it
   changes.

## Non-goals

- A `parley sandbox tool` CLI command. Scripts use the REST routes.
- `parley mcp`, the CLI serving Parley's own operations with code mode. It
  is PRD 19.
- Search, outlines and symbols on the hub without a sandbox. It is PRD 20.
- Building or publishing the tool server or an image that holds it. The
  operator's image holds `agentkit-tools`; it comes from the agentkit-go
  repository and its CI/CD.
- A held-open tool session per client. Every call is its own process in the
  sandbox, because the runtime's exec carries no input.
- MCP features other than tools: no resources, prompts, sampling,
  elicitation or server-to-client requests, and no MCP authorization flow
  (OAuth). The credential is a bearer token.
- Streaming a tool's output while it runs. A call answers when it ends.
- Events for the calls a `code_mode` script makes inside its one call.
- Any change to exec, files, logs or attach.

## Background

Parley's sandboxes (PRD §13, packages 44 to 48) give an agent four ways to
act in one: exec, file upload and download, logs, and a terminal. An agent
that runs inside the sandbox needs none of them. An agent that runs outside
has to build its own file reading, searching and editing out of exec and
tar streams, and each harness would do that differently.

agentkit-go has those tools: `read_file`, `write_file`, `edit_file`,
`list_files`, `find_files`, `search_files`, `file_outline`, `find_symbol`,
`find_references`, `execute`, `run_command`, the indexed `code_search`, and
`code_mode`, which runs a model-written script over the others. Its PRD 15
(`docs/prd/15-serve-the-tools-from-one-command-and-drop-the-sandbox.md` in
that repository, pull request 185, not built yet) adds the command
`agentkit-tools`, which serves them for one directory. This PRD uses three
of its forms:

- `agentkit-tools tools` prints the tool definitions as a JSON array of
  `{name, description, input_schema, output_schema}`.
- `agentkit-tools call --request <file>` reads `{"name", "input"}` from a
  file, makes the call, prints one JSON object (`ok`, `data`, `error`,
  `detail`, `metadata`, and `text` when the model-facing text differs) and
  exits 0 whether the tool succeeded or failed; it exits 2 for a request it
  cannot read.
- `--dir`, `--index-dir`, `--read-only` and `--timeout` select the served
  directory, where the search index is kept between processes, the set
  without `write_file`, `edit_file`, `execute` and `run_command`, and the
  bound on one call.

On the hub, every route that uses a sandbox passes one gate
(`internal/sandbox/use.go`): scope and workspace access, the no-widening
rule of SBX-FR-012 for routes that act in the sandbox, the state check, and
the count of concurrent uses. The runtime client
(`internal/sandbox/runtime`) offers `Exec`, which runs a command and
collects its output, and `Upload`, which writes a file. The OpenShell client
refuses an exec with standard input, because the SDK's run takes none, so
the request of a tool call has to reach the sandbox as a file.

The hub has no MCP code today and `go.mod` holds no MCP module.

## Requirements

### Enabling: the profile names the tool server

A sandbox profile gains one field, `tool_server`: the absolute path of
`agentkit-tools` in the profile's image. The default is empty, which means
the profile's sandboxes have no tools.

- Static validation refuses a `tool_server` that is not an absolute path, or
  is longer than 256 bytes, with a message naming the profile and the key.
  The hub does not check at start that the image holds the file.
- `GET /sandbox-profiles` shows `tool_server` as a boolean, `tools`. The
  sandbox object gains `tools` (bool), a snapshot of the profile at
  creation like `image`.
- On a sandbox with `tools: false`, the three routes of this PRD answer
  `404`, `error_type: tools_unavailable`, after the scope and access checks.
- With the feature off (no `[sandbox]` section) they answer
  `404 sandboxes_disabled`, like every sandbox route.

### Who gets which tools

There are two tool sets. The **full set** is every tool the tool server
serves. The **read-only set** is the full set without `write_file`,
`edit_file`, `execute` and `run_command`, with `code_mode` bound to the
rest.

- A caller gets the full set when it would be allowed to exec in the
  sandbox: `sandboxes:write`, write access to the workspace, and a
  credential that covers the sandbox's hub credential (SBX-FR-012).
- Any other caller with `sandboxes:read` and read access to the workspace
  gets the read-only set. This includes a caller with write access whose
  credential does not cover the hub credential.
- A caller with neither is refused as on the logs route: `401`,
  `403 insufficient_scope`, `403 forbidden` or `404`.
- Workspace-bound tokens may use the routes, under the same rules.
- The set is decided per request. Listing shows exactly the caller's set,
  and a call to a tool outside it is refused before anything runs in the
  sandbox.
- The routes serve a sandbox in state `ready` only. In any other state they
  answer `409 sandbox_not_ready`.

### What one call does in the sandbox

For a tool call the hub writes the request to a file in the sandbox, runs
the tool server once on it, reads the result, and removes the file.

- The request file and the search index live under one directory outside
  the working directory, `/tmp/parley-tools`, so neither is ever part of a
  checkpoint or a commit. The request file is readable by the workload user
  only and has a name the hub chooses at random.
- The tool server runs as the workload user, in the working directory, with
  the sandbox's environment, as an exec does. It is given the working
  directory as the served directory, the index directory, the read-only
  switch when the caller has the read-only set, and the call's time bound.
- The request file is removed after the call, whether it succeeded or not.
  A failed removal is logged and does not change the answer.
- Arguments are at most 4 MiB as JSON; more is `413`. A result of more than
  4 MiB is a failed call (`tool_server_failed`, below).
- A call's time bound is the request's `timeout` (REST) or 10 minutes
  (MCP), cut to `[sandbox].max_exec_timeout`. When it passes, the answer is
  a tool result with `ok: false`, `error: "timeout"`.
- Each call counts as one of the sandbox's concurrent execs
  (`[sandbox].max_concurrent_execs`); one over the limit is `429`.
- Each call sets `last_activity_at`. Listing does not.
- The tool definitions are read from the tool server once per sandbox and
  set, and kept until the sandbox leaves `ready`.
- **When the tool server fails.** If it cannot be started, exits with a
  status other than 0, or prints something that is not one JSON object of
  the expected shape, the call (or the listing) fails with
  `error_type: tool_server_failed` and a message of at most 1,024 bytes,
  scrubbed as SBX-FR-064 says. When the runtime cannot be reached it is
  `sandbox_runtime_error`, as for exec.
- A tool that fails (a file not found, a non-unique edit, a command the
  tool reports as failed) is not a failed call: the caller gets the tool's
  result with `ok: false` and the tool's error code.
- The hub does not redact results: like exec output, a result may hold
  whatever the sandbox holds, exposed secrets included.

### REST routes

`GET /workspaces/:slug/sandboxes/:id/tools`

- Lists the caller's tool set: `200`
  `{"set": "full" | "read_only", "tools": [{"name", "description", "input_schema", "output_schema"}]}`,
  in the tool server's order. `output_schema` is null for a tool without
  one.
- Errors: the access errors above; `404 sandbox_not_found`;
  `404 tools_unavailable`; `409 sandbox_not_ready`;
  `502 tool_server_failed`; `502 sandbox_runtime_error`.

`POST /workspaces/:slug/sandboxes/:id/tools/:name`

- Body: `{"input"?: object, "timeout"?: duration}`. `input` defaults to
  `{}`. `timeout` defaults to 60 s.
- Response: `200`
  `{"ok": bool, "data": object, "error"?: string, "detail"?: string, "text"?: string, "duration_ms": int}`.
  A tool failure and a timeout are `200` with `ok: false`.
- Errors: `400` for a body that is not such an object or a `timeout` that
  is not a positive duration; `403 insufficient_scope` for a tool of the
  full set called by a caller with the read-only set;
  `404`, `error_type: tool_not_found` for a name that is in neither set;
  `413`; `429`; and those of the listing.

### The MCP route

`POST /workspaces/:slug/sandboxes/:id/mcp` is an MCP server over Streamable
HTTP for that one sandbox.

- It is stateless: it sets and reads no session id, each request stands
  alone, and a hub restart between two requests is not visible to the
  client. `GET` and `DELETE` answer `405`. Responses are `application/json`.
- The credential is the request's `Authorization: Bearer` header, any
  credential the REST routes accept.
- `initialize` answers with the server name `parley`, the hub's version,
  the tools capability, and instructions of one sentence naming the
  workspace and the sandbox id. `ping` answers.
- `tools/list` answers with the caller's set: each tool's name,
  description, input schema and, where it has one, output schema, as the
  tool server gave them.
- `tools/call` makes the call as above. The result's `isError` is the
  negation of `ok`. Its text content is the tool server's `text` when
  there is one, and otherwise the JSON of `data` for a success and
  `<error>: <detail>` for a failure. `structuredContent` is `data` when the
  tool has an output schema.
- A `tools/call` naming a tool outside the caller's set, or unknown, is a
  JSON-RPC error with code `-32602` whose message names the tool. A failed
  call (`tool_server_failed`, `sandbox_runtime_error`) is a JSON-RPC error
  with code `-32603` and the scrubbed message.
- What is decided before the MCP message is read is answered as plain HTTP
  with the hub's error object, not as JSON-RPC: `401`, `403`, `404`
  (`sandbox_not_found`, `tools_unavailable`, `sandboxes_disabled`),
  `409 sandbox_not_ready`, `413` for a body over 8 MiB, and `429` for the
  rate limit. A `tools/call` over the concurrent-exec limit is a JSON-RPC
  error with code `-32000` and the message `sandbox busy`.
- Any other MCP method answers the protocol's method-not-found error.

### Events and metrics

- Every tool call that was started in the sandbox emits one hub event,
  `parley.sandbox.tool_call`, when it ends: resource type `sandbox`,
  resource id the sandbox id, the workspace's normal visibility, the
  caller as actor, the sandbox's `session_id`, and metadata
  `{"tool", "ok", "error"?, "duration_ms", "via": "mcp" | "rest", "set": "full" | "read_only"}`.
  `error` is the tool's error code, `timeout`, `tool_server_failed` or
  `sandbox_runtime_error`. Arguments and results never appear.
- A request refused before anything ran (access, state, limit, unknown
  tool) emits no event. Listing emits none.
- A `code_mode` call is one event, whatever its script called.
- Two metrics are new: `parley_sandbox_tool_calls_total`, by tool and
  outcome (`ok`, `tool_error`, `timeout`, `failed`), and
  `parley_sandbox_tool_call_duration_seconds`.

### End-to-end flows

1. **A harness outside the sandbox.** A developer creates a fork with a
   sandbox of a profile that names the tool server, and configures Claude
   Code with the sandbox's MCP URL and the fork's token. Claude Code lists
   thirteen tools, searches with `code_search`, edits with `edit_file`,
   runs the tests with `execute`, and pushes with `execute`. The hub's
   event stream shows one `parley.sandbox.tool_call` per call. The
   sandbox's checkpoint holds the edit and no index or request file.
2. **A reviewer.** An org member with read access lists the tools of a
   colleague's sandbox over REST, gets the read-only set, reads a file with
   `read_file`, and is refused `403 insufficient_scope` on `execute`.
3. **A script.** A CI script calls `POST …/tools/search_files` with a
   pattern and reads the matches from `data`.
4. **A profile without tools.** The same calls on a sandbox of a profile
   without `tool_server` answer `404 tools_unavailable`.

## Design Decisions

1. **The hub relays; the tools run in the sandbox** (owner's decision). The
   hub implements MCP and turns each call into one run of the tool server.
2. **One process per call** (decided after checking the runtime): the
   OpenShell SDK's exec takes no input, so a held-open session is not
   possible. The index persists in the sandbox to make that cheap.
3. **The profile opts in with `tool_server`** (owner's decision).
4. **REST routes beside MCP, no CLI command** (owner's decision).
5. **Read access gets the read-only set** (owner's decision). A caller that
   fails the no-widening rule falls back to it instead of being refused,
   because it may already read the sandbox's files and logs.
6. **One hub event per call** (owner's decision), without arguments or
   results, which may hold file contents and secrets.
7. **Stateless MCP with JSON responses.** It needs no session store, and
   hub restarts stay harmless (SBX-US-09).
8. **Constraint: the MCP protocol comes from the official Go SDK**
   (`github.com/modelcontextprotocol/go-sdk`), which agentkit-go already
   uses, and is not written by hand. It is a new direct requirement of the
   hub.
9. **Fixed scratch directory `/tmp/parley-tools`**, not a profile field: an
   operator has no reason to choose it, and it must only be outside the
   working directory and writable by the workload user. An index lost with
   `/tmp` is rebuilt by the next search.
10. **The hub does not check for the binary at start**, because it would
    have to create a sandbox per profile to look. A profile whose image
    lacks it shows on first use as `tool_server_failed`.
11. **Timeout defaults differ by route**: 60 s for REST, as exec, since a
    script can pass its own; 10 minutes for MCP, where the client cannot.
12. **Works on any runtime**: only `Exec` without input and `Upload` are
    used, so a second runtime behind the same client needs nothing more.

## Dependencies

- Packages 44 `sandbox_runtime_and_profiles`, 45
  `sandbox_creation_and_credentials`, 46 `sandbox_exec_files_and_logs`,
  47 `sandbox_lifecycle_and_checkpoints` (all active, every task done):
  the profile, the sandbox object, the use gate, the no-widening rule, the
  runtime client and the checkpoint this builds on. Their intent is not
  changed; the profile and the sandbox object each gain one field.
- Package 52 `durable_event_log_and_stream` (active, not yet implemented):
  carries the new event type like every other; this PRD does not wait for
  it.
- agentkit-go PRD 15 (pull request 185, not built): the `agentkit-tools`
  command and its `tools` and `call` forms. The hub's tests use a stand-in
  program with the same output; the end-to-end flow needs the real one.
- `docs/errata/46_sandbox_exec_files_and_logs.md`: `sandboxes:write`
  satisfies `sandboxes:read`, which the set rule relies on.

## Verified External API

`github.com/modelcontextprotocol/go-sdk` v1.8.0, package `mcp`, read with
`go doc` in the module cache (it requires `go 1.25.0`; the hub is on 1.27):

- `func NewServer(impl *Implementation, options *ServerOptions) *Server`
- `type Implementation struct { Name, Title, Description, Version string … }`
- `ServerOptions.Instructions string`
- `func (s *Server) AddTool(t *Tool, h ToolHandler)`: the input schema must
  be non-nil with type `object`; arguments arrive as `json.RawMessage` and
  are not validated by the SDK.
- `type ToolHandler func(context.Context, *CallToolRequest) (*CallToolResult, error)`
- `type CallToolResult struct { Content []Content; StructuredContent any; IsError bool … }`
- `func NewStreamableHTTPHandler(getServer func(*http.Request) *Server, opts *StreamableHTTPOptions) *StreamableHTTPHandler`
- `StreamableHTTPOptions.Stateless bool` (no session id; `GET` and `DELETE`
  answer 405) and `StreamableHTTPOptions.JSONResponse bool`.

**Unverified**: the fields of `Tool` and `CallToolRequest`, the `Content`
implementations, and how a handler returns a JSON-RPC error with a chosen
code. **Not built**: `agentkit-tools`; its forms are as its PRD states them.

## Glossary

- **Tool server**: the program `agentkit-tools` in the sandbox's image,
  which serves agentkit-go's tools for a directory.
- **Tool set**: the tools a caller may list and call: the full set or the
  read-only set.
- **Tool call**: one call of one tool through either route; one run of the
  tool server in the sandbox.
- **Failed call**: a tool call that produced no tool result because the
  tool server or the runtime failed. A tool result with `ok: false` is not
  one.
