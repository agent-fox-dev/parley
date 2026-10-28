---
spec_id: "15"
spec_name: "web_ui"
title: "Show a workspace's state in the browser"
status: "active"
created_at: "2026-10-27T19:08:11Z"
updated_at: "2026-10-28T15:33:08Z"
intent_hash: "9edf9cddd75b400c65b7681d8d4fc74d158fdee8573ad6d318dbcf1e52c2f436"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Show a workspace's state in the browser

## Intent

Everything the service knows is reachable through an API that only a program
can read comfortably. A person who wants to know whether their patch stack
is healthy, which agent is running, or why a sync failed should not have to
curl for it. This is the smallest honest answer: sign in, see your
workspaces, open one and read its state. It changes nothing — every write
stays on the API, where `afc` and the conformance suite already cover it —
so the UI can be useful without becoming a second surface anyone has to keep
compatible.

## Goals

- A landing page describes the service and offers sign-in.
- Signing in through the browser uses the same OAuth flow as the API and
  does **not** revoke an API key the person's automation holds.
- A dashboard lists the signed-in person's workspaces, and a workspace view
  shows that workspace's state, its patches where it carries them, its
  recent jobs and its recent events.
- The interface reads in the visual language of GitHub and GitHub Copilot,
  in a dark and a light theme, while carrying Parley's own name and marks.
- The UI shows only what the signed-in person's own credential may read.

## Non-goals

- **Any write.** No route of this package changes anything: no create, no
  sync, no merge, no rebuild, no settings. Every change stays on the API.
- **Being a second API.** The UI reads through the service's own API with
  the person's session, rather than through a private path into the store.
- **Impersonating GitHub.** The interface follows GitHub's visual language
  because that is what this audience reads fluently; it carries Parley's
  name and marks and never presents itself as GitHub.
- **Live updates.** The workspace view reads when it is opened and when a
  person reloads it; subscribing it to the event stream is a later PRD.
- **Administration.** Managing users and organizations stays on
  `afc admin`.

## Background

The whole API exists by now, and `12_audit_and_events` gives the events the
workspace view shows.

One detail shapes the whole package. `POST /api/v1/auth/callback` **revokes
every existing API key of the user** — so if the browser signed in by
calling it, a developer opening the dashboard would silently kill the key
their agent was running with. The browser therefore gets a session of its
own, established through the same OAuth provider but not through the route
that issues and revokes API keys (decision D7 of the source PRD).

The source PRD also fixes the look: GitHub's and GitHub Copilot's visual
language, in both themes, following the viewer's system setting. That is a
requirement about what a person sees, and it is written here the way the
rest of this package is written — as things that can be checked.

## Requirements

### The landing page

- The landing page says what the service is and what it does, in a few
  sentences, and offers sign-in.
- It is served to a person who is not signed in, and never shows a
  workspace, a slug or any other detail of the deployment's contents.

### Signing in

- Sign-in uses the same OAuth provider as the API and ends with the browser
  holding a session.
- It does not issue an API key and does not revoke one: a key the person's
  automation holds keeps working across a browser sign-in.
- The session identifies the same user the API would, is carried in a way a
  third-party site cannot replay, expires, and can be ended by signing out.
- A person who is not signed in, or whose session has expired, sees the
  landing page rather than an error.

### The dashboard

- The dashboard lists the signed-in person's workspaces, each with its slug,
  its status, the access they have to it and when it last synced, and links
  to the workspace's view.
- It lists exactly the workspaces the same person's credential would be
  given by the API, and no more.
- A person with no workspaces sees a page saying so rather than an empty
  frame.

### The workspace view

- The workspace view shows the workspace's fields: its slug, display name,
  description, status, mode, head, upstream head, last sync, any clone or
  sync error, and its settings.
- For a `carry_patch` workspace it also shows the patch list with each
  patch's position, branch, status and latest rebuild result, and the latest
  rebuild.
- It shows the workspace's most recent jobs and its most recent audit
  events.
- A failure to read any one of those sections leaves the rest of the page
  usable and says which part could not be read.

### Reading nothing the person may not

- Every read the UI makes is one the person's own session is entitled to
  make, through the service's own API.
- A workspace the person may not read does not appear and cannot be opened
  by guessing its address.
- A member of a workspace's organization sees what the API gives a member,
  and is shown nothing the API would refuse them.

### Look and feel

- The interface follows the visual language of GitHub and GitHub Copilot in
  its density, its type scale, its spacing and its status colours, and in
  its patterns for a list of repositories, a status badge and a timeline.
- It has a dark and a light theme, following the viewer's system setting and
  overridable, with every text and background pair meeting the contrast
  level for normal text.
- Every page works at a phone's width with no horizontal scrolling.
- Every interactive element is reachable and operable from the keyboard, and
  each page has one main landmark, a heading structure with no skipped level
  and an accessible name for every control.
- The interface carries Parley's own name and marks, and nowhere presents
  itself as GitHub or implies an association.

### End-to-end flows

1. **A developer signs in and finds a workspace.** A person opens the
   landing page, signs in with the provider, and is shown the dashboard with
   their workspaces; their agent's API key, held throughout, still works
   afterwards.
2. **A developer reads a patch stack's health.** From the dashboard they
   open a `carry_patch` workspace and see its state, its patches with their
   latest rebuild results, the latest rebuild and the most recent events —
   enough to tell whether the stack is healthy without running a command.
3. **A member sees only what they may.** A person who is a member of an
   organization signs in, sees that organization's workspaces alongside
   their own, opens one, and is shown the member's view; a workspace of
   neither kind is absent and cannot be opened by address.

## Design Decisions

1. **The browser session is not an API key.** The API's sign-in revokes
   every existing key of the user, so sharing that path would make opening
   the dashboard a destructive act for anyone running an agent.
2. **The UI reads through the API, not past it.** One access rule, already
   specified and already tested, stays the only one. A private read path
   would be a second place for the member and bound-token rules to be got
   wrong.
3. **Read-only, on purpose.** Every write is already covered by `afc` and
   the conformance suite; adding write paths here would add CSRF handling,
   confirmation flows and a second surface to keep compatible, for
   capabilities that already exist.
4. **The look is specified as checkable properties** — themes, contrast,
   phone width, keyboard reachability — rather than as a resemblance that
   cannot be tested. The resemblance is the intent; these are what can be
   verified.
5. **No live updates in this package.** The event stream exists, but a page
   that subscribes to it needs reconnection, backfill and ordering rules of
   its own. Reading on open is honest and small.

## Dependencies

- `01_service_foundation` for the service that serves the pages and the two
  deployment targets it must run on.
- `02_identity_and_credentials` for the OAuth provider the browser sign-in
  shares, and for the user it identifies.
- `03_workspaces` for the workspaces and the access rules the UI inherits.
- `09_carry_patch` for the patch list and the rebuilds the workspace view
  shows.
- `10_merges` for the jobs it shows, and `12_audit_and_events` for the
  recent events.
- `docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md` goal G3 and
  decision D7 are normative for this package; `docs/prd/api.md` describes
  the routes it reads.

## Verified External API

Nothing was verified against installed code. The source PRD names React and
Vite as suggestions and no framework has been chosen; neither has a session
mechanism, a theming approach or an accessibility testing tool. The first
task of this package chooses them, verifies each against its installed
types, and records the choice. The contrast and keyboard requirements above
are stated so that whichever tool is chosen can check them.
