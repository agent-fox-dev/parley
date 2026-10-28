import type { Context } from 'hono';
import type { RouteDefinition } from '../http/router.js';
import type { StoredRef } from '../platform/repostore.js';
import type { RefChange } from '../platform/repostore.js';
import { type GitAccessDeps, GitRefusal, NOT_AVAILABLE, gitAccess } from './access.js';
import { ZERO_OID } from './transport.js';
import {
  FLUSH,
  concatBytes,
  decodePktLines,
  encodePkt,
  encodeSpecial,
  pktError,
  textOf,
} from './pktline.js';

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
export type GitRouteConfig = GitAccessDeps;

export const UPLOAD_PACK = 'git-upload-pack';
export const RECEIVE_PACK = 'git-receive-pack';

/** What the hub tells a client it can do, kept to what it really does. */
const FETCH_CAPABILITIES = 'object-format=sha1 agent=parley';
const PUSH_CAPABILITIES = 'report-status delete-refs object-format=sha1 agent=parley';

/** The ref advertisement, as git's own server writes it. */
export function advertisement(service: string, refs: readonly StoredRef[]): Uint8Array {
  const capabilities = service === RECEIVE_PACK ? PUSH_CAPABILITIES : FETCH_CAPABILITIES;
  const branch = defaultBranch(refs);
  const head = refs.find((ref) => ref.name === `refs/heads/${branch}`);
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
    const [verb, oid] = textOf(line).trim().split(' ');
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
        const { wants, haves } = negotiation(new Uint8Array(await c.req.arrayBuffer()));
        const pack = await subject.repos.servePack(subject.repository, wants, haves);
        return new Response(concatBytes([encodePkt('NAK\n'), pack]) as BodyInit, {
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
        const { commands, pack } = receiveRequest(new Uint8Array(await c.req.arrayBuffer()));
        if (commands.length === 0) {
          return new Response(new Uint8Array() as BodyInit, {
            headers: { 'content-type': `application/x-${RECEIVE_PACK}-result` },
          });
        }

        await subject.repos.receivePack(subject.repository, pack, commands);
        return new Response(
          concatBytes([
            encodePkt('unpack ok\n'),
            ...commands.map((command) => encodePkt(`ok ${command.name}\n`)),
            encodeSpecial(FLUSH),
          ]) as BodyInit,
          {
            headers: {
              'content-type': `application/x-${RECEIVE_PACK}-result`,
              'cache-control': 'no-cache',
            },
          },
        );
      }),
    },
  ];
}
