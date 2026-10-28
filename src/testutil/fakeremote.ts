import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  FLUSH,
  concatBytes,
  decodePktLines,
  encodePkt,
  encodeSpecial,
  textOf,
} from '../git/pktline.js';
import { objectId, writePack } from '../platform/local/pack.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { RepositoryStore } from '../platform/repostore.js';

export interface FakeRemoteOptions {
  /** The credential the remote insists on, or none for a public remote. */
  requires?: string;
  /** Answers every request with this status instead of serving. */
  fails?: number;
  /** Rejects the connection outright, as an unreachable host does. */
  unreachable?: boolean;
}

export interface FakeRemote {
  url: string;
  fetch: typeof fetch;
  /** Every Authorization header the remote was sent. */
  seen: () => string[];
  /** Puts a branch or tag on the remote at a fresh commit. */
  put: (ref: string, content: string) => Promise<string>;
  requests: () => number;
  dispose: () => void;
}

const V2 = [
  'version 2\n',
  'agent=parley-fake-remote/1.0\n',
  'ls-refs=unborn\n',
  'fetch=shallow filter\n',
  'object-format=sha1\n',
];

/**
 * A git remote a copy can be run against: it serves protocol v2 from a
 * repository of its own, records what it was asked and what credential it
 * was shown, and can be told to refuse or to be unreachable.
 */
export function fakeRemote(options: FakeRemoteOptions = {}): FakeRemote {
  const root = mkdtempSync(join(tmpdir(), 'parley-fake-remote-'));
  const store: RepositoryStore = localRepositoryStore(root);
  const name = 'origin';
  const seen: string[] = [];
  let requests = 0;
  const ready = store.create(name);

  async function serve(url: string, init?: RequestInit): Promise<Response> {
    await ready;
    requests += 1;
    const headers = (init?.headers ?? {}) as Record<string, string>;
    const authorization = headers['authorization'] ?? headers['Authorization'] ?? '';
    if (authorization.length > 0) {
      seen.push(authorization);
    }

    if (options.unreachable === true) {
      throw new TypeError('fetch failed');
    }
    if (options.fails !== undefined) {
      return new Response('the remote refused', { status: options.fails });
    }
    if (options.requires !== undefined && authorization !== options.requires) {
      return new Response('authentication required', { status: 401 });
    }

    if (url.includes('/info/refs')) {
      return new Response(
        concatBytes([
          encodePkt('# service=git-upload-pack\n'),
          encodeSpecial(FLUSH),
          ...V2.map((line) => encodePkt(line)),
          encodeSpecial(FLUSH),
        ]) as BodyInit,
        { status: 200 },
      );
    }

    const body = new Uint8Array((init?.body ?? new Uint8Array()) as ArrayBuffer);
    const asked = decodePktLines(body)
      .filter((line) => line.kind === 'data')
      .map(textOf);

    if (asked.includes('command=ls-refs\n')) {
      const refs = await store.listRefs(name);
      const head = refs.find((ref) => ref.name === 'refs/heads/main') ?? refs[0];
      return new Response(
        concatBytes([
          ...refs.map((ref) => encodePkt(`${ref.oid} ${ref.name}\n`)),
          ...(head === undefined
            ? []
            : [encodePkt(`${head.oid} HEAD symref-target:${head.name}\n`)]),
          encodeSpecial(FLUSH),
        ]) as BodyInit,
        { status: 200 },
      );
    }

    const wants = asked
      .filter((line) => line.startsWith('want '))
      .map((line) => line.slice('want '.length).trim());
    const pack = await store.servePack(name, wants);
    return new Response(
      concatBytes([
        encodePkt('packfile\n'),
        encodePkt(concatBytes([new Uint8Array([1]), pack])),
        encodeSpecial(FLUSH),
      ]) as BodyInit,
      { status: 200 },
    );
  }

  return {
    url: 'https://forge.test/owner/repo.git',
    fetch: (async (input: string | URL | Request, init?: RequestInit) =>
      serve(String(input), init)) as unknown as typeof fetch,
    seen: () => [...seen],
    requests: () => requests,
    put: async (ref: string, content: string) => {
      await ready;
      const data = new TextEncoder().encode(content);
      const oid = objectId('blob', data);
      await store.receivePack(name, writePack([{ oid, type: 'blob', data }]), [
        { name: ref, oldOid: null, newOid: oid, force: true },
      ]);
      return oid;
    },
    dispose: () => {
      rmSync(root, { recursive: true, force: true });
    },
  };
}
