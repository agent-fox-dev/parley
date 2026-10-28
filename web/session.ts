import { hashSecret, newIdentifier, newSecret } from '../src/auth/credentials.js';
import type { Clock, RelationalStore } from '../src/platform/index.js';

/**
 * The browser's session.
 *
 * It is this package's own, and deliberately not an API key. The
 * API's sign-in revokes every key the person holds, so a developer
 * opening the dashboard would silently kill the key their agent was
 * running with. A session here issues nothing and revokes nothing.
 *
 * The value is a secret the browser keeps; the store keeps only its
 * hash, as it does for every other credential, so a leaked row is
 * not a way in.
 */
export const SESSION_COOKIE = 'parley_session';

/** How long a browser stays signed in without signing in again. */
export const SESSION_HOURS = 12;

export interface OpenedSession {
  /** The value the browser carries, which exists only here. */
  value: string;
  expires_at: string;
}

/** Opens a session for a person who has just proved who they are. */
export async function openSession(
  store: RelationalStore,
  clock: Clock,
  userId: string,
): Promise<OpenedSession> {
  const value = `${newIdentifier()}.${newSecret()}`;
  const now = clock.now();
  const expires = new Date(now.getTime() + SESSION_HOURS * 3_600_000).toISOString();

  await store.run(
    'INSERT INTO web_sessions (id, user_id, secret_hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?)',
    [newIdentifier(), userId, await hashSecret(value), now.toISOString(), expires],
  );
  return { value, expires_at: expires };
}

/** Whose session this is, or nobody's. */
export async function signedInUserId(
  store: RelationalStore,
  clock: Clock,
  value: string | undefined,
): Promise<string | undefined> {
  if (value === undefined || value.length === 0) {
    return undefined;
  }
  const rows = await store.all<{ user_id: string }>(
    'SELECT user_id FROM web_sessions WHERE secret_hash = ? AND expires_at > ?',
    [await hashSecret(value), clock.now().toISOString()],
  );
  return rows[0]?.user_id;
}

/** Ends a session, so the value the browser holds is worth nothing. */
export async function endSession(store: RelationalStore, value: string | undefined): Promise<void> {
  if (value === undefined || value.length === 0) {
    return;
  }
  await store.run('DELETE FROM web_sessions WHERE secret_hash = ?', [await hashSecret(value)]);
}

/** Clears the sessions nobody can use any more. */
export async function sweepSessions(store: RelationalStore, clock: Clock): Promise<void> {
  await store.run('DELETE FROM web_sessions WHERE expires_at <= ?', [clock.now().toISOString()]);
}

/**
 * The cookie the session travels in: unreadable by a script, not
 * sent on another site's request, and over TLS wherever the request
 * came in over TLS.
 */
export function sessionCookie(
  value: string,
  options: { secure: boolean; clear?: boolean },
): string {
  const age = options.clear === true ? 0 : SESSION_HOURS * 3600;
  return [
    `${SESSION_COOKIE}=${value}`,
    'Path=/',
    'HttpOnly',
    'SameSite=Lax',
    `Max-Age=${String(age)}`,
    ...(options.secure ? ['Secure'] : []),
  ].join('; ');
}

/** The session a request carries, among whatever else it carries. */
export function sessionFrom(headers: Headers): string | undefined {
  const cookies = headers.get('cookie');
  if (cookies === null) {
    return undefined;
  }
  for (const pair of cookies.split(';')) {
    const [name, ...rest] = pair.trim().split('=');
    if (name === SESSION_COOKIE) {
      const value = rest.join('=');
      return value.length === 0 ? undefined : value;
    }
  }
  return undefined;
}
