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
import { ZERO_OID } from '../git/transport.js';
import type { RefChange, RepositoryStore } from '../platform/repostore.js';
import { localRepositoryStore } from '../platform/local/repostore.js';
import type { ArtifactsBinding, ArtifactsRepoHandle } from '../platform/cloudflare/artifacts.js';

/**
 * An Artifacts namespace that answers the git protocol for real, backed
 * by a repository store on disk. It exists so the Cloudflare adapter's
 * own framing is exercised by the contract suite rather than agreed with
 * by a stub.
 */
export interface FakeArtifacts {
  binding: ArtifactsBinding;
  fetch: typeof fetch;
  /** The token the namespace last minted, so a test can look for it. */
  lastToken: () => string;
  dispose: () => void;
}

const V2_CAPABILITIES = [
  'version 2\n',
  'agent=parley-fake/1.0\n',
  'ls-refs=unborn\n',
  'fetch=shallow filter\n',
  'object-format=sha1\n',
];

const RECEIVE_CAPABILITIES = 'report-status delete-refs atomic ofs-delta side-band-64k';

function nameOf(url: string): string {
  const match = /\/git\/[^/]+\/([^/]+)\.git/.exec(url);
  return match?.[1] ?? '';
}

export function fakeArtifacts(): FakeArtifacts {
  const root = mkdtempSync(join(tmpdir(), 'parley-fake-artifacts-'));
  const store: RepositoryStore = localRepositoryStore(root);
  const known = new Set<string>();
  let minted = '';
  let issued = 0;

  async function advertise(name: string, service: string): Promise<Response> {
    if (service === 'git-upload-pack') {
      return new Response(
        concatBytes([
          encodePkt(`# service=${service}\n`),
          encodeSpecial(FLUSH),
          ...V2_CAPABILITIES.map((line) => encodePkt(line)),
          encodeSpecial(FLUSH),
        ]) as BodyInit,
        { status: 200 },
      );
    }

    const refs = await store.listRefs(name);
    const first = refs[0];
    return new Response(
      concatBytes([
        encodePkt(`# service=${service}\n`),
        encodeSpecial(FLUSH),
        encodePkt(
          first === undefined
            ? `${ZERO_OID} capabilities^{}\u0000${RECEIVE_CAPABILITIES}\n`
            : `${first.oid} ${first.name}\u0000${RECEIVE_CAPABILITIES}\n`,
        ),
        ...refs.slice(1).map((ref) => encodePkt(`${ref.oid} ${ref.name}\n`)),
        encodeSpecial(FLUSH),
      ]) as BodyInit,
      { status: 200 },
    );
  }

  async function uploadPack(name: string, body: Uint8Array): Promise<Response> {
    const asked = decodePktLines(body)
      .filter((line) => line.kind === 'data')
      .map(textOf);

    if (asked.includes('command=ls-refs\n')) {
      const refs = await store.listRefs(name);
      return new Response(
        concatBytes([
          ...refs.map((ref) => encodePkt(`${ref.oid} ${ref.name}\n`)),
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

  async function receivePack(name: string, body: Uint8Array): Promise<Response> {
    // The commands are pkt-lines; the pack is whatever follows the flush.
    const flushAt = findFlush(body);
    const commands = decodePktLines(body.subarray(0, flushAt))
      .filter((line) => line.kind === 'data')
      .map((line) => textOf(line).split('\u0000')[0] ?? '')
      .filter((line) => line.trim().length > 0);
    const pack = body.subarray(flushAt + 4);

    const changes: RefChange[] = commands.map((line) => {
      const [oldOid, newOid, ref] = line.trim().split(' ');
      return {
        name: ref ?? '',
        oldOid: oldOid === ZERO_OID ? null : (oldOid ?? null),
        newOid: newOid === ZERO_OID ? null : (newOid ?? null),
        force: true,
      };
    });

    const answers: Uint8Array[] = [];
    try {
      if (pack.length > 0) {
        await store.receivePack(name, pack, changes);
      } else {
        await store.updateRefs(name, changes);
      }
      answers.push(encodePkt('unpack ok\n'));
      for (const change of changes) {
        answers.push(encodePkt(`ok ${change.name}\n`));
      }
    } catch (err) {
      answers.push(encodePkt('unpack ok\n'));
      for (const change of changes) {
        answers.push(encodePkt(`ng ${change.name} ${(err as Error).message}\n`));
      }
    }
    answers.push(encodeSpecial(FLUSH));
    return new Response(concatBytes(answers) as BodyInit, { status: 200 });
  }

  const handle = (name: string): ArtifactsRepoHandle => ({
    info: async () => Promise.resolve({ remote: `https://fake.artifacts.test/git/ns/${name}.git` }),
    createToken: async () => {
      issued += 1;
      minted = `art_v2_fake_token_${String(issued)}`;
      return Promise.resolve({ plaintext: minted });
    },
    readBlob: async (hash: string) => {
      const object = await store.readObject(name, hash);
      return object === undefined ? null : new Blob([object.data as unknown as ArrayBuffer]);
    },
    readCommit: async (hash: string) => store.readObject(name, hash),
    readTree: async (hash: string) => store.readObject(name, hash),
  });

  return {
    binding: {
      create: async (name: string) => {
        if (known.has(name)) {
          throw Object.assign(new Error('already exists'), { code: 'ALREADY_EXISTS' });
        }
        known.add(name);
        await store.create(name);
        return { remote: `https://fake.artifacts.test/git/ns/${name}.git`, token: 'initial' };
      },
      get: async (name: string) => {
        if (!known.has(name)) {
          throw Object.assign(new Error('not found'), { code: 'NOT_FOUND' });
        }
        return Promise.resolve(handle(name));
      },
      delete: async (name: string) => {
        if (!known.has(name)) {
          throw Object.assign(new Error('not found'), { code: 'NOT_FOUND' });
        }
        known.delete(name);
        await store.remove(name);
        return Promise.resolve(undefined);
      },
      list: async () => Promise.resolve({ repos: [...known].map((name) => ({ name })) }),
    },

    fetch: (async (input: string | URL | Request, init?: RequestInit) => {
      const url = String(input);
      const name = nameOf(url);
      if (url.includes('/info/refs')) {
        const service = url.split('service=')[1] ?? '';
        return advertise(name, service);
      }
      const body = new Uint8Array((init?.body ?? new Uint8Array()) as ArrayBuffer);
      if (url.endsWith('/git-upload-pack')) {
        return uploadPack(name, body);
      }
      if (url.endsWith('/git-receive-pack')) {
        return receivePack(name, body);
      }
      return Promise.resolve(new Response('no such endpoint', { status: 404 }));
    }) as unknown as typeof fetch,

    lastToken: () => minted,
    dispose: () => {
      rmSync(root, { recursive: true, force: true });
    },
  };
}

/** Where the first flush packet sits, which is where the pack begins. */
function findFlush(bytes: Uint8Array): number {
  let at = 0;
  while (at + 4 <= bytes.length) {
    const header = new TextDecoder().decode(bytes.subarray(at, at + 4));
    const length = Number.parseInt(header, 16);
    if (length === 0) {
      return at;
    }
    at += length < 4 ? 4 : length;
  }
  return bytes.length;
}
