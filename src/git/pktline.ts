/**
 * The framing every git smart-HTTP exchange is made of: a four-digit hex
 * length followed by that many bytes, the four counted. Three lengths are
 * special and carry no payload.
 */
export const FLUSH = 0;
export const DELIM = 1;
export const RESPONSE_END = 2;

const HEADER = 4;
const MAX_PAYLOAD = 65516;

export type PktLine =
  | { kind: 'data'; payload: Uint8Array }
  | { kind: 'flush' }
  | { kind: 'delim' }
  | { kind: 'response-end' };

const encoder = new TextEncoder();
const decoder = new TextDecoder();

export function concatBytes(parts: readonly Uint8Array[]): Uint8Array {
  const total = parts.reduce((sum, part) => sum + part.length, 0);
  const out = new Uint8Array(total);
  let at = 0;
  for (const part of parts) {
    out.set(part, at);
    at += part.length;
  }
  return out;
}

function hexLength(value: number): Uint8Array {
  return encoder.encode(value.toString(16).padStart(HEADER, '0'));
}

export function encodePkt(payload: string | Uint8Array): Uint8Array {
  const body = typeof payload === 'string' ? encoder.encode(payload) : payload;
  if (body.length > MAX_PAYLOAD) {
    throw new Error(`a pkt-line may carry at most ${String(MAX_PAYLOAD)} bytes`);
  }
  return concatBytes([hexLength(body.length + HEADER), body]);
}

export function encodeSpecial(which: number): Uint8Array {
  return hexLength(which);
}

const SPECIAL: Readonly<Record<number, PktLine>> = {
  [FLUSH]: { kind: 'flush' },
  [DELIM]: { kind: 'delim' },
  [RESPONSE_END]: { kind: 'response-end' },
};

/**
 * Every line in the stream. A malformed length, or a line claiming more
 * bytes than are there, is refused rather than guessed at: a half-read
 * frame means the rest of the stream cannot be trusted either.
 */
export function decodePktLines(bytes: Uint8Array): PktLine[] {
  const lines: PktLine[] = [];
  let at = 0;

  while (at < bytes.length) {
    if (at + HEADER > bytes.length) {
      throw new Error('a pkt-line stream ended inside a length');
    }
    const header = decoder.decode(bytes.subarray(at, at + HEADER));
    if (!/^[0-9a-f]{4}$/.test(header)) {
      throw new Error(`a pkt-line length must be four hex digits, not ${header}`);
    }
    const length = Number.parseInt(header, 16);
    at += HEADER;

    const special = SPECIAL[length];
    if (special !== undefined) {
      lines.push(special);
      continue;
    }
    if (length < HEADER) {
      throw new Error(`a pkt-line cannot be ${String(length)} bytes long`);
    }
    const end = at + length - HEADER;
    if (end > bytes.length) {
      throw new Error('a pkt-line claims more bytes than the stream holds');
    }
    lines.push({ kind: 'data', payload: bytes.subarray(at, end) });
    at = end;
  }

  return lines;
}

export function textOf(line: PktLine): string {
  return line.kind === 'data' ? decoder.decode(line.payload) : '';
}

/**
 * A refusal in git's own framing. A git client shows the person the
 * message of a pkt-line body; the service's JSON envelope it would
 * report as an unreadable failure, so every git route answers this way.
 */
export function pktError(
  status: number,
  message: string,
  headers: Record<string, string> = {},
): Response {
  return new Response(encodePkt(`ERR ${message}\n`) as BodyInit, {
    status,
    headers: {
      'content-type': 'application/x-git-result',
      'cache-control': 'no-cache',
      ...headers,
    },
  });
}
