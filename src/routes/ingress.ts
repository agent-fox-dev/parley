import type { Context } from 'hono';
import { HttpError, errorResponse } from '../http/errors.js';
import { jsonResponse } from '../http/json.js';
import type { LogSink, RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import { workspaceScope } from '../platform/secrets.js';
import type { SecretStore } from '../platform/secrets.js';
import type { EventRecorder } from '../audit/recorder.js';
import { findWorkspace } from '../store/workspaces.js';
import { claimDelivery } from '../webhook/dedupe.js';
import { mapDelivery } from '../webhook/mapping.js';
import { relayDelivery, targetOf } from '../webhook/relay.js';
import { settleDelivery } from '../webhook/log.js';
import { SECRET_NAME } from '../webhook/secret.js';
import {
  type DeliveryIdentity,
  MOST_BODY_BYTES,
  NOT_FOUND,
  checkGithubSignature,
  checkGitlabToken,
  identityOf,
  schemeOf,
} from '../webhook/verify.js';
import type { WorkspaceRecord } from '../store/workspaces.js';

/**
 * Where a forge posts what happened.
 *
 * It is the one route served without a bearer credential, at the
 * server root and outside the API's body limit, so the order of its
 * checks is the security: everything that can be decided from the
 * slug, the stored secret and the headers is decided before a byte
 * of the body is read. An unsigned request from anyone at all costs
 * one workspace lookup and one secret read.
 */
export interface IngressConfig {
  store: RelationalStore;
  clock: Clock;
  secrets?: SecretStore;
  events?: EventRecorder;
  log?: LogSink;
}

/** What the ingress knows once the sender has been established. */
export interface AcceptedDelivery {
  workspace: WorkspaceRecord;
  identity: DeliveryIdentity;
  /** The body as it arrived, parsed by whoever needs it. */
  body: Uint8Array;
}

/**
 * The workspace and its secret, or the one refusal they share. An
 * unknown slug and a workspace without a secret answer alike: the
 * route would otherwise say which workspaces exist to anyone who
 * asked.
 */
async function senderOf(
  c: Context,
  config: IngressConfig,
): Promise<{ workspace: WorkspaceRecord; secret: string }> {
  const slug = c.req.param('slug') ?? '';
  // The workspace's status does not matter: an archived workspace
  // with a secret still relays what its forge says happened.
  const workspace = await findWorkspace(config.store, slug);
  if (workspace === undefined || config.secrets === undefined) {
    throw new HttpError(404, NOT_FOUND);
  }

  const secret = await config.secrets.get(workspaceScope(workspace.slug), SECRET_NAME);
  if (secret === undefined || secret.length === 0) {
    throw new HttpError(404, NOT_FOUND);
  }
  return { workspace, secret };
}

/** The body, read only once the sender has been established. */
async function bodyOf(c: Context): Promise<Uint8Array> {
  const declared = c.req.header('content-length');
  if (declared !== undefined && Number(declared) > MOST_BODY_BYTES) {
    throw new HttpError(413, 'body_too_large', 'body_too_large');
  }
  const read = new Uint8Array(await c.req.arrayBuffer());
  if (read.length > MOST_BODY_BYTES) {
    throw new HttpError(413, 'body_too_large', 'body_too_large');
  }
  return read;
}

/**
 * Everything up to the point where the hub believes the delivery.
 * The caller then decides what to make of its body.
 */
async function acceptDelivery(c: Context, config: IngressConfig): Promise<AcceptedDelivery> {
  const { workspace, secret } = await senderOf(c, config);
  const headers = c.req.raw.headers;
  const forge = schemeOf(headers);

  // GitLab proves itself with the secret alone, so it is checked
  // before the body is read at all.
  if (forge === 'gitlab') {
    checkGitlabToken(headers, secret);
  }

  const body = await bodyOf(c);

  if (forge === 'github') {
    await checkGithubSignature(headers, body, secret);
  }

  return { workspace, identity: identityOf(headers, forge), body };
}

/**
 * Asks the platform to let the relay finish whatever the forge
 * does. Not every runtime offers it, and the one that does not
 * cancels nothing, so its absence is not a failure.
 */
function keepRunning(c: Context, work: Promise<unknown>): void {
  try {
    c.executionCtx.waitUntil(work.catch(() => undefined));
  } catch {
    // No execution context here: nothing cancels the work either.
  }
}

/**
 * What the delivery meant, answered to the forge, and then relayed.
 *
 * The answer is written before the first event is recorded, so a
 * slow audit store cannot hold the forge's connection open — which
 * is also why the relay must survive the forge hanging up.
 */
async function answer(
  c: Context,
  config: IngressConfig,
  accepted: AcceptedDelivery,
): Promise<Response> {
  const { workspace, identity } = accepted;
  const settled = { workspace_slug: workspace.slug, delivery_id: identity.id };

  let mapped;
  try {
    mapped = mapDelivery(
      identity.forge,
      identity.event,
      JSON.parse(new TextDecoder().decode(accepted.body)) as unknown,
      identity.id,
    );
  } catch (err) {
    // Kept rather than discarded: a webhook that silently drops
    // what it cannot parse is one nobody can debug.
    await settleDelivery(config.store, settled, { outcome: 'malformed' });
    throw err instanceof HttpError
      ? err
      : new HttpError(400, 'the delivery body is not one JSON object');
  }

  const target = targetOf(workspace, mapped.repository);
  const outcome =
    mapped.events.length === 0 ? 'ignored' : target === null ? 'repository_mismatch' : 'relayed';

  const relaying = relayDelivery(
    {
      store: config.store,
      ...(config.events === undefined ? {} : { events: config.events }),
      ...(config.log === undefined ? {} : { log: config.log }),
    },
    workspace,
    { id: identity.id, mapped, target },
  );
  // Handed to the platform and not awaited: the forge is answered
  // now, and the relay finishes whether or not it waits.
  keepRunning(c, relaying);

  return jsonResponse({ outcome }, 202);
}

export function ingressRoutes(config: IngressConfig): RouteDefinition[] {
  return [
    {
      // At the server root: a forge is given this address to post
      // to, and /api/v1 has no business in it.
      path: '/hooks/forge/:slug',
      methods: ['POST'],
      handler: async (c) => {
        try {
          const accepted = await acceptDelivery(c, config);

          // The row before anything else: it is the claim, and a
          // copy that loses the race for it is the duplicate.
          const claim = await claimDelivery(config.store, {
            workspace_slug: accepted.workspace.slug,
            delivery_id: accepted.identity.id,
            forge: accepted.identity.forge,
            event: accepted.identity.event,
            received_at: config.clock.now().toISOString(),
          });
          if (claim === 'duplicate') {
            return jsonResponse({ outcome: 'duplicate' }, 202);
          }

          return await answer(c, config, accepted);
        } catch (err) {
          if (err instanceof HttpError) {
            return errorResponse(err);
          }
          config.log?.error('a delivery could not be read', err);
          return errorResponse(new HttpError(500, 'internal server error'));
        }
      },
    },
  ];
}
