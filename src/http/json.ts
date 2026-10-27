import type { Context } from 'hono';
import { HttpError } from './errors.js';

/** A JSON answer, with whatever headers the route adds to it. */
export function jsonResponse(
  body: unknown,
  status = 200,
  headers: Record<string, string> = {},
): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json; charset=UTF-8', ...headers },
  });
}

/** No body at all, for the routes whose success says nothing. */
export function emptyResponse(status: number): Response {
  return new Response(null, { status });
}

/**
 * The request's body as a JSON object. Anything that is not one — absent,
 * unparseable, an array, a string, null — is refused the same way, because
 * a route can do nothing useful with any of them.
 */
export async function readJsonObject(c: Context): Promise<Record<string, unknown>> {
  let parsed: unknown;
  try {
    parsed = await c.req.json();
  } catch {
    throw new HttpError(400, 'the request body must be a JSON object');
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    throw new HttpError(400, 'the request body must be a JSON object');
  }
  return parsed as Record<string, unknown>;
}

/** A required string field, or a 400 naming it. */
export function requiredString(body: Record<string, unknown>, field: string): string {
  const value = body[field];
  if (typeof value !== 'string' || value.length === 0) {
    throw new HttpError(400, `${field} is required`);
  }
  return value;
}
