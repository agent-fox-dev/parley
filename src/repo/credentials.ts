import type { LogSink } from '../http/router.js';
import type { SecretStore } from '../platform/secrets.js';

export type GitCredential =
  { kind: 'token'; token: string } | { kind: 'basic'; username: string; password: string };

/** The reserved names, per remote. Everything else is an ordinary secret. */
const NAMES = {
  origin: { token: 'GIT_PAT', username: 'GIT_USERNAME', password: 'GIT_PASSWORD' },
  upstream: {
    token: 'UPSTREAM_GIT_PAT',
    username: 'UPSTREAM_GIT_USERNAME',
    password: 'UPSTREAM_GIT_PASSWORD',
  },
} as const;

async function credentialNamed(
  secrets: SecretStore,
  scope: string,
  names: { token: string; username: string; password: string },
  log?: LogSink,
): Promise<GitCredential | undefined> {
  const token = await secrets.get(scope, names.token);
  if (token !== undefined && token.length > 0) {
    return { kind: 'token', token };
  }

  const username = await secrets.get(scope, names.username);
  const password = await secrets.get(scope, names.password);
  if (username !== undefined && password !== undefined) {
    return { kind: 'basic', username, password };
  }

  // Half a pair cannot authenticate anything, and guessing the other half
  // is worse than going without. It is said once, by name and never by
  // value, because an operator who stored one half meant to store both.
  const half =
    username === undefined ? (password === undefined ? undefined : names.password) : names.username;
  if (half !== undefined) {
    log?.info?.('a half credential pair was ignored', { scope, name: half });
  }
  return undefined;
}

/**
 * The credential a remote needs, read from the workspace's own reserved
 * secrets and from nowhere else: not the owner's, not the organization's.
 * A workspace's git access is the workspace's to grant.
 *
 * Upstream falls back to origin's credential, because a fork and its
 * upstream are usually on the same forge under the same account.
 */
export async function resolveGitCredential(
  secrets: SecretStore,
  scope: string,
  which: 'origin' | 'upstream',
  log?: LogSink,
): Promise<GitCredential | undefined> {
  const own = await credentialNamed(secrets, scope, NAMES[which], log);
  if (own !== undefined || which === 'origin') {
    return own;
  }
  return credentialNamed(secrets, scope, NAMES.origin, log);
}
