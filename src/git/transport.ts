import {
  DELIM,
  FLUSH,
  type PktLine,
  concatBytes,
  decodePktLines,
  encodePkt,
  encodeSpecial,
  textOf,
} from './pktline.js';

export interface Remote {
  /** The repository's URL, without a trailing slash. */
  url: string;
  /** What to send so the remote accepts the request. */
  headers?: Readonly<Record<string, string>>;
  /** Injected so a test can answer without a network. */
  fetch?: typeof fetch;
}

export interface RemoteRef {
  name: string;
  oid: string;
  /** Where a symbolic ref points, for HEAD. */
  target?: string;
}

export interface RefCommand {
  name: string;
  /** The zero oid creates the ref; anything else expects that value. */
  oldOid: string;
  /** The zero oid deletes the ref. */
  newOid: string;
}

export interface PushReport {
  unpacked: boolean;
  /** Each ref the remote answered for, with its reason when refused. */
  refs: Array<{ name: string; ok: boolean; reason?: string }>;
}

export const ZERO_OID = '0'.repeat(40);

const UPLOAD = 'git-upload-pack';
const RECEIVE = 'git-receive-pack';

const decoder = new TextDecoder();

function send(remote: Remote): typeof fetch {
  return remote.fetch ?? fetch;
}

async function bodyOf(response: Response, what: string): Promise<Uint8Array> {
  if (!response.ok) {
    throw new Error(`the remote answered ${String(response.status)} to ${what}`);
  }
  return new Uint8Array(await response.arrayBuffer());
}

/**
 * What a remote says it can do, read from its advertisement. Protocol v2
 * lists its capabilities as their own lines; v1 hangs them off the first
 * ref after a NUL.
 */
export async function discover(
  remote: Remote,
  service: string,
): Promise<{ version: number; capabilities: Map<string, string> }> {
  const response = await send(remote)(`${remote.url}/info/refs?service=${service}`, {
    headers: { ...remote.headers, 'git-protocol': 'version=2' },
  });
  const lines = decodePktLines(await bodyOf(response, 'the advertisement'));
  const data = lines.filter((line) => line.kind === 'data').map(textOf);

  const header = data[0] ?? '';
  if (!header.startsWith(`# service=${service}`)) {
    throw new Error(`the remote advertised ${header.trim()} when asked about ${service}`);
  }

  const capabilities = new Map<string, string>();
  const second = data[1] ?? '';

  if (second.startsWith('version 2')) {
    for (const line of data.slice(2)) {
      const [name, ...rest] = line.trim().split('=');
      capabilities.set(name ?? '', rest.join('='));
    }
    return { version: 2, capabilities };
  }

  // Version 1: the first ref line carries the capabilities after a NUL.
  const advertised = second.split('\u0000')[1] ?? '';
  for (const entry of advertised.trim().split(' ').filter(Boolean)) {
    const [name, ...rest] = entry.split('=');
    capabilities.set(name ?? '', rest.join('='));
  }
  return { version: 1, capabilities };
}

async function command(
  remote: Remote,
  service: string,
  lines: readonly Uint8Array[],
): Promise<PktLine[]> {
  const response = await send(remote)(`${remote.url}/${service}`, {
    method: 'POST',
    headers: {
      ...remote.headers,
      'content-type': `application/x-${service}-request`,
      'git-protocol': 'version=2',
    },
    body: concatBytes(lines) as BodyInit,
  });
  return decodePktLines(await bodyOf(response, service));
}

/** Every ref the remote holds, asked for with the v2 command. */
export async function lsRefs(remote: Remote, prefixes?: readonly string[]): Promise<RemoteRef[]> {
  await discover(remote, UPLOAD);

  const answer = await command(remote, UPLOAD, [
    encodePkt('command=ls-refs\n'),
    encodePkt('object-format=sha1\n'),
    encodeSpecial(DELIM),
    encodePkt('peel\n'),
    encodePkt('symrefs\n'),
    ...(prefixes ?? []).map((prefix) => encodePkt(`ref-prefix ${prefix}\n`)),
    encodeSpecial(FLUSH),
  ]);

  return answer
    .filter((line) => line.kind === 'data')
    .map((line) => textOf(line).trim())
    .filter((line) => line.length > 0)
    .map((line) => {
      const [oid, name, ...attributes] = line.split(' ');
      const symref = attributes.find((a) => a.startsWith('symref-target:'));
      return {
        name: name ?? '',
        oid: oid ?? '',
        ...(symref === undefined ? {} : { target: symref.slice('symref-target:'.length) }),
      };
    });
}

/**
 * Splits a sideband answer: band 1 is the pack, band 2 the remote's
 * progress, band 3 an error it wants the caller to see.
 */
export function demultiplex(lines: readonly PktLine[]): {
  pack: Uint8Array;
  progress: string;
  errors: string[];
} {
  const pack: Uint8Array[] = [];
  const progress: string[] = [];
  const errors: string[] = [];

  for (const line of lines) {
    if (line.kind !== 'data' || line.payload.length === 0) {
      continue;
    }
    const band = line.payload[0];
    const rest = line.payload.subarray(1);
    if (band === 1) {
      pack.push(rest);
    } else if (band === 2) {
      progress.push(decoder.decode(rest));
    } else if (band === 3) {
      errors.push(decoder.decode(rest));
    }
  }

  return { pack: concatBytes(pack), progress: progress.join(''), errors };
}

/**
 * The pack holding what the caller wants and does not already have. The
 * answer is read whole here; streaming it is what a copy of real size
 * will need, and the store interface takes a stream for that reason.
 */
export async function fetchPack(
  remote: Remote,
  request: { wants: readonly string[]; haves?: readonly string[]; filter?: string },
): Promise<Uint8Array> {
  await discover(remote, UPLOAD);

  const answer = await command(remote, UPLOAD, [
    encodePkt('command=fetch\n'),
    encodePkt('object-format=sha1\n'),
    encodeSpecial(DELIM),
    encodePkt('thin-pack\n'),
    encodePkt('ofs-delta\n'),
    ...(request.filter === undefined ? [] : [encodePkt(`filter ${request.filter}\n`)]),
    ...request.wants.map((oid) => encodePkt(`want ${oid}\n`)),
    ...(request.haves ?? []).map((oid) => encodePkt(`have ${oid}\n`)),
    encodePkt('done\n'),
    encodeSpecial(FLUSH),
  ]);

  // Everything before the packfile section header is the remote telling
  // us what it decided; the pack itself follows.
  const start = answer.findIndex((line) => line.kind === 'data' && textOf(line) === 'packfile\n');
  const split = demultiplex(start === -1 ? answer : answer.slice(start + 1));
  if (split.errors.length > 0) {
    throw new Error(split.errors.join('; ').trim());
  }
  return split.pack;
}

/** Sends the commands and the pack, and reads the remote's report back. */
export async function pushPack(
  remote: Remote,
  request: { commands: readonly RefCommand[]; pack: Uint8Array },
): Promise<PushReport> {
  const { capabilities } = await discover(remote, RECEIVE);
  const wanted = ['report-status', 'side-band-64k'].filter((name) => capabilities.has(name));

  const commands = request.commands.map((one, index) =>
    encodePkt(
      index === 0
        ? `${one.oldOid} ${one.newOid} ${one.name}\u0000${wanted.join(' ')}\n`
        : `${one.oldOid} ${one.newOid} ${one.name}\n`,
    ),
  );

  const response = await send(remote)(`${remote.url}/${RECEIVE}`, {
    method: 'POST',
    headers: {
      ...remote.headers,
      'content-type': `application/x-${RECEIVE}-request`,
    },
    body: concatBytes([...commands, encodeSpecial(FLUSH), request.pack]) as BodyInit,
  });

  const answered = decodePktLines(await bodyOf(response, RECEIVE));
  const sideband = demultiplex(answered);
  const report = sideband.pack.length > 0 ? decodePktLines(sideband.pack) : answered;

  const lines = report
    .filter((line) => line.kind === 'data')
    .map((line) => textOf(line).trim())
    .filter((line) => line.length > 0);

  return {
    unpacked: lines.includes('unpack ok'),
    refs: lines
      .filter((line) => line.startsWith('ok ') || line.startsWith('ng '))
      .map((line) => {
        const [status, name, ...reason] = line.split(' ');
        return status === 'ok'
          ? { name: name ?? '', ok: true }
          : { name: name ?? '', ok: false, reason: reason.join(' ') };
      }),
  };
}
