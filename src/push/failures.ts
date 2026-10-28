import { HttpError } from '../http/errors.js';
import { RemoteError } from '../git/transport.js';
import type { GitCredential } from '../repo/credentials.js';
import { refusal } from './checks.js';

/**
 * What a push that did not happen is answered with.
 *
 * Every failure maps to one of the route's documented answers, and the
 * message of each begins with its own error type, because the CLI
 * shows the message and nothing else. None of them carries the
 * credential, the userinfo of the address, or the transport's own
 * words: a client learns what happened and no more.
 */
export interface FailureContext {
  branch: string;
  gitUrl: string;
  /** What the workspace had to authenticate with, if anything. */
  credential?: GitCredential;
}

/** Anything a remote may have said, with nothing secret left in it. */
export function scrub(said: string, gitUrl: string): string {
  const userinfo = (() => {
    try {
      const parsed = new URL(gitUrl);
      return [parsed.username, parsed.password].filter((part) => part.length > 0);
    } catch {
      return [];
    }
  })();

  let left = said
    .replaceAll(/(Bearer|Basic)\s+[A-Za-z0-9+/=._-]+/gi, '$1 <redacted>')
    .replaceAll(/[A-Za-z0-9._-]+:[^\s/@]+@/g, '');
  for (const part of userinfo) {
    left = left.replaceAll(part, '<redacted>');
  }
  return left.trim();
}

/** Which credential the workspace offered, by kind and never by value. */
function offered(credential: GitCredential | undefined): string {
  if (credential === undefined) {
    return 'this workspace has no stored credential for origin';
  }
  return credential.kind === 'token'
    ? 'origin rejected the stored GIT_PAT'
    : 'origin rejected the stored GIT_USERNAME and GIT_PASSWORD';
}

/**
 * The answer for a push origin would not take, from what origin said
 * about the ref. A remote distinguishes "you are behind" from "I will
 * not have it" only in words, so the words are read.
 */
export function refusedRef(
  reason: string,
  context: FailureContext,
  options: { force: boolean; theirs?: string },
): HttpError {
  if (/non-fast-forward|fetch first|not a fast.?forward|behind/i.test(reason)) {
    return nonFastForward(context, { ...options, origin: options.theirs, refusedForce: true });
  }
  return refusal(
    422,
    'forge_rejected',
    `origin refused ${context.branch}: ${scrub(reason, context.gitUrl)}`,
  );
}

/**
 * The answer for a push origin is ahead of. What to do about it
 * differs: an ordinary branch is overwritten with force, but the
 * workspace's own branch is brought up to date by a sync, because
 * forcing it is refused anyway.
 */
export function nonFastForward(
  context: FailureContext,
  options: { force: boolean; origin?: string; own?: boolean; refusedForce?: boolean },
): HttpError {
  const where =
    options.origin === undefined
      ? `origin's ${context.branch} has commits this workspace's does not`
      : `origin's ${context.branch} is at ${options.origin}, which this workspace's branch does not descend from`;

  const what =
    options.refusedForce === true
      ? 'origin refuses non-fast-forward updates, also with force'
      : options.own === true
        ? 'sync the workspace to bring it up to date'
        : 'push with force to overwrite it';

  return refusal(409, 'non_fast_forward', `${where}; ${what}`);
}

/**
 * What may be said about a remote that did not answer well: enough to
 * act on, and nothing of the credential, the address or the
 * transport's own words. Every route that talks to origin says it the
 * same way, so a client reads one vocabulary.
 */
export function remoteReason(err: unknown): string {
  if (err instanceof RemoteError) {
    if (err.status === 401) {
      return 'origin refused the stored credential';
    }
    if (err.status === 403) {
      return 'origin refused this workspace access';
    }
    if (err.status === 404) {
      return 'origin has no such repository, or the stored credential cannot see it';
    }
    return `origin answered ${String(err.status)}`;
  }
  if (err instanceof Error && err.name === 'TimeoutError') {
    return 'origin did not answer in time';
  }
  if (err instanceof TypeError) {
    return 'origin could not be reached';
  }
  return 'origin answered something this service could not read';
}

/**
 * Everything else that can go wrong between here and origin, each with
 * its own answer. A failure nobody anticipated is forge_error rather
 * than a 500: the hub is working, the remote is not.
 */
export function pushFailure(err: unknown, context: FailureContext): HttpError {
  if (err instanceof HttpError) {
    return err;
  }

  if (err instanceof RemoteError) {
    if (err.status === 401) {
      return refusal(502, 'forge_unauthenticated', offered(context.credential));
    }
    if (err.status === 403) {
      return refusal(
        502,
        'forge_forbidden',
        `origin refused this workspace access to ${context.branch}`,
      );
    }
    if (err.status === 404) {
      return refusal(
        404,
        'forge_not_found',
        'origin has no such repository, or the stored credential cannot see it',
      );
    }
    return refusal(502, 'forge_error', `origin answered ${String(err.status)}`);
  }

  if (err instanceof Error && err.name === 'TimeoutError') {
    return refusal(504, 'forge_timeout', 'the push to origin did not finish in time');
  }
  if (err instanceof TypeError) {
    return refusal(502, 'forge_unreachable', 'origin could not be reached');
  }
  return refusal(502, 'forge_error', 'origin answered something this service could not read');
}
