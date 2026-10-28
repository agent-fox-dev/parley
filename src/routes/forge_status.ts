import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import { jsonResponse } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import { workspaceScope } from '../platform/secrets.js';
import { identityOf } from '../auth/middleware.js';
import { accessLevel } from '../workspace/access.js';
import { effectiveSettings } from '../workspace/settings.js';
import { type WorkspaceRecord, findWorkspace } from '../store/workspaces.js';
import { resolveGitCredential } from '../repo/credentials.js';
import { checkedSetting, defaultApiUrl, named, repositoryFrom } from '../forge/target.js';
import type { ForgeRouteConfig } from './forge.js';

/**
 * How the hub reaches a workspace's forge, answered without
 * contacting it.
 *
 * It is the route an operator runs when another answers
 * `forge_unknown`, so it reports a host it cannot classify instead of
 * refusing like the others — the whole point is to say what the hub
 * made of the configuration.
 */
export interface ForgeStatusConfig extends ForgeRouteConfig {
  /** When the workspace's latest delivery arrived, where a log holds one. */
  lastDeliveryAt?: (slug: string) => Promise<string | undefined>;
}

interface Described {
  forge: string;
  api_url: string | null;
  repository: { host: string; owner: string; name: string } | null;
  credential: 'pat' | 'basic' | 'none';
}

/** One remote described: what it is, where its API is, and what we hold for it. */
async function describe(
  config: ForgeStatusConfig,
  workspace: WorkspaceRecord,
  which: 'origin' | 'upstream',
): Promise<Described> {
  const settings = effectiveSettings(workspace.settings);
  const settingName = which === 'upstream' ? 'upstream_forge_api_url' : 'forge_api_url';
  const setting = String(settings[settingName] ?? '')
    .trim()
    .replace(/\/+$/, '');
  if (setting.length > 0) {
    checkedSetting(setting, settingName);
  }

  const remoteUrl = which === 'upstream' ? (workspace.upstream_url ?? '') : workspace.git_url;
  const repository = repositoryFrom(remoteUrl);
  const forge = (setting.length > 0 ? named(setting) : undefined) ?? named(repository?.host ?? '');

  const stored =
    config.secrets === undefined
      ? undefined
      : await resolveGitCredential(config.secrets, workspaceScope(workspace.slug), which);

  const chosen =
    forge === undefined
      ? null
      : setting.length > 0
        ? setting
        : defaultApiUrl(forge, repository?.host ?? '');
  return {
    forge: forge ?? 'unknown',
    api_url:
      chosen !== null && forge === 'gitlab' && !/\/api\/v\d+$/.test(chosen)
        ? `${chosen}/api/v4`
        : chosen,
    repository: repository ?? null,
    credential: stored === undefined ? 'none' : stored.kind === 'token' ? 'pat' : 'basic',
  };
}

/** Where the forge is to post, and when it last did. */
async function webhookBlock(
  config: ForgeStatusConfig,
  slug: string,
): Promise<Record<string, unknown>> {
  const secret =
    config.secrets === undefined
      ? undefined
      : await config.secrets.get(workspaceScope(slug), 'FORGE_WEBHOOK_SECRET');

  return {
    configured: secret !== undefined && secret.length > 0,
    url:
      config.externalUrl === undefined || config.externalUrl.length === 0
        ? null
        : `${config.externalUrl.replace(/\/$/, '')}/hooks/forge/${slug}`,
    last_delivery_at: (await config.lastDeliveryAt?.(slug)) ?? null,
  };
}

async function status(c: Context, config: ForgeStatusConfig): Promise<Response> {
  const workspace = await findWorkspace(config.store, c.req.param('slug') ?? '');
  if (workspace === undefined) {
    throw new HttpError(404, 'workspace not found');
  }
  if ((await accessLevel(config.store, identityOf(c), workspace)) === undefined) {
    throw new HttpError(404, 'workspace not found');
  }

  // Everything is gathered before anything is answered: a
  // description missing its webhook block would read as a workspace
  // with no webhook rather than as a store that could not be read.
  const origin = await describe(config, workspace, 'origin');
  const carries =
    workspace.workspace_mode === 'carry_patch' && (workspace.upstream_url ?? '').length > 0;
  const upstream = carries ? await describe(config, workspace, 'upstream') : null;
  const webhook = await webhookBlock(config, workspace.slug);

  return jsonResponse({ ...origin, upstream, webhook });
}

export function forgeStatusRoutes(config: ForgeStatusConfig): RouteDefinition[] {
  return [
    {
      path: `${config.mountPoint}/workspaces/:slug/forge`,
      scope: 'forge:read',
      handler: async (c) => status(c, config),
    },
  ];
}
