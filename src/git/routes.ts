import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import type { RepositoryStore, StoredRef } from '../platform/repostore.js';
import { type AuthOptions, identifyCredential } from '../auth/middleware.js';
import { accessLevel } from '../workspace/access.js';
import { repositoryOf } from '../repo/repository.js';
import { type WorkspaceRecord, findWorkspace } from '../store/workspaces.js';
import { findOrgByNameOrSlug } from '../store/identity.js';
import { ZERO_OID } from './transport.js';
import { asBearer } from './auth.js';
import {
  FLUSH,
  concatBytes,
  decodePktLines,
  encodePkt,
  encodeSpecial,
  pktError,
  textOf,
} from './pktline.js';

/**
 * The git smart HTTP surface. It lives at the server root rather than
 * under the mount point, because a clone URL is what a person types and
 * `/api/v1` has no business in it.
 *
 * Every refusal here is pkt-line: a git client renders that to the
 * person, where it reports the service's JSON envelope as a failure it
 * could not read.
 */
export interface GitRouteConfig {
  store: RelationalStore;
  clock: Clock;
  /** The deployment's admin token, where one is configured. */
  adminToken?: string;
  /** Absent where the deployment was given no repository store. */
  repos?: RepositoryStore;
}

export const UPLOAD_PACK = 'git-upload-pack';
export const RECEIVE_PACK = 'git-receive-pack';

const NOT_FOUND = 'repository not found';
const UNAUTHENTICATED = 'authentication required';
const MEMBERS_READ_ONLY = 'org members have read-only access to this workspace';
const NOT_AVAILABLE = 'repository not available';

/** What the hub tells a client it can do, kept to what it really does. */
const CAPABILITIES = 'object-format=sha1 agent=parley';

interface Opened {
  workspace: WorkspaceRecord;
  repository: string;
  repos: RepositoryStore;
}

/**
 * The workspace a git path names and the caller's right to it, or the
 * refusal. The checks run in the order the contract fixes: the path,
 * the credential, the workspace and its organization, the repository's
 * readiness, then access.
 */
async function open(
  c: Context,
  config: GitRouteConfig,
  need: 'read' | 'write',
): Promise<Opened | Response> {
  const repo = c.req.param('repo') ?? '';
  if (!repo.endsWith('.git')) {
    return pktError(404, NOT_FOUND);
  }

  const auth: AuthOptions = {
    store: config.store,
    clock: config.clock,
    mountPoint: '',
    ...(config.adminToken === undefined ? {} : { adminToken: config.adminToken }),
  };
  const identity = await identifyCredential(asBearer(c.req.header('authorization')), auth);
  if (identity === undefined) {
    // The challenge is what makes a git client send its credential at
    // all: without it git asks nobody and reports the refusal as final.
    return pktError(401, UNAUTHENTICATED, { 'www-authenticate': 'Basic realm="Parley"' });
  }
  c.set('identity', identity);

  const slug = repo.slice(0, -'.git'.length);
  const workspace = await findWorkspace(config.store, slug);
  const org =
    workspace?.org_id === null || workspace === undefined
      ? undefined
      : await findOrgByNameOrSlug(config.store, '', c.req.param('org') ?? '');

  // A workspace that is not there, one in another organization and a
  // path that is not a repository are one answer, so nothing here says
  // whether a workspace exists.
  if (workspace === undefined || org === undefined || org.id !== workspace.org_id) {
    return pktError(404, NOT_FOUND);
  }
  // A token bound to another workspace is told the same.
  if (identity.boundTo !== undefined && identity.boundTo !== workspace.slug) {
    return pktError(404, NOT_FOUND);
  }
  if (workspace.status !== 'ready') {
    return pktError(409, NOT_AVAILABLE);
  }

  const level = await accessLevel(config.store, identity, workspace);
  if (level === 'member' && need === 'write') {
    return pktError(403, MEMBERS_READ_ONLY);
  }
  if (level === undefined) {
    return pktError(404, NOT_FOUND);
  }
  if (config.repos === undefined) {
    return pktError(503, NOT_AVAILABLE);
  }

  return { workspace, repository: repositoryOf(workspace), repos: config.repos };
}

/** The ref advertisement, as git's own server writes it. */
export function advertisement(service: string, refs: readonly StoredRef[]): Uint8Array {
  const head = refs.find((ref) => ref.name === `refs/heads/${defaultBranch(refs)}`);
  const lines: Uint8Array[] = [encodePkt(`# service=${service}\n`), encodeSpecial(FLUSH)];

  const listed = [...(head === undefined ? [] : [{ name: 'HEAD', oid: head.oid }]), ...refs];
  if (listed.length === 0) {
    // An empty repository still has to say what it can do.
    lines.push(encodePkt(`${ZERO_OID} capabilities^{}\u0000${CAPABILITIES}\n`));
  }
  for (const [index, ref] of listed.entries()) {
    const symref = head === undefined ? '' : ` symref=HEAD:refs/heads/${defaultBranch(refs)}`;
    lines.push(
      encodePkt(
        index === 0
          ? `${ref.oid} ${ref.name}\u0000${CAPABILITIES}${symref}\n`
          : `${ref.oid} ${ref.name}\n`,
      ),
    );
  }
  lines.push(encodeSpecial(FLUSH));
  return concatBytes(lines);
}

/** Which branch HEAD follows: the repository's main, else its first. */
function defaultBranch(refs: readonly StoredRef[]): string {
  const heads = refs
    .filter((ref) => ref.name.startsWith('refs/heads/'))
    .map((ref) => ref.name.slice('refs/heads/'.length));
  return (
    heads.find((name) => name === 'main') ??
    heads.find((name) => name === 'master') ??
    heads[0] ??
    ''
  );
}

/** The wants and haves a client sent, in the order it sent them. */
export function negotiation(body: Uint8Array): { wants: string[]; haves: string[] } {
  const wants: string[] = [];
  const haves: string[] = [];
  for (const line of decodePktLines(body)) {
    const text = textOf(line);
    const [verb, oid] = text.trim().split(' ');
    if (verb === 'want' && oid !== undefined) {
      wants.push(oid);
    }
    if (verb === 'have' && oid !== undefined) {
      haves.push(oid);
    }
  }
  return { wants, haves };
}

/**
 * Everything a git route answers, in git's framing. A failure nobody
 * expected is a pkt-line too: a client that receives the service's JSON
 * envelope reports that it could not understand the server, which tells
 * the person nothing about what went wrong.
 */
function gitHandler(what: (c: Context) => Promise<Response>): (c: Context) => Promise<Response> {
  return async (c) => {
    try {
      return await what(c);
    } catch (err) {
      if (err instanceof HttpError) {
        // A repository the store does not hold is one the workspace
        // record promised; the client is told it cannot be served, not
        // that it does not exist.
        return pktError(
          err.status === 404 ? 503 : err.status,
          err.status === 404 ? NOT_AVAILABLE : err.message,
        );
      }
      return pktError(500, 'internal server error');
    }
  };
}

export function gitRoutes(config: GitRouteConfig): RouteDefinition[] {
  return [
    {
      path: '/git/:org/:repo/info/refs',
      methods: ['GET'],
      handler: gitHandler(async (c) => {
        const service = new URL(c.req.url).searchParams.get('service') ?? '';
        const opened = await open(c, config, service === RECEIVE_PACK ? 'write' : 'read');
        if (opened instanceof Response) {
          return opened;
        }
        const refs = await opened.repos.listRefs(opened.repository);
        return new Response(advertisement(service, refs) as BodyInit, {
          headers: {
            'content-type': `application/x-${service}-advertisement`,
            'cache-control': 'no-cache',
          },
        });
      }),
    },
    {
      path: `/git/:org/:repo/${UPLOAD_PACK}`,
      methods: ['POST'],
      handler: gitHandler(async (c) => {
        const opened = await open(c, config, 'read');
        if (opened instanceof Response) {
          return opened;
        }
        const { wants, haves } = negotiation(new Uint8Array(await c.req.arrayBuffer()));
        const pack = await opened.repos.servePack(opened.repository, wants, haves);
        return new Response(concatBytes([encodePkt('NAK\n'), pack]) as BodyInit, {
          headers: {
            'content-type': `application/x-${UPLOAD_PACK}-result`,
            'cache-control': 'no-cache',
          },
        });
      }),
    },
  ];
}
