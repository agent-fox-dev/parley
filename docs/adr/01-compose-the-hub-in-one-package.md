# 01. Compose the hub in one package

## Status

Accepted.

## Context

`parleyd` is one process built from many packages. Each of them brings
configuration keys, schema steps for the stores, work to run at start,
maintenance tasks, REST routes and the checks of the request pipeline
(authentication, the binding guard, the scope check, the access checks).
The start must run these in one fixed order: configuration, data,
bootstrap, start work, maintenance, listener.

Three places could compose them: `cmd/parleyd`, each feature package
reaching into the others, or one package that does nothing else. A `main`
package cannot be imported, so tests and `test/e2e` could not start a whole
hub in-process; and feature packages that wire each other up would make the
start order a property of import order, written down nowhere.

## Decision

`internal/hub` is the one place that composes the process.

- `hub.Run(ctx, hub.Options) int` is `parleyd`. It takes the arguments, an
  environment lookup, the working directory, stdout and stderr, and returns
  the exit status. `cmd/parleyd` is a thin `main` that passes the process's
  inputs and is not edited when a package plugs in.
- A package plugs in through a *part*, a `func(*hub.Composition)`, written in
  a file of its own in `internal/hub` (`identity.go`, `access.go`, ...).
  The part registers on the `Composition`: configuration keys and whole
  sections, schema steps, start hooks, maintenance tasks, routes, surfaces
  outside the REST API, and the pipeline's checks.
- The parts are listed explicitly, in order, in the `parts` variable of
  `internal/hub/hub.go`; a package appends its part there. The list, not
  `init` functions, decides the order, because the order of schema steps and
  start hooks matters and `init` order would follow file names.
- Start hooks and maintenance tasks receive a `*hub.Runtime` (the loaded
  configuration, the open stores, the logger, the scrubber and the clock).
  Code that runs at request time keeps `(*Composition).Runtime()`, which the
  hub fills before the first hook runs and before the first request.
- A registration that is refused (a hook or task without a name or function,
  a name registered twice, a key or section the configuration registry
  refuses, a route the server refuses) is a start error: exit status 1 and
  one error line, before anything is opened. It never panics the process.
- `hub.Options` also holds the fields tests need to control the hub without
  changing what it does: `Compose` (parts added after the hub's own), `Port`,
  `Now` and the time limits. `parleyd` leaves them zero.

## Consequences

- A test or `test/e2e` starts a complete hub with `hub.Run`, on a temporary
  working directory, with parts of its own added through `Options.Compose`.
- Adding a package to the hub touches its own file in `internal/hub` and
  one line of `parts`; the start order of every package is readable in one
  place.
- `internal/hub` imports every feature package it composes, so no feature
  package may import `internal/hub`; what they share (configuration, stores,
  scrubbing, the server) lives in their own packages.
- The flags and the `backup` command are parsed in `internal/hub` as well, so
  `cmd/parleyd` holds no behaviour of its own.
