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
  /** Refuses every ref a push offers, with this reason. */
  refusesPush?: string;
}

export interface FakeRemote {
  url: string;
  /** The repository behind it, so a suite can build real history in it. */
  store: RepositoryStore;
  /** What that repository is called in the store. */
  repository: string;
  /** Resolves once the repository exists. */
  ready: Promise<void>;
  fetch: typeof fetch;
  /** Every Authorization header the remote was sent. */
  seen: () => string[];
  /** Puts a branch or tag on the remote at a fresh commit. */
  put: (ref: string, content: string) => Promise<string>;
  /** Every ref the remote now holds. */
  refs: () => Promise<Array<{ name: string; oid: string }>>;
  /** Every ref command the remote was pushed, in order. */
  pushed: () => Array<{ oldOid: string; newOid: string; name: string }>;
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
/** The push half: applies what it is sent, or refuses as it was told to. */
async function receive(
  store: RepositoryStore,
  name: string,
  options: FakeRemoteOptions,
  body: Uint8Array,
): Promise<Response> {
  const { commands, pack } = commandsIn(body);

  const answers = [encodePkt('unpack ok\n')];
  for (const one of commands) {
    if (options.refusesPush !== undefined) {
      answers.push(encodePkt(`ng ${one.name} ${options.refusesPush}\n`));
      continue;
    }
    const held = (await store.listRefs(name)).find((ref) => ref.name === one.name);
    // Without force, a ref that moved under the pusher is refused.
    if (held !== undefined && held.oid !== one.oldOid) {
      answers.push(encodePkt(`ng ${one.name} non-fast-forward\n`));
      continue;
    }
    if (pack.length > 0) {
      await store.receivePack(name, pack, [
        { name: one.name, oldOid: null, newOid: one.newOid, force: true },
      ]);
    } else {
      await store.updateRefs(name, [
        { name: one.name, oldOid: null, newOid: one.newOid, force: true },
      ]);
    }
    answers.push(encodePkt(`ok ${one.name}\n`));
  }
  answers.push(encodeSpecial(FLUSH));
  return new Response(concatBytes(answers) as BodyInit, { status: 200 });
}

export function fakeRemote(options: FakeRemoteOptions = {}): FakeRemote {
  const root = mkdtempSync(join(tmpdir(), 'parley-fake-remote-'));
  const store: RepositoryStore = localRepositoryStore(root);
  const name = 'origin';
  const seen: string[] = [];
  const pushed: Array<{ oldOid: string; newOid: string; name: string }> = [];
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
      return url.includes('git-receive-pack')
        ? receiveAdvertisement(await store.listRefs(name))
        : uploadAdvertisement();
    }

    if (url.endsWith('/git-receive-pack')) {
      const body = new Uint8Array((init?.body ?? new Uint8Array()) as ArrayBuffer);
      pushed.push(...commandsIn(body).commands);
      return receive(store, name, options, body);
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
    store,
    repository: name,
    ready: ready.then(() => undefined),
    fetch: (async (input: string | URL | Request, init?: RequestInit) =>
      serve(String(input), init)) as unknown as typeof fetch,
    seen: () => [...seen],
    requests: () => requests,
    refs: async () => {
      await ready;
      return store.listRefs(name);
    },
    put: async (ref: string, content: string) => {
      await ready;
      const data = new TextEncoder().encode(content);
      const oid = objectId('blob', data);
      await store.receivePack(name, writePack([{ oid, type: 'blob', data }]), [
        { name: ref, oldOid: null, newOid: oid, force: true },
      ]);
      return oid;
    },
    pushed: () => [...pushed],
    dispose: () => {
      rmSync(root, { recursive: true, force: true });
    },
  };
}

function uploadAdvertisement(): Response {
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

function receiveAdvertisement(refs: ReadonlyArray<{ name: string; oid: string }>): Response {
  const first = refs[0];
  const capabilities = 'report-status delete-refs atomic';
  return new Response(
    concatBytes([
      encodePkt('# service=git-receive-pack\n'),
      encodeSpecial(FLUSH),
      encodePkt(
        first === undefined
          ? `${'0'.repeat(40)} capabilities^{}\u0000${capabilities}\n`
          : `${first.oid} ${first.name}\u0000${capabilities}\n`,
      ),
      ...refs.slice(1).map((ref) => encodePkt(`${ref.oid} ${ref.name}\n`)),
      encodeSpecial(FLUSH),
    ]) as BodyInit,
    { status: 200 },
  );
}

/** The commands a receive-pack body carries, before the pack itself. */
export function commandsIn(body: Uint8Array): {
  commands: Array<{ oldOid: string; newOid: string; name: string }>;
  pack: Uint8Array;
} {
  const flushAt = firstFlush(body);
  const commands = decodePktLines(body.subarray(0, flushAt))
    .filter((line) => line.kind === 'data')
    .map((line) => textOf(line).split('\u0000')[0] ?? '')
    .filter((line) => line.trim().length > 0)
    .map((line) => {
      const [oldOid, newOid, ref] = line.trim().split(' ');
      return { oldOid: oldOid ?? '', newOid: newOid ?? '', name: ref ?? '' };
    });
  return { commands, pack: body.subarray(flushAt + 4) };
}

/** Where the first flush packet sits, which is where a pack begins. */
function firstFlush(bytes: Uint8Array): number {
  let at = 0;
  while (at + 4 <= bytes.length) {
    const length = Number.parseInt(new TextDecoder().decode(bytes.subarray(at, at + 4)), 16);
    if (length === 0) {
      return at;
    }
    at += length < 4 ? 4 : length;
  }
  return bytes.length;
}
