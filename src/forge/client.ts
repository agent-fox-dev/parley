import { HttpError } from '../http/errors.js';
import type { LogSink } from '../http/router.js';
import type { RelationalStore } from '../platform/index.js';
import type { SecretStore } from '../platform/secrets.js';
import { workspaceScope } from '../platform/secrets.js';
import { resolveGitCredential } from '../repo/credentials.js';
import { effectiveSettings } from '../workspace/settings.js';
import type { WorkspaceRecord } from '../store/workspaces.js';
import { type ForgeTarget, forgeFor } from './target.js';

/**
 * Talking to a forge.
 *
 * Everything here is decided per call: which repository, which API
 * URL, which credential. Nothing is cached, because the reference
 * requires a rotated secret and a changed setting to apply to the
 * next call, and a cached client would hide both.
 */
export type TargetName = 'origin' | 'upstream';

/** What the hub holds for a forge, as the description route reports it. */
export type CredentialKind = 'pat' | 'basic' | 'none';

export interface ForgeRequest {
  method: string;
  /** The path under the API URL, beginning with a slash. */
  path: string;
  query?: Record<string, string>;
  body?: unknown;
}

export interface ForgeAnswer {
  status: number;
  /** The parsed body, or undefined where there was none to parse. */
  body: unknown;
  headers: Headers;
}

export interface ForgeClient {
  send(request: ForgeRequest): Promise<ForgeAnswer>;
}

export interface ReachDeps {
  store: RelationalStore;
  secrets?: SecretStore;
  fetch?: typeof fetch;
  log?: LogSink;
  /** How long one call has, retries included. */
  deadlineMs?: number;
  /** The deployment's own API URLs, which reads may use and writes may not. */
  environment?: { github?: string; gitlab?: string };
}

export interface Reached {
  target: ForgeTarget;
  credential: CredentialKind;
  client: ForgeClient;
}

/** The version the hub introduces itself as. */
export const USER_AGENT = 'af-hub/parley';

/** How long one call has, its retries included. */
export const DEADLINE_MS = 30_000;

/** How many same-host redirects a request will follow before giving up. */
const MOST_REDIRECTS = 3;

/**
 * Which repository a route works on, with the credential and the API
 * URL that go with it. A target the workspace cannot offer is refused
 * here, before anything is sent.
 */
export async function reachForge(
  deps: ReachDeps,
  workspace: WorkspaceRecord,
  request: { target?: string; writing?: boolean },
): Promise<Reached> {
  const asked = request.target ?? 'origin';
  if (asked !== 'origin' && asked !== 'upstream') {
    throw new HttpError(400, 'target must be "origin" or "upstream"');
  }

  const settings = effectiveSettings(workspace.settings);
  const upstreamUrl = workspace.upstream_url ?? '';
  if (
    asked === 'upstream' &&
    (workspace.workspace_mode !== 'carry_patch' || upstreamUrl.length === 0)
  ) {
    throw new HttpError(
      400,
      'the upstream target needs a carry_patch workspace with an upstream URL',
      'workspace_mode_mismatch',
    );
  }

  const remoteUrl = asked === 'upstream' ? upstreamUrl : workspace.git_url;
  const settingName = asked === 'upstream' ? 'upstream_forge_api_url' : 'forge_api_url';
  const target = forgeFor({
    remoteUrl,
    // The upstream setting does not fall back to the origin's: they
    // are two repositories and may be on two forges.
    setting: String(settings[settingName] ?? ''),
    settingName,
    // A write never takes the deployment's own API URL.
    ...(request.writing === true || deps.environment === undefined
      ? {}
      : { environment: deps.environment }),
  });

  const stored =
    deps.secrets === undefined
      ? undefined
      : await resolveGitCredential(deps.secrets, workspaceScope(workspace.slug), asked, deps.log);

  return {
    target,
    credential: stored === undefined ? 'none' : stored.kind === 'token' ? 'pat' : 'basic',
    client: forgeClient({
      target,
      // A username and password authenticate with the password: the
      // forge APIs take a token, and that is where the token is.
      ...(stored === undefined
        ? {}
        : { token: stored.kind === 'token' ? stored.token : stored.password }),
      ...(deps.fetch === undefined ? {} : { fetch: deps.fetch }),
      ...(deps.deadlineMs === undefined ? {} : { deadlineMs: deps.deadlineMs }),
    }),
  };
}

export interface ClientDeps {
  target: ForgeTarget;
  token?: string;
  fetch?: typeof fetch;
  deadlineMs?: number;
}

/** The address a request goes to, with its query. */
function addressOf(apiUrl: string, request: ForgeRequest): string {
  const url = new URL(`${apiUrl.replace(/\/$/, '')}${request.path}`);
  for (const [name, value] of Object.entries(request.query ?? {})) {
    url.searchParams.set(name, value);
  }
  return url.href;
}

/** Whether a redirect stays on the host, port and scheme it started on. */
function sameOrigin(from: string, to: string): boolean {
  try {
    return new URL(to, from).origin === new URL(from).origin;
  } catch {
    return false;
  }
}

export function forgeClient(deps: ClientDeps): ForgeClient {
  const call = deps.fetch ?? fetch;
  const deadlineMs = deps.deadlineMs ?? DEADLINE_MS;

  return {
    async send(request: ForgeRequest): Promise<ForgeAnswer> {
      const headers: Record<string, string> = {
        accept: 'application/json',
        'user-agent': USER_AGENT,
      };
      if (deps.token !== undefined && deps.token.length > 0) {
        headers['authorization'] = `Bearer ${deps.token}`;
      }
      if (request.body !== undefined) {
        headers['content-type'] = 'application/json';
      }

      let address = addressOf(deps.target.apiUrl, request);
      const giveUp = AbortSignal.timeout(deadlineMs);

      for (let redirects = 0; redirects <= MOST_REDIRECTS; redirects += 1) {
        let answer: Response;
        try {
          answer = await call(address, {
            method: request.method,
            headers,
            // Redirects are the hub's to decide: one to another host
            // would send the workspace's credential somewhere else.
            redirect: 'manual',
            signal: giveUp,
            ...(request.body === undefined ? {} : { body: JSON.stringify(request.body) }),
          });
        } catch (err) {
          if (giveUp.aborted) {
            throw new HttpError(504, 'the forge did not answer in time', 'forge_timeout');
          }
          throw new HttpError(
            502,
            `the forge could not be reached: ${err instanceof Error ? err.message : 'the connection failed'}`,
            'forge_unreachable',
          );
        }

        const moved = answer.status >= 300 && answer.status < 400;
        const location = answer.headers.get('location');
        if (!moved || location === null) {
          return { status: answer.status, body: await parsed(answer), headers: answer.headers };
        }
        if (!sameOrigin(address, location)) {
          throw new HttpError(
            502,
            'the forge redirected the request to another host',
            'forge_error',
          );
        }
        address = new URL(location, address).href;
      }

      throw new HttpError(502, 'the forge redirected the request too many times', 'forge_error');
    },
  };
}

/** The answer's body, or undefined where there was nothing to read. */
async function parsed(answer: Response): Promise<unknown> {
  const text = await answer.text().catch(() => '');
  if (text.length === 0) {
    return undefined;
  }
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}
