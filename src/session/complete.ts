import { HttpError } from '../http/errors.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { Identity } from '../auth/middleware.js';
import { type SessionRecord, type SessionStatus, sessionById } from './store.js';

/**
 * Closing a session, and reporting what it spent.
 *
 * Both are for the session's creator or an admin token, and for
 * nobody else — not even the workspace's owner. Several agents share
 * one workspace, so an owner check would let any of them close
 * another's session or write into its totals.
 */
const TERMINAL: readonly SessionStatus[] = ['completed', 'failed', 'timeout'];

/** Whether this caller is the one who opened the session. */
export function isCreator(session: SessionRecord, identity: Identity | undefined): boolean {
  if (identity === undefined) {
    return false;
  }
  if (identity.kind === 'admin_token') {
    return true;
  }
  // The credential is what tells two agents apart, so where the
  // session recorded one it is the whole test: two keys of one user
  // are two agents, and one must not close the other's session.
  if (session.credential_id !== null) {
    return session.credential_id === identity.credentialId;
  }
  return session.created_by !== null && session.created_by === identity.userId;
}

/** The session this route names, with the right to act on it. */
export async function ownedSession(
  store: RelationalStore,
  id: string,
  identity: Identity | undefined,
): Promise<SessionRecord> {
  const session = await sessionById(store, id);
  if (session === undefined) {
    throw new HttpError(404, 'session not found');
  }
  if (!isCreator(session, identity)) {
    throw new HttpError(403, 'not session owner');
  }
  return session;
}

export interface CompleteRequest {
  status: SessionStatus;
  error_message?: string;
  duration_ms?: number;
  cache_creation_input_tokens?: number;
}

/** What a close asks for. `terminated` is the hub's to set, not a client's. */
export function readCompleteBody(raw: unknown): CompleteRequest {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new HttpError(400, 'the request body must be a JSON object');
  }
  const body = raw as Record<string, unknown>;

  const status = body['status'];
  if (status === 'terminated') {
    throw new HttpError(400, 'terminated is set by the hub, not by a client');
  }
  if (typeof status !== 'string' || !TERMINAL.includes(status as SessionStatus)) {
    throw new HttpError(400, `status must be one of ${TERMINAL.join(', ')}`);
  }

  const asked: CompleteRequest = { status: status as SessionStatus };
  if (body['error_message'] !== undefined) {
    if (typeof body['error_message'] !== 'string') {
      throw new HttpError(400, 'error_message must be a string');
    }
    asked.error_message = body['error_message'];
  }
  for (const name of ['duration_ms', 'cache_creation_input_tokens'] as const) {
    const value = body[name];
    if (value === undefined) {
      continue;
    }
    if (typeof value !== 'number' || !Number.isInteger(value) || value < 0) {
      throw new HttpError(400, `${name} must be a whole number of at least 0`);
    }
    asked[name] = value;
  }
  return asked;
}

/**
 * Closes a session. One that is already closed is answered with as it
 * stands: a retry of a close that was made is not a second close.
 */
export async function completeSession(
  store: RelationalStore,
  clock: Clock,
  session: SessionRecord,
  asked: CompleteRequest,
): Promise<SessionRecord> {
  if (session.status !== 'active') {
    return session;
  }

  const at = clock.now().toISOString();
  // Conditional on the session still being active, so two closes in
  // flight cannot both write: the second finds nothing to update.
  await store.run(
    `UPDATE sessions
       SET status = ?, error_message = ?, duration_ms = ?, cache_creation_input_tokens = ?, ended_at = ?
     WHERE id = ? AND status = 'active'`,
    [
      asked.status,
      asked.error_message ?? null,
      asked.duration_ms ?? null,
      asked.cache_creation_input_tokens ?? null,
      at,
      session.id,
    ],
  );

  const closed = await sessionById(store, session.id);
  if (closed === undefined) {
    throw new HttpError(500, 'the session could not be read back after it was closed');
  }
  return closed;
}
