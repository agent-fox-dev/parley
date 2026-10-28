import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import { emptyResponse, jsonResponse } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import { workspaceScope } from '../platform/secrets.js';
import type { SecretStore } from '../platform/secrets.js';
import { actorIn, record } from '../audit/recorder.js';
import { MAX_ENTRIES_PER_SCOPE } from '../entries/validate.js';
import { SECRET_NAME, generatedSecret, ingressUrl, suppliedSecret } from '../webhook/secret.js';
import { workspaceAccess } from '../workspace/access.js';
import type { ForgeRouteConfig } from './forge.js';

/**
 * Setting up the forge's webhook.
 *
 * The hub gives a person the URL and the secret to enter in the
 * forge's settings and calls nothing itself. The secret is the
 * ordinary workspace secret, so the secrets routes are the other
 * door to the same value, and a generated one is shown in the answer
 * that generated it and nowhere else ever again.
 */
export type WebhookRouteConfig = ForgeRouteConfig;

function secretsOf(config: WebhookRouteConfig): SecretStore {
  if (config.secrets === undefined) {
    throw new HttpError(500, 'this deployment has no key to seal secrets with');
  }
  return config.secrets;
}

async function set(c: Context, config: WebhookRouteConfig): Promise<Response> {
  const { workspace } = await workspaceAccess(c, config.store, 'write');
  if (workspace.status === 'archived') {
    throw new HttpError(400, 'workspace is archived');
  }

  const raw: unknown = await c.req.json().catch(() => undefined);
  const supplied = suppliedSecret(raw);
  const secrets = secretsOf(config);
  const scope = workspaceScope(workspace.slug);

  // The scope's limit is the entry store's, and this route writes
  // through the secret store, which does not count for itself.
  const held = await secrets.names(scope);
  if (!held.includes(SECRET_NAME) && held.length >= MAX_ENTRIES_PER_SCOPE) {
    throw new HttpError(
      400,
      `maximum of ${String(MAX_ENTRIES_PER_SCOPE)} entries per scope exceeded`,
    );
  }

  const secret = supplied ?? generatedSecret();
  await secrets.put(scope, SECRET_NAME, secret);

  await record(config.events, {
    type: 'hub.forge.webhook.set',
    workspace: workspace.slug,
    resourceId: workspace.slug,
    actor: actorIn(c),
    metadata: { generated: supplied === undefined },
  });

  return jsonResponse({
    configured: true,
    url: ingressUrl(config.externalUrl, workspace.slug),
    // Only ever here: a secret the hub made is shown in the answer
    // that made it and in nothing afterwards.
    ...(supplied === undefined ? { secret } : {}),
  });
}

async function unset(c: Context, config: WebhookRouteConfig): Promise<Response> {
  // Any status: a workspace being archived is no reason to leave its
  // ingress open.
  const { workspace } = await workspaceAccess(c, config.store, 'write');
  const secrets = secretsOf(config);
  const scope = workspaceScope(workspace.slug);

  const existed = (await secrets.get(scope, SECRET_NAME)) !== undefined;
  await secrets.delete(scope, SECRET_NAME);

  await record(config.events, {
    type: 'hub.forge.webhook.unset',
    workspace: workspace.slug,
    resourceId: workspace.slug,
    actor: actorIn(c),
    metadata: { existed },
  });

  return emptyResponse(204);
}

export function webhookRoutes(config: WebhookRouteConfig): RouteDefinition[] {
  const path = `${config.mountPoint}/workspaces/:slug/forge/webhook`;
  return [
    { path, methods: ['PUT'], scope: 'forge:write', handler: async (c) => set(c, config) },
    { path, methods: ['DELETE'], scope: 'forge:write', handler: async (c) => unset(c, config) },
  ];
}
