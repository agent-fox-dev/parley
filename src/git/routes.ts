import type { Context } from 'hono';
import type { LogSink, RouteDefinition } from '../http/router.js';
import type { SecretStore } from '../platform/secrets.js';
import type { StoredRef } from '../platform/repostore.js';
import type { RefChange } from '../platform/repostore.js';
import { type GitAccessDeps, GitRefusal, NOT_AVAILABLE, gitAccess } from './access.js';
import { negotiation, refusedService, uploadPackResponse } from './fetch.js';
import { type Decider, type RefDecision, servePush } from './push.js';
import { mirrorAfterPush, pushControl } from './push_control.js';
import { protectedRefs } from './protected_refs.js';
import { headRef, refreshHead } from './after_push.js';
import { ZERO_OID } from './transport.js';
import { FLUSH, concatBytes, encodePkt, encodeSpecial, pktError } from './pktline.js';

const decoder = new TextDecoder();

/**
 * The git smart HTTP surface. It lives at the server root rather than
 * under the mount point, because a clone URL is what a person types and
 * `/api/v1` has no business in it.
 *
 * Every answer here is pkt-line, refusals included: a git client shows
 * the person a pkt-line message and reports the service's JSON envelope
 * as a server it could not understand (ADR 10).
 */
export interface GitRouteConfig extends GitAccessDeps {
  /**
   * What the service makes of each ref of a push, beyond the control
   * the workspace's own settings call for. The protected namespace is
   * decided here; without one, only push control applies.
   */
  decide?: Decider;
  /** Where the fork's credentials are read from, for a forward. */
  secrets?: SecretStore;
  /** Injected so a suite reaches a fork of its own. */
  fetch?: typeof fetch;
  log?: LogSink;
}

export const UPLOAD_PACK = 'git-upload-pack';
export const RECEIVE_PACK = 'git-receive-pack';

/** What the hub tells a client it can do, kept to what it really does. */
const FETCH_CAPABILITIES = 'object-format=sha1 agent=parley';
const PUSH_CAPABILITIES = 'report-status delete-refs object-format=sha1 agent=parley';

/** The ref advertisement, as git's own server writes it. */
export function advertisement(service: string, refs: readonly StoredRef[]): Uint8Array {
  const capabilities = service === RECEIVE_PACK ? PUSH_CAPABILITIES : FETCH_CAPABILITIES;
  const head = headRef(refs);
  const branch = head === undefined ? '' : head.name.slice('refs/heads/'.length);
  const lines: Uint8Array[] = [encodePkt(`# service=${service}\n`), encodeSpecial(FLUSH)];

  // HEAD is advertised for a fetch alone: a push needs no default
  // branch, and git's own receive-pack does not offer one.
  const listed = [
    ...(head === undefined || service === RECEIVE_PACK ? [] : [{ name: 'HEAD', oid: head.oid }]),
    ...refs,
  ];
  if (listed.length === 0) {
    // An empty repository still has to say what it can do.
    lines.push(encodePkt(`${ZERO_OID} capabilities^{}\u0000${capabilities}\n`));
  }
  for (const [index, ref] of listed.entries()) {
    const symref =
      head === undefined || service === RECEIVE_PACK ? '' : ` symref=HEAD:refs/heads/${branch}`;
    lines.push(
      encodePkt(
        index === 0
          ? `${ref.oid} ${ref.name}\u0000${capabilities}${symref}\n`
          : `${ref.oid} ${ref.name}\n`,
      ),
    );
  }
  lines.push(encodeSpecial(FLUSH));
  return concatBytes(lines);
}

/**
 * The ref commands of a push and the pack that follows them. The
 * commands are read before any of it reaches the store, which is what
 * lets the service decide each ref on its own.
 */
export function receiveRequest(body: Uint8Array): {
  commands: RefChange[];
  capabilities: string[];
  pack: Uint8Array;
} {
  const commands: RefChange[] = [];
  let capabilities: string[] = [];
  let at = 0;

  // Walked by hand rather than decoded whole: what follows the flush is
  // a packfile, and a pack read as pkt-lines is nonsense.
  while (at + 4 <= body.byteLength) {
    const length = Number.parseInt(decoder.decode(body.subarray(at, at + 4)), 16);
    if (Number.isNaN(length)) {
      break;
    }
    if (length === 0) {
      at += 4;
      break;
    }
    const line = decoder.decode(body.subarray(at + 4, at + length)).replace(/\n$/, '');
    at += length;

    const [stated, offered] = line.split('\u0000');
    const [oldOid, newOid, name] = (stated ?? '').split(' ');
    if (oldOid === undefined || newOid === undefined || name === undefined) {
      continue;
    }
    if (offered !== undefined) {
      capabilities = offered.split(' ').filter(Boolean);
    }
    commands.push({
      name,
      oldOid: oldOid === ZERO_OID ? null : oldOid,
      newOid: newOid === ZERO_OID ? null : newOid,
      force: true,
    });
  }

  return { commands, capabilities, pack: body.subarray(at) };
}

/**
 * The request's bytes, decompressed where the client compressed them.
 * git gzips a request body when it is worth it and says so, and a body
 * read without undoing that is not pkt-line at all.
 */
async function requestBody(c: Context): Promise<Uint8Array> {
  const raw = new Uint8Array(await c.req.arrayBuffer());
  if ((c.req.header('content-encoding') ?? '').toLowerCase() !== 'gzip') {
    return raw;
  }
  const compressed = new Response(raw as BodyInit).body;
  if (compressed === null) {
    return raw;
  }
  const stream = compressed.pipeThrough(new DecompressionStream('gzip'));
  return new Uint8Array(await new Response(stream).arrayBuffer());
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
      if (err instanceof GitRefusal) {
        return pktError(err.status, err.message, err.headers);
      }
      // A repository the workspace record promised but the store does
      // not hold, and anything else nobody expected.
      const status = (err as { status?: number }).status;
      return status === 404
        ? pktError(503, NOT_AVAILABLE)
        : pktError(500, 'the service could not serve this request');
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
        const refused = refusedService(service);
        if (refused !== undefined) {
          throw refused;
        }
        const subject = await gitAccess(c, config, service === RECEIVE_PACK ? 'write' : 'read');
        const refs = await subject.repos.listRefs(subject.repository);

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
        const subject = await gitAccess(c, config, 'read');
        const { wants, haves } = negotiation(await requestBody(c));
        const pack = await subject.repos.servePack(subject.repository, wants, haves);

        return new Response(uploadPackResponse(pack) as BodyInit, {
          headers: {
            'content-type': `application/x-${UPLOAD_PACK}-result`,
            'cache-control': 'no-cache',
          },
        });
      }),
    },
    {
      path: `/git/:org/:repo/${RECEIVE_PACK}`,
      methods: ['POST'],
      handler: gitHandler(async (c) => {
        const subject = await gitAccess(c, config, 'write');
        const { commands, pack } = receiveRequest(await requestBody(c));
        const served = `application/x-${RECEIVE_PACK}-result`;

        // A push with no commands is a client that changed its mind.
        if (commands.length === 0) {
          return new Response(new Uint8Array() as BodyInit, {
            headers: { 'content-type': served },
          });
        }

        const control = {
          store: config.store,
          workspace: subject.workspace,
          pack,
          ...(config.secrets === undefined ? {} : { secrets: config.secrets }),
          ...(config.fetch === undefined ? {} : { fetch: config.fetch }),
          ...(config.log === undefined ? {} : { log: config.log }),
        };
        const controlled = pushControl(control);
        const held = protectedRefs();
        const extra = config.decide;

        const report = await servePush({
          repos: subject.repos,
          repository: subject.repository,
          commands,
          pack,
          // Each says its piece and the first refusal wins, so the
          // protected namespace holds whatever the settings say and
          // whoever is asking.
          decide: async (command): Promise<RefDecision> => {
            for (const decider of [held, extra, controlled]) {
              const decision = await decider?.(command);
              if (decision !== undefined && !decision.accept) {
                return decision;
              }
            }
            return { accept: true };
          },
          afterward: async (outcomes) => {
            // The refresh first, because a mirror is best effort and
            // head_sha is what every client reads.
            await refreshHead({
              store: config.store,
              repos: subject.repos,
              repository: subject.repository,
              slug: subject.workspace.slug,
              clock: config.clock,
            })(outcomes);
            await mirrorAfterPush(control, commands)(outcomes);
          },
        });
        return new Response(report as BodyInit, {
          headers: { 'content-type': served, 'cache-control': 'no-cache' },
        });
      }),
    },
  ];
}
