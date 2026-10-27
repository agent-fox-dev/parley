import type { Context } from 'hono';
import { HttpError } from '../http/errors.js';
import { emptyResponse, jsonResponse, readJsonObject, requiredString } from '../http/json.js';
import type { RouteDefinition } from '../http/router.js';
import type { Clock, RelationalStore } from '../platform/index.js';
import {
  type OrgRecord,
  addMember,
  deleteOrg,
  findOrg,
  findOrgByNameOrSlug,
  findUser,
  insertOrg,
  listOrgs,
  membersOfOrg,
  removeMember,
  updateOrg,
} from '../store/identity.js';
import { requireAdmin } from './admin_users.js';

export interface AdminOrgRouteConfig {
  mountPoint: string;
  store: RelationalStore;
  clock: Clock;
}

/** Lower case words joined by single hyphens, as the reference requires. */
const SLUG = /^[a-z0-9]+(-[a-z0-9]+)*$/;

async function namedOrg(c: Context, store: RelationalStore): Promise<OrgRecord> {
  const org = await findOrg(store, c.req.param('id') ?? '');
  if (org === undefined) {
    throw new HttpError(404, 'organization not found');
  }
  return org;
}

export function adminOrgRoutes(config: AdminOrgRouteConfig): RouteDefinition[] {
  const { store, clock } = config;
  const base = `${config.mountPoint}/orgs`;

  const statusRoute = (suffix: string, status: 'active' | 'blocked'): RouteDefinition => ({
    path: `${base}/:id/${suffix}`,
    methods: ['POST'],
    handler: async (c) => {
      await requireAdmin(c, store);
      const org = await namedOrg(c, store);
      const at = clock.now().toISOString();
      await updateOrg(store, org.id, { status }, at);
      return jsonResponse({ ...org, status, updated_at: at });
    },
  });

  return [
    {
      path: base,
      methods: ['GET', 'POST'],
      handler: async (c) => {
        await requireAdmin(c, store);
        if (c.req.method === 'GET') {
          return jsonResponse(await listOrgs(store));
        }

        const body = await readJsonObject(c);
        const name = requiredString(body, 'name');
        const slug = requiredString(body, 'slug');
        if (!SLUG.test(slug)) {
          throw new HttpError(400, 'slug must be lower-case words joined by single hyphens');
        }
        const ownerId = requiredString(body, 'owner_id');
        if ((await findUser(store, ownerId)) === undefined) {
          throw new HttpError(400, 'owner_id names no user');
        }
        if ((await findOrgByNameOrSlug(store, name, slug)) !== undefined) {
          throw new HttpError(409, 'that name or slug is taken');
        }

        const at = clock.now().toISOString();
        const org: OrgRecord = {
          id: crypto.randomUUID(),
          name,
          slug,
          url: typeof body['url'] === 'string' ? body['url'] : '',
          owner_id: ownerId,
          status: 'active',
          created_at: at,
          updated_at: at,
        };
        await insertOrg(store, org);
        // The owner is a member from the start, so the organization is
        // never one nobody can read.
        await addMember(store, {
          org_id: org.id,
          user_id: ownerId,
          role: 'owner',
          created_at: at,
        });
        return jsonResponse(org, 201);
      },
    },
    {
      path: `${base}/:id`,
      methods: ['PATCH', 'DELETE'],
      handler: async (c) => {
        await requireAdmin(c, store);
        const org = await namedOrg(c, store);

        if (c.req.method === 'DELETE') {
          await deleteOrg(store, org.id);
          return emptyResponse(204);
        }

        const body = await readJsonObject(c);
        const name = body['name'];
        const url = body['url'];
        if (typeof name !== 'string' && typeof url !== 'string') {
          throw new HttpError(400, 'name or url is required');
        }
        if (typeof name === 'string' && name !== org.name) {
          const taken = await findOrgByNameOrSlug(store, name, name);
          if (taken !== undefined && taken.id !== org.id) {
            throw new HttpError(409, 'that name is taken');
          }
        }

        const at = clock.now().toISOString();
        const fields = {
          ...(typeof name === 'string' ? { name } : {}),
          ...(typeof url === 'string' ? { url } : {}),
        };
        await updateOrg(store, org.id, fields, at);
        return jsonResponse({ ...org, ...fields, updated_at: at });
      },
    },
    statusRoute('block', 'blocked'),
    statusRoute('unblock', 'active'),
    {
      path: `${base}/:id/members`,
      handler: async (c) => {
        await requireAdmin(c, store);
        const org = await namedOrg(c, store);
        return jsonResponse(await membersOfOrg(store, org.id));
      },
    },
    {
      path: `${base}/:id/members/:user_id`,
      methods: ['PUT', 'DELETE'],
      handler: async (c) => {
        await requireAdmin(c, store);
        const org = await namedOrg(c, store);
        const userId = c.req.param('user_id') ?? '';

        if (c.req.method === 'DELETE') {
          await removeMember(store, org.id, userId);
          return emptyResponse(204);
        }

        const user = await findUser(store, userId);
        if (user === undefined) {
          throw new HttpError(404, 'user not found');
        }
        const at = clock.now().toISOString();
        await addMember(store, { org_id: org.id, user_id: userId, role: 'member', created_at: at });
        return jsonResponse({
          org_id: org.id,
          user_id: userId,
          username: user.username,
          email: user.email,
          role: 'member',
          created_at: at,
        });
      },
    },
  ];
}
