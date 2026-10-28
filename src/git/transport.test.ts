import { describe, expect, it, vi } from 'vitest';
import { FLUSH, concatBytes, decodePktLines, encodePkt, encodeSpecial, textOf } from './pktline.js';
import {
  type Remote,
  ZERO_OID,
  demultiplex,
  discover,
  fetchPack,
  lsRefs,
  pushPack,
} from './transport.js';

const bytes = (value: string) => new TextEncoder().encode(value);
const OID = 'a'.repeat(40);
const OTHER = 'b'.repeat(40);

interface Call {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: Uint8Array;
}

/** A remote that answers from a table and records what it was asked. */
function stubRemote(answers: Record<string, Uint8Array>): { remote: Remote; calls: Call[] } {
  const calls: Call[] = [];
  const fetching = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    const body =
      init?.body === undefined ? new Uint8Array() : new Uint8Array(init.body as ArrayBuffer);
    calls.push({
      url,
      method: init?.method ?? 'GET',
      headers: (init?.headers ?? {}) as Record<string, string>,
      body,
    });
    const path = url.split('?')[0]?.split('/').slice(-1)[0] ?? '';
    const key = url.includes('service=') ? (url.split('service=')[1] ?? '') : path;
    const answer = answers[key];
    if (answer === undefined) {
      return Promise.resolve(new Response('no such endpoint', { status: 404 }));
    }
    return Promise.resolve(new Response(answer as BodyInit, { status: 200 }));
  });

  return {
    remote: {
      url: 'https://store.test/git/ns/repo.git',
      headers: { authorization: 'Bearer a-token' },
      fetch: fetching as unknown as typeof fetch,
    },
    calls,
  };
}

const V2_ADVERTISEMENT = concatBytes([
  encodePkt('# service=git-upload-pack\n'),
  encodeSpecial(FLUSH),
  encodePkt('version 2\n'),
  encodePkt('agent=gitty/1.0\n'),
  encodePkt('ls-refs=unborn\n'),
  encodePkt('fetch=shallow filter sideband-all\n'),
  encodeSpecial(FLUSH),
]);

const RECEIVE_ADVERTISEMENT = concatBytes([
  encodePkt('# service=git-receive-pack\n'),
  encodeSpecial(FLUSH),
  encodePkt(`${OID} refs/heads/main\u0000report-status delete-refs atomic side-band-64k\n`),
  encodeSpecial(FLUSH),
]);

describe('discovering what a remote speaks', () => {
  it('reads the version and the capabilities it advertises', async () => {
    const { remote, calls } = stubRemote({ 'git-upload-pack': V2_ADVERTISEMENT });

    const found = await discover(remote, 'git-upload-pack');

    expect(found.version).toBe(2);
    expect(found.capabilities.get('agent')).toBe('gitty/1.0');
    expect(found.capabilities.get('ls-refs')).toBe('unborn');
    expect(found.capabilities.has('fetch')).toBe(true);
    expect(calls[0]?.url).toContain('/info/refs?service=git-upload-pack');
    expect(calls[0]?.headers['authorization']).toBe('Bearer a-token');
    expect(calls[0]?.headers['git-protocol']).toBe('version=2');
  });

  it('reads a version 1 advertisement, where the refs carry the capabilities', async () => {
    const { remote } = stubRemote({ 'git-receive-pack': RECEIVE_ADVERTISEMENT });

    const found = await discover(remote, 'git-receive-pack');

    expect(found.version).toBe(1);
    expect(found.capabilities.has('delete-refs')).toBe(true);
    expect(found.capabilities.has('report-status')).toBe(true);
  });

  it('refuses an advertisement that is not for the service it asked about', async () => {
    const { remote } = stubRemote({
      'git-upload-pack': concatBytes([encodePkt('# service=git-receive-pack\n')]),
    });

    await expect(discover(remote, 'git-upload-pack')).rejects.toThrow();
  });
});

describe('listing a remote’s refs', () => {
  const LS_REFS_ANSWER = concatBytes([
    encodePkt(`${OID} refs/heads/main\n`),
    encodePkt(`${OTHER} refs/tags/v1\n`),
    encodePkt(`${OID} HEAD symref-target:refs/heads/main\n`),
    encodeSpecial(FLUSH),
  ]);

  it('asks with the v2 command and reads every ref back', async () => {
    const { remote, calls } = stubRemote({
      'git-upload-pack': V2_ADVERTISEMENT,
      'git-upload-pack.': LS_REFS_ANSWER,
    });
    // The POST goes to the service path rather than to info/refs.
    const refs = await lsRefs({
      ...remote,
      fetch: (async (input: string | URL, init?: RequestInit) => {
        const url = String(input);
        if (url.includes('info/refs')) {
          return new Response(V2_ADVERTISEMENT as BodyInit, { status: 200 });
        }
        calls.push({
          url,
          method: init?.method ?? 'GET',
          headers: (init?.headers ?? {}) as Record<string, string>,
          body: new Uint8Array(init?.body as ArrayBuffer),
        });
        return new Response(LS_REFS_ANSWER as BodyInit, { status: 200 });
      }) as unknown as typeof fetch,
    });

    expect(refs).toEqual([
      { name: 'refs/heads/main', oid: OID },
      { name: 'refs/tags/v1', oid: OTHER },
      { name: 'HEAD', oid: OID, target: 'refs/heads/main' },
    ]);

    const posted = calls.at(-1);
    const asked = decodePktLines(posted?.body ?? new Uint8Array())
      .filter((l) => l.kind === 'data')
      .map(textOf);
    expect(posted?.method).toBe('POST');
    expect(asked).toContain('command=ls-refs\n');
    expect(asked).toContain('peel\n');
    expect(asked).toContain('symrefs\n');
  });
});

describe('taking a pack from a remote', () => {
  const PACK = bytes('PACK-pretend-bytes');
  const FETCH_ANSWER = concatBytes([
    encodePkt('packfile\n'),
    encodePkt(concatBytes([new Uint8Array([2]), bytes('counting objects\n')])),
    encodePkt(concatBytes([new Uint8Array([1]), PACK])),
    encodeSpecial(FLUSH),
  ]);

  it('asks for what it wants and returns the pack the remote sent', async () => {
    let posted: Uint8Array = new Uint8Array();
    const remote: Remote = {
      url: 'https://store.test/git/ns/repo.git',
      headers: { authorization: 'Bearer a-token' },
      fetch: (async (input: string | URL, init?: RequestInit) => {
        if (String(input).includes('info/refs')) {
          return new Response(V2_ADVERTISEMENT as BodyInit, { status: 200 });
        }
        posted = new Uint8Array(init?.body as ArrayBuffer);
        return new Response(FETCH_ANSWER as BodyInit, { status: 200 });
      }) as unknown as typeof fetch,
    };

    const pack = await fetchPack(remote, { wants: [OID, OTHER], haves: [], filter: 'blob:none' });

    expect(new TextDecoder().decode(pack)).toBe('PACK-pretend-bytes');
    const asked = decodePktLines(posted)
      .filter((l) => l.kind === 'data')
      .map(textOf);
    expect(asked).toContain('command=fetch\n');
    expect(asked).toContain(`want ${OID}\n`);
    expect(asked).toContain(`want ${OTHER}\n`);
    expect(asked).toContain('filter blob:none\n');
    expect(asked).toContain('done\n');
  });

  it('carries what it already has, so the remote sends less', async () => {
    let posted: Uint8Array = new Uint8Array();
    const remote: Remote = {
      url: 'https://store.test/git/ns/repo.git',
      fetch: (async (input: string | URL, init?: RequestInit) => {
        if (String(input).includes('info/refs')) {
          return new Response(V2_ADVERTISEMENT as BodyInit, { status: 200 });
        }
        posted = new Uint8Array(init?.body as ArrayBuffer);
        return new Response(FETCH_ANSWER as BodyInit, { status: 200 });
      }) as unknown as typeof fetch,
    };

    await fetchPack(remote, { wants: [OID], haves: [OTHER] });

    expect(
      decodePktLines(posted)
        .filter((l) => l.kind === 'data')
        .map(textOf),
    ).toContain(`have ${OTHER}\n`);
  });

  it('refuses when the remote answered an error band', async () => {
    const remote: Remote = {
      url: 'https://store.test/git/ns/repo.git',
      fetch: (async (input: string | URL) => {
        if (String(input).includes('info/refs')) {
          return new Response(V2_ADVERTISEMENT as BodyInit, { status: 200 });
        }
        return new Response(
          concatBytes([
            encodePkt('packfile\n'),
            encodePkt(concatBytes([new Uint8Array([3]), bytes('upload-pack died')])),
            encodeSpecial(FLUSH),
          ]) as BodyInit,
          { status: 200 },
        );
      }) as unknown as typeof fetch,
    };

    await expect(fetchPack(remote, { wants: [OID] })).rejects.toThrow('upload-pack died');
  });
});

describe('separating the bands of a sideband answer', () => {
  it('keeps the pack, collects the progress and reports the errors', () => {
    const lines = decodePktLines(
      concatBytes([
        encodePkt(concatBytes([new Uint8Array([1]), bytes('PACK')])),
        encodePkt(concatBytes([new Uint8Array([2]), bytes('working\n')])),
        encodePkt(concatBytes([new Uint8Array([1]), bytes('-more')])),
        encodePkt(concatBytes([new Uint8Array([3]), bytes('it went wrong')])),
      ]),
    );

    const split = demultiplex(lines);

    expect(new TextDecoder().decode(split.pack)).toBe('PACK-more');
    expect(split.progress).toBe('working\n');
    expect(split.errors).toEqual(['it went wrong']);
  });
});

const REPORT = concatBytes([
  encodePkt('unpack ok\n'),
  encodePkt('ok refs/heads/main\n'),
  encodePkt('ng refs/heads/other non-fast-forward\n'),
  encodeSpecial(FLUSH),
]);

async function pushTo(answer: Uint8Array) {
  let posted: Uint8Array = new Uint8Array();
  const remote: Remote = {
    url: 'https://store.test/git/ns/repo.git',
    fetch: (async (input: string | URL, init?: RequestInit) => {
      if (String(input).includes('info/refs')) {
        return new Response(RECEIVE_ADVERTISEMENT as BodyInit, { status: 200 });
      }
      posted = new Uint8Array(init?.body as ArrayBuffer);
      return new Response(answer as BodyInit, { status: 200 });
    }) as unknown as typeof fetch,
  };
  const report = await pushPack(remote, {
    commands: [
      { name: 'refs/heads/main', oldOid: OID, newOid: OTHER },
      { name: 'refs/heads/gone', oldOid: OID, newOid: ZERO_OID },
    ],
    pack: bytes('PACKDATA'),
  });
  return { report, posted };
}
describe('pushing to a remote', () => {
  it('sends each command and the pack, and reads the report back', async () => {
    const { report, posted } = await pushTo(REPORT);

    expect(report.unpacked).toBe(true);
    expect(report.refs).toEqual([
      { name: 'refs/heads/main', ok: true },
      { name: 'refs/heads/other', ok: false, reason: 'non-fast-forward' },
    ]);

    const text = new TextDecoder().decode(posted);
    expect(text).toContain(`${OID} ${OTHER} refs/heads/main`);
    expect(text).toContain(`${OID} ${ZERO_OID} refs/heads/gone`);
    expect(text).toContain('report-status');
    expect(text.endsWith('PACKDATA')).toBe(true);
  });

  it('reports a remote that could not unpack what it was sent', async () => {
    const { report } = await pushTo(
      concatBytes([encodePkt('unpack index-pack failed\n'), encodeSpecial(FLUSH)]),
    );

    expect(report.unpacked).toBe(false);
  });
});
