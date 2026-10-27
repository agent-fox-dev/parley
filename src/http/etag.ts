import type { Context } from 'hono';
import { jsonResponse } from './json.js';

/**
 * An entity tag over a representation. It is a digest of the body the
 * route would send, so a client's tag matches exactly while nothing it
 * would see has changed.
 */
export async function etagOf(body: unknown): Promise<string> {
  const digest = await crypto.subtle.digest(
    'SHA-256',
    new TextEncoder().encode(JSON.stringify(body)),
  );
  const hex = [...new Uint8Array(digest)]
    .slice(0, 16)
    .map((byte) => byte.toString(16).padStart(2, '0'))
    .join('');
  return `"${hex}"`;
}

/**
 * The representation, or 304 when the client already holds it. Every route
 * that supports a conditional GET answers through here.
 */
export async function conditionalJson(c: Context, body: unknown): Promise<Response> {
  const etag = await etagOf(body);
  if (c.req.header('if-none-match') === etag) {
    return new Response(null, { status: 304, headers: { etag } });
  }
  return jsonResponse(body, 200, { etag });
}
