# 4. Seal secrets with AES-GCM under a deployment key

## Status

Accepted.

## Context

A workspace carries the credentials its repository needs — a token, or a
username and a password — and later packages add user, organization and
workspace secrets of their own. Unlike a credential Parley issues
([ADR 3](03-hash-credential-secrets-with-one-pass-of-sha-256.md)), these have
to be **read back**: a clone, a sync and a push all present them to the
remote. A digest is therefore no use here, and the question is how a value
nobody may read out of the store is nonetheless readable by the service.

## Decision

Every secret value is sealed with AES-GCM through `crypto.subtle` before it
reaches any store, under one key the deployment supplies as 32 base64 bytes
in `PARLEY_SECRET_KEY`. Each write draws a fresh 12-byte nonce, and the
stored form is `base64(nonce).base64(ciphertext+tag)`.

A deployment that configures no key has no secret store. The routes that
would need one say so — a workspace create carrying credentials answers
`500` — rather than storing a credential in clear.

## Consequences

- A copy of the database yields no credential. That is the property the
  reference's "no response carries a secret" rule is worth having at rest.
- The fresh nonce means two equal secrets do not look equal in the store, so
  nobody learns that two workspaces share a token by reading rows.
- The authentication tag means a value edited in the store is refused rather
  than returned, and a value sealed under another key cannot be opened. Both
  are tested.
- The key is the deployment's to keep and to rotate. Rotating it makes every
  stored secret unreadable; a rotation procedure that re-seals them is work
  nobody has needed yet, and it needs a decision of its own.
- Sealing and opening cost one AES-GCM operation each, which is negligible
  against a request that is about to talk to a git remote.
