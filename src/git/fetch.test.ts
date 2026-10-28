import { describe, expect, it } from 'vitest';
import { FLUSH, concatBytes, decodePktLines, encodePkt, encodeSpecial, textOf } from './pktline.js';
import { negotiation, refusedService, uploadPackResponse } from './fetch.js';

describe('the service a client asks for', () => {
  it.each([
    ['nothing at all', ''],
    ['a service nobody serves', 'git-make-coffee'],
    ['a plausible misspelling', 'git-upload-packs'],
    ['one with a trailing space', 'git-upload-pack '],
  ])('is refused when it is %s', (_what, service) => {
    const refusal = refusedService(service);

    expect(refusal?.status).toBe(403);
    expect(refusal?.message).toBe('invalid or missing service parameter');
  });

  it.each(['git-upload-pack', 'git-receive-pack'])('is served when it is %s', (service) => {
    expect(refusedService(service)).toBeUndefined();
  });
});

describe('what a client asked for and what it already has', () => {
  it('is read from the lines it sent, in the order it sent them', () => {
    const wanted = 'a'.repeat(40);
    const held = 'b'.repeat(40);
    const alsoHeld = 'c'.repeat(40);

    const asked = negotiation(
      concatBytes([
        encodePkt(`want ${wanted} multi_ack side-band-64k\n`),
        encodeSpecial(FLUSH),
        encodePkt(`have ${held}\n`),
        encodePkt(`have ${alsoHeld}\n`),
        encodePkt('done\n'),
      ]),
    );

    expect(asked).toEqual({ wants: [wanted], haves: [held, alsoHeld] });
  });

  it('is empty for a body that carries neither', () => {
    expect(negotiation(concatBytes([encodeSpecial(FLUSH)]))).toEqual({ wants: [], haves: [] });
  });

  it('stops at bytes that are not pkt-lines, which is where a pack begins', () => {
    const asked = negotiation(
      concatBytes([
        encodePkt(`want ${'d'.repeat(40)}\n`),
        new Uint8Array([0x50, 0x41, 0x43, 0x4b, 0, 0, 0, 2]),
      ]),
    );

    expect(asked.wants).toHaveLength(1);
  });
});

describe('the answer to a fetch', () => {
  it('acknowledges nothing and carries the pack after it', () => {
    const pack = new Uint8Array([1, 2, 3, 4, 5]);

    const body = uploadPackResponse(pack);
    const lines = decodePktLines(body.subarray(0, 8));

    expect(lines.map((line) => textOf(line))).toEqual(['NAK\n']);
    expect([...body.subarray(8)]).toEqual([...pack]);
  });
});
