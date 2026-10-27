# 1. Define the platform interfaces and their implementations

## Status

Accepted.

## Context

Parley must run on Cloudflare and on a host with no Cloudflare account, and
the portability claim is only worth something if the same checks pass on
both. Every facility of the host platform is therefore reached through a
named interface, with one contract suite per interface run against every
implementation a deployment can be wired with.

Four facilities are needed by the service foundation: the relational store,
the clock, the configuration source and the metrics sink.

Two of them differ genuinely by platform:

- the **relational store** is D1 on Cloudflare and SQLite on a host;
- the **configuration source** is the Worker's bindings on Cloudflare and
  the process environment on a host.

Two of them do not. Reading the current time and counting a metric are the
same operation wherever the service runs. Writing a "Cloudflare clock" and a
"local clock" would produce two copies of `new Date()`, which is duplication
the code would have to carry for ever to satisfy a word in a requirement.

## Decision

The contract suite of every interface runs against **two implementations**.

For the relational store and the configuration source, those are the two
platform implementations.

For the clock and the metrics sink there is **one** implementation, shared by
both targets, and the second implementation the suite runs against is the
deterministic one the tests use: a fixed clock, and the in-memory sink. They
are held to the same contract as the real thing.

## Consequences

The portability claim is measured where platforms genuinely differ, and
nothing is duplicated where they do not.

Holding the test doubles to the same contract has a second benefit: a fake
that drifts from the real implementation fails the suite, which is the usual
way a fake stops being a useful stand-in.

The D1 implementation is exercised in the fast suite against a stub binding
shaped like `D1Database`, because real D1 needs a deployment. It is exercised
against real D1 by the conformance harness, at `make check-release`.
