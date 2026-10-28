import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import type { LogSink } from '../http/router.js';
import type { RelationalStore } from '../platform/index.js';
import type { SecretStore } from '../platform/secrets.js';
import type { EventRecorder } from '../audit/recorder.js';
import { identityOf } from '../auth/middleware.js';
import { accessLevel } from '../workspace/access.js';
import { type WorkspaceRecord, findWorkspace } from '../store/workspaces.js';
import { type Reached, type TargetName, reachForge } from './client.js';

/**
 * The order a forge route checks in.
 *
 * The reads and the writes check different things in different
 * orders, and the order is the contract: it decides whether a caller
 * without the scope learns that a workspace exists, and whether an
 * oversized body on a workspace whose forge nobody can identify is
 * told about the body or about the forge. Both routes run this, so
 * neither can drift.
 *
 * The first two checks are not here: a missing credential is the
 * authentication middleware's answer, and a personal access token
 * without the scope is the router's, which refuses before the
 * handler runs and therefore before the workspace is looked up.
 */
export interface ForgeDeps {
  store: RelationalStore;
  secrets?: SecretStore;
  fetch?: typeof fetch;
  log?: LogSink;
  events?: EventRecorder;
  /** The deployment's own API URLs, which reads may use and writes may not. */
  environment?: { github?: string; gitlab?: string };
  /** How long a call to the forge has; the default is the reference's. */
  deadlineMs?: number;
}

export interface Guarded {
  workspace: WorkspaceRecord;
  target: TargetName;
  writing: boolean;
}

/** A path number, which every route spells the same way. */
export function checkedNumber(given: string): number {
  if (!/^[1-9]\d*$/.test(given)) {
    throw new HttpError(400, 'n must be a positive decimal integer');
  }
  return Number(given);
}

/**
 * The target a request asks for, checked for its spelling alone. What
 * the workspace can offer is settled later, when the forge is
 * reached, because that is where the upstream URL is read.
 */
function askedTarget(c: Context): TargetName {
  const url = new URL(c.req.url);
  const given = url.searchParams.getAll('target');
  if (given.length > 1) {
    throw new HttpError(400, 'target must be "origin" or "upstream"');
  }
  const asked = given[0] ?? 'origin';
  if (asked !== 'origin' && asked !== 'upstream') {
    throw new HttpError(400, 'target must be "origin" or "upstream"');
  }
  return asked;
}

/**
 * The workspace and the target of a forge route, with everything the
 * reference checks before the route's own parameters.
 */
export async function forgeGuard(
  c: Context,
  deps: ForgeDeps,
  need: 'read' | 'write',
): Promise<Guarded> {
  const slug = c.req.param('slug') ?? '';
  const workspace = await findWorkspace(deps.store, slug);
  if (workspace === undefined) {
    throw new HttpError(404, 'workspace not found');
  }

  const level = await accessLevel(deps.store, identityOf(c), workspace);
  if (level === undefined) {
    throw new HttpError(404, 'workspace not found');
  }
  // A member reads the forge with the workspace's credential, as the
  // owner does, and writes nothing.
  if (need === 'write' && level === 'member') {
    throw new HttpError(403, 'forbidden');
  }
  // Only a write needs a workspace that is still live; every read
  // answers for an archived one.
  if (need === 'write' && workspace.status === 'archived') {
    throw new HttpError(400, 'workspace is archived');
  }

  return { workspace, target: askedTarget(c), writing: need === 'write' };
}

/**
 * The forge this request works on, reached last of all: after the
 * workspace, the target and the route's own parameters, so a request
 * refused for any of those never learns whether the forge could be
 * identified.
 */
export async function reachedForge(deps: ForgeDeps, guarded: Guarded): Promise<Reached> {
  return reachForge(
    {
      store: deps.store,
      ...(deps.secrets === undefined ? {} : { secrets: deps.secrets }),
      ...(deps.fetch === undefined ? {} : { fetch: deps.fetch }),
      ...(deps.log === undefined ? {} : { log: deps.log }),
      ...(deps.deadlineMs === undefined ? {} : { deadlineMs: deps.deadlineMs }),
      ...(deps.environment === undefined ? {} : { environment: deps.environment }),
    },
    guarded.workspace,
    { target: guarded.target, writing: guarded.writing },
  );
}
