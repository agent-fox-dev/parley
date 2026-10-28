import { describe, expect, it } from 'vitest';
import {
  DELIM,
  FLUSH,
  RESPONSE_END,
  concatBytes,
  decodePktLines,
  encodePkt,
  encodeSpecial,
  textOf,
} from './pktline.js';

const text = (bytes: Uint8Array) => new TextDecoder().decode(bytes);
const bytes = (value: string) => new TextEncoder().encode(value);

describe('framing one line', () => {
  it.each([
    ['a short payload', 'a', '0005a'],
    ['a line with its newline', 'hello\n', '000ahello\n'],
    [
      'the documented example',
      'want 0a53e9ddeaddad63ad106860237bbf53411d11a7\n',
      '0032want 0a53e9ddeaddad63ad106860237bbf53411d11a7\n',
    ],
    ['an empty payload', '', '0004'],
  ])('writes %s as its length and itself', (_name, payload, expected) => {
    expect(text(encodePkt(payload))).toBe(expected);
  });

  it('counts bytes, not characters', () => {
    // Three bytes of UTF-8, so the length is 4 + 3 and not 4 + 1.
    expect(text(encodePkt('中'))).toBe('0007中');
  });

  it('writes the length in lower-case hexadecimal', () => {
    expect(text(encodePkt('K'.repeat(27))).startsWith('001f')).toBe(true);
  });

  it.each([
    ['a flush', FLUSH, '0000'],
    ['a delimiter', DELIM, '0001'],
    ['a response end', RESPONSE_END, '0002'],
  ])('writes %s as its own length and nothing else', (_name, which, expected) => {
    expect(text(encodeSpecial(which))).toBe(expected);
  });
});

describe('reading a stream of lines', () => {
  it('reads back what it wrote', () => {
    const stream = concatBytes([
      encodePkt('first\n'),
      encodePkt('second\n'),
      encodeSpecial(FLUSH),
      encodePkt('third\n'),
    ]);

    const lines = decodePktLines(stream);

    expect(lines.map((line) => line.kind)).toEqual(['data', 'data', 'flush', 'data']);
    expect(lines.filter((l) => l.kind === 'data').map(textOf)).toEqual([
      'first\n',
      'second\n',
      'third\n',
    ]);
  });

  it('tells the three special lines apart', () => {
    const lines = decodePktLines(
      concatBytes([encodeSpecial(FLUSH), encodeSpecial(DELIM), encodeSpecial(RESPONSE_END)]),
    );

    expect(lines.map((line) => line.kind)).toEqual(['flush', 'delim', 'response-end']);
  });

  it('reads an empty data line as data with nothing in it', () => {
    const lines = decodePktLines(encodePkt(''));

    expect(lines).toEqual([{ kind: 'data', payload: new Uint8Array() }]);
  });

  it('reads a real advertisement', () => {
    const advertisement =
      '001e# service=git-upload-pack\n0000000eversion 2\n0014agent=gitty/1.0\n0013ls-refs=unborn\n0000';

    const lines = decodePktLines(bytes(advertisement));

    expect(lines.filter((l) => l.kind === 'data').map(textOf)).toEqual([
      '# service=git-upload-pack\n',
      'version 2\n',
      'agent=gitty/1.0\n',
      'ls-refs=unborn\n',
    ]);
  });

  it('keeps payload bytes exactly, including those that are not text', () => {
    const payload = new Uint8Array([0, 1, 2, 255, 254]);

    const [line] = decodePktLines(encodePkt(payload));

    expect(line?.kind).toBe('data');
    expect(line?.kind === 'data' ? [...line.payload] : []).toEqual([0, 1, 2, 255, 254]);
  });

  it.each([
    ['a length that is not hexadecimal', 'zzzzhello'],
    ['a length shorter than its own header', '0003'],
    ['a line that runs past the end of the stream', '0010short'],
  ])('refuses %s', (_name, malformed) => {
    expect(() => decodePktLines(bytes(malformed))).toThrow();
  });

  it('reads nothing from nothing', () => {
    expect(decodePktLines(new Uint8Array())).toEqual([]);
  });
});
