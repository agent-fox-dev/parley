import { GitRefusal } from './access.js';
import { concatBytes, encodePkt } from './pktline.js';

/**
 * Serving a fetch.
 *
 * The hub speaks version 0 of the protocol, and a client that asks for
 * version 2 falls back to it, which is what the API reference describes
 * and what ADR 10 records the measurements for: version 2's negotiation
 * over HTTP needs the server to remember what it acknowledged between
 * one request and the next, and this service keeps no such state. In
 * version 0 a client resends every `have` each round, so one round
 * narrows the pack to what it lacks.
 */
const SERVICES = ['git-upload-pack', 'git-receive-pack'];

const decoder = new TextDecoder();

/** The refusal for a service parameter the hub does not serve. */
export function refusedService(service: string): GitRefusal | undefined {
  return SERVICES.includes(service)
    ? undefined
    : new GitRefusal(403, 'invalid or missing service parameter');
}

/**
 * The wants and haves a client sent. The body is walked rather than
 * decoded whole, because a client may follow its lines with a pack.
 */
export function negotiation(body: Uint8Array): { wants: string[]; haves: string[] } {
  const wants: string[] = [];
  const haves: string[] = [];

  let at = 0;
  while (at + 4 <= body.byteLength) {
    const length = Number.parseInt(decoder.decode(body.subarray(at, at + 4)), 16);
    if (Number.isNaN(length)) {
      break;
    }
    if (length < 4) {
      at += 4;
      continue;
    }
    const [verb, oid] = decoder
      .decode(body.subarray(at + 4, at + length))
      .trim()
      .split(' ');
    at += length;
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
 * The answer to a fetch: the acknowledgement line, then the pack. The
 * service acknowledges nothing, because the pack it builds already
 * leaves out everything the client said it had.
 */
export function uploadPackResponse(pack: Uint8Array): Uint8Array {
  return concatBytes([encodePkt('NAK\n'), pack]);
}
