# 3. Hash credential secrets with one pass of SHA-256

## Status

Accepted.

## Context

Parley holds three kinds of credential: API keys, personal access tokens and
the deployment's admin token. Every authenticated request presents one, and
the service has to decide in the same request whether the secret it carries
matches what was issued. No response may return a secret, and the store
should not hold one either, so what is kept has to be a one-way function of
it.

The obvious reflex is a password hash — bcrypt, scrypt, Argon2 — because
that is what a stored secret usually calls for. Those functions are slow on
purpose: they exist to make guessing a _human-chosen_ password expensive.
That cost would be paid here on **every** authenticated request, inside a
Worker's per-request CPU budget, for a secret no person chose.

## Decision

A credential secret is 32 bytes from `crypto.getRandomValues`, rendered as
64 hex characters, and the store holds one pass of SHA-256 over it, in hex.
The credential's identifier is kept in clear beside it, so a presented
credential is found by primary key rather than by scanning and comparing
every row.

Comparison is a fixed-time digest comparison, so the time an answer takes
does not say how much of a digest matched.

## Consequences

- Guessing a secret means searching 2^256, which no iteration count would
  improve on. The property a password hash buys — surviving a low-entropy
  input — is one this input cannot lack, because the service generates it.
- Verification costs one SHA-256 over 64 bytes, which is negligible against
  the per-request budget on both targets.
- A stolen database yields no usable credential, which is what the
  reference's "no response carries a secret" rule is for.
- The scheme is only sound while the secret stays machine-generated. A
  credential a person could choose — were one ever added — must not reuse
  it; that would need a password hash and a decision of its own.
