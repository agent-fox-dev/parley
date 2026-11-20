# Errata: 56_sandbox_tool_routes

Where the implementation of package 56
(`.specs/56_sandbox_tool_routes`) differs from what the package says, or
decides what it leaves open.

## The hub answers an unserved MCP method itself

The package takes the MCP protocol from the official Go SDK alone (design
decision 8) and asks for the protocol's method-not-found error for every
method other than `initialize`, `ping`, `tools/list` and `tools/call`. The
SDK (v1.8.0) answers a method it does not know with `400` and a plain text,
outside JSON-RPC, and it serves the methods of resources, prompts,
completion and logging whether or not the server holds any. The route
therefore reads the method of a request before the SDK does, and answers one
it does not serve with the JSON-RPC error `-32601` (`method "<m>" is not
served`), and a batch, in which such a method would reach the SDK, with
`-32600` (`batches are not served`). Notifications and everything else that
is not one request go to the SDK. These are the only JSON-RPC answers the
hub writes by hand. Code: `unservedMCPMessage` in
`internal/sandbox/tools_mcp.go`; test:
`TestMCP_AnswersAnyOtherMethodNotFound`.

## `tools/list` is answered in the tool server's order

The SDK lists a server's tools by name. The package lists them in the tool
server's order, as the REST listing does, so a receiving middleware answers
`tools/list` with the tools as they were added, on one page. Code:
`listInOrder`; test: `TestMCP_ListsTheCallersSetAsTheToolServerDefinesIt`.

## The SDK's refusal of a loopback connection under another host is off

The SDK's streamable HTTP handler refuses, with `403`, a request that
reaches a loopback address with a `Host` that is not one, as a protection
of local servers against DNS rebinding. A hub behind a reverse proxy on its
own host is reached exactly that way, and every request to the route
carries a bearer credential, which a page in a browser does not have. The
handler is built with `DisableLocalhostProtection`. The SDK's own body
limit is off too: the route's limit of 8 MiB has been applied by the body
stage before the gate.

## `GET` and `DELETE` on the MCP route are the router's `405`

The plan declares `GET` and `DELETE …/mcp` as routes of the gate that answer
`405` once its checks passed. The hub's router already answers a method a
path has no route for with `405 method_not_allowed`, before
authentication, so only `POST` is declared. A caller without a credential
gets `405` too, not `401`.

## A tool server that cannot list its tools fails every MCP request

The server of an MCP request is built from the caller's tool definitions
before the SDK reads the message. While they cannot be read, every request
to the route, `initialize` and `ping` included, is answered `502
tool_server_failed` (or `sandbox_runtime_error`) as plain HTTP. The same
answer is given to a definition the SDK does not take and panics on: an
input schema that does not have the type `object`, a schema nested deeper
than it reads, an annotation it refuses. The REST listing serves such a
definition as it is. Test:
`TestMCP_AnswersDefinitionsItCannotServeAsAFailedToolServer`.

## A token bound to another workspace is `404`, as on the logs route

The package's contract lists `403 forbidden` for a workspace-bound token of
another workspace. The binding guard answers such a token `404 not_found`
on every route of a workspace, the logs route included, and the criterion
asks for what the logs route answers, so the three routes answer `404`.
Test: `TestTools_RefuseACallerTheLogsRouteRefusesTheSameWay`.

## An abandoned MCP call gets no answer

The SDK writes a JSON-RPC error for whatever a tool handler returns. For a
call whose caller left or that ran when the hub began to stop, the route
drops what the SDK writes and then the connection, as the REST route does.
Code: `abandonableWriter`; test:
`TestToolCall_EndsACallNobodyWaitsForAndRecordsItAsFailed`.

## `input: null` is an absent input

The package refuses an `input` that is not a JSON object. The hub's bodies
read a member that is `null` as absent, and the call route does too: `null`
is the input `{}`. Any other value that is not an object is `400
invalid_request`. Over MCP, absent or `null` `arguments` are `{}`, and
`arguments` that are not an object are `-32602`.

## A tool result's `data` must be an object

The package names a result "one JSON object with a boolean ok". A result
whose `data` is present and not an object, or whose `error`, `detail` or
`text` is not a string, is a failed call (`tool_server_failed`), since the
REST answer promises `data` as an object. An absent or `null` `data` is
`{}`.

## Smaller choices

- A tool's error code is kept in the call's event up to 64 characters, and
  scrubbed like every text about a sandbox: nothing holds a tool server to
  printing a code.
- Two definitions of one name are a failed listing.
- The definitions kept of a sandbox are dropped when it leaves `ready` and
  again when it becomes `ready`, so that a request that passed the gate
  just before it left cannot leave old ones behind.

- A tool server's error output is kept up to 64 KiB for the message of a
  failed call, which is then cut to 1,024 bytes.
- The request file's removal runs under a context of its own of 10 seconds,
  and the event's write under one of 5 seconds, since both run after the
  caller may have left.
- The timeout's result carries the `detail` `the call did not end within
  <bound>`.
- The duration histogram's buckets are `0.05` to `600` seconds.
- The tests' stand-in tool server lives in
  `internal/sandbox/runtime/runtimetest` (`ToolServer`), with accessors for
  a fake sandbox's files (`SandboxFile`, `SandboxFiles`,
  `RemoveSandboxFile`).
- The catalogue of metrics holds 29 entries; the tests of
  `internal/metrics` and `test/e2e` that count it were raised from 27.
