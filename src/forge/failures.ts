import { HttpError } from '../http/errors.js';

/**
 * What a forge's failure becomes.
 *
 * The statuses here are deliberately not the forge's own: its 401
 * means the workspace's credential was refused, not the caller's, so
 * the caller is told 502 and not 401. Its message is carried through
 * because it is usually the only thing that says what was wrong, and
 * scrubbed because it may hold the credential it refused.
 */

/** How much of a forge's message is carried through. */
const MOST_BYTES = 1024;

/**
 * A message with nothing of a credential left in it: a URL's
 * userinfo removed, and the text cut to what the reference allows.
 */
export function scrubbed(message: string, token?: string): string {
  let said = message;
  if (token !== undefined && token.length > 0) {
    said = said.replaceAll(token, '[redacted]');
  }
  // Any userinfo, whether or not it is the credential this call used.
  said = said.replace(/([a-z][a-z0-9+.-]*:\/\/)[^/\s@]+@/gi, '$1');

  const bytes = new TextEncoder().encode(said);
  if (bytes.length <= MOST_BYTES) {
    return said;
  }
  return new TextDecoder().decode(bytes.slice(0, MOST_BYTES)).replace(/�$/, '');
}

/** What the forge said, in the words the reference gives it. */
export function forgeFailure(
  status: number,
  body: unknown,
  options: { token?: string } = {},
): HttpError {
  const said = messageOf(body);
  const message = `the forge answered ${String(status)}${said.length === 0 ? '' : `: ${said}`}`;

  if (status === 404) {
    return new HttpError(404, scrubbed(message, options.token), 'forge_not_found');
  }
  if (status === 401) {
    return new HttpError(502, scrubbed(message, options.token), 'forge_unauthenticated');
  }
  if (status === 403) {
    return new HttpError(502, scrubbed(message, options.token), 'forge_forbidden');
  }
  if (status >= 400 && status < 500) {
    return new HttpError(422, scrubbed(message, options.token), 'forge_rejected');
  }
  return new HttpError(502, scrubbed(message, options.token), 'forge_error');
}

/** The forge's own words, wherever this forge puts them. */
function messageOf(body: unknown): string {
  if (typeof body === 'string') {
    return body;
  }
  if (typeof body !== 'object' || body === null) {
    return '';
  }
  const held = body as Record<string, unknown>;
  for (const name of ['message', 'error', 'error_description']) {
    if (typeof held[name] === 'string') {
      return held[name];
    }
  }
  return '';
}
