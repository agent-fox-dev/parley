# 13. Render the web interface on the server, with no build of its own

## Status

Accepted.

## Context

The web interface has to sign a person in, list their workspaces and
show one workspace's state, in both themes, at a phone's width, from
a keyboard — and it has to run unchanged on both deployment targets:
a Cloudflare Worker and a host process.

The obvious shape is the usual one: a front-end framework, a
bundler, a client-side router, a static asset pipeline and an
accessibility test harness in a headless browser. That is five
dependencies and a second build before the first page exists.

Three things rule it out here.

**The service is one Hono app on two targets.** A Worker serves no
file system; every asset would have to be embedded or uploaded, and
the two targets would serve the interface differently. An interface
that is routes of the same app is served by both by construction.

**`hono/jsx` is already installed.** Hono renders JSX on the server
with no bundler and no React: a page is a function returning a
string of HTML. The project's one TypeScript build already compiles
it, and `make check` already runs the suites that test it.

**Nothing may be installed.** The fast gate runs with no network —
the gate's own self-test strips the npm registry to prove it — so a
dependency the interface needs is a dependency the gate cannot get.

## Decision

- **Pages are `hono/jsx` components rendered on the server.** No
  bundler, no client framework, no hydration. The only JavaScript the
  browser runs is a few lines for the theme toggle, inline.
- **`web/` is part of the one TypeScript project**, not a package of
  its own: `tsconfig.json` gains `jsx: react-jsx` and
  `jsxImportSource: hono/jsx`, and `web` joins the lint and test
  globs. The spec names a `web/package.json`; it would describe a
  package that does not exist.
- **The design tokens are one CSS file** of custom properties, served
  by a route, with both palettes in it. A page sets no type size or
  spacing of its own.
- **What a browser is needed for is tested in a browser, and
  nowhere else.** Contrast is arithmetic over the tokens and is
  measured in the unit suites; the landmarks, the heading structure
  and the accessible names are measured over the rendered HTML;
  layout at 360 pixels and real focus order need a browser, and
  those are the smoke tests, which skip where there is none.

## Consequences

The interface has no client-side state, so every view is a fresh
render and a reload is the refresh — which the PRD already accepts:
live updates are a later package.

The accessibility guarantees this package can make are the
structural ones. A page could still be unusable in ways only a
browser reveals, and the smoke tests are where that is caught; where
they do not run, the gap is an open item rather than a claim.

Adding a client framework later means adding a build, and this
decision is what would have to be revisited: nothing in the pages
assumes the absence of one beyond their own simplicity.
