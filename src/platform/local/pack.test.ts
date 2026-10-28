import { deflateSync } from 'node:zlib';
import { describe, expect, it } from 'vitest';
import { applyDelta, objectId, readPack, writePack } from './pack.js';

const bytes = (value: string) => new TextEncoder().encode(value);
const text = (value: Uint8Array) => new TextDecoder().decode(value);

describe('the id git gives an object', () => {
  it('is the one git itself computes', () => {
    // `printf '' | git hash-object --stdin` for an empty blob.
    expect(objectId('blob', new Uint8Array())).toBe('e69de29bb2d1d6434b8b29ae775ad8c2e48c5391');
  });

  it('is the documented hash of a known blob', () => {
    // `printf 'hello\n' | git hash-object --stdin`
    expect(objectId('blob', bytes('hello\n'))).toBe('ce013625030ba8dba906f756967f9e9ca394464a');
  });

  it('differs between two types holding the same bytes', () => {
    expect(objectId('blob', bytes('x'))).not.toBe(objectId('commit', bytes('x')));
  });
});

describe('a pack written and read back', () => {
  it('holds every object it was given, with its type and its bytes', () => {
    const objects = [
      { oid: objectId('blob', bytes('first')), type: 'blob' as const, data: bytes('first') },
      { oid: objectId('blob', bytes('second')), type: 'blob' as const, data: bytes('second') },
      {
        oid: objectId('commit', bytes('tree x\n')),
        type: 'commit' as const,
        data: bytes('tree x\n'),
      },
    ];

    const read = readPack(writePack(objects));

    expect(read.map((o) => [o.type, text(o.data)])).toEqual([
      ['blob', 'first'],
      ['blob', 'second'],
      ['commit', 'tree x\n'],
    ]);
    expect(read.map((o) => o.oid)).toEqual(objects.map((o) => o.oid));
  });

  it('holds an object larger than one varint length byte', () => {
    const large = bytes('L'.repeat(5000));
    const read = readPack(writePack([{ oid: objectId('blob', large), type: 'blob', data: large }]));

    expect(read[0]?.data.length).toBe(5000);
  });

  it('holds nothing when it was given nothing', () => {
    expect(readPack(writePack([]))).toEqual([]);
  });

  it('refuses something that is not a pack', () => {
    expect(() => readPack(bytes('NOTAPACK and more'))).toThrow('not a pack');
  });
});

describe('applying a delta to its base', () => {
  it('copies from the base and inserts what the delta carries', () => {
    const base = bytes('the quick brown fox');
    // base length 19, result length 23: copy 'the quick ', insert 'red ', copy 'brown fox'
    const delta = Uint8Array.from([
      19, // base size
      23, // result size
      0x90,
      0x0a, // copy, offset 0 implied, length 10
      4,
      ...bytes('red '), // insert 4 bytes
      0x91,
      0x0a,
      0x09, // copy from offset 10, length 9
    ]);

    expect(text(applyDelta(base, delta))).toBe('the quick red brown fox');
  });

  it('reads a delta that only copies', () => {
    const base = bytes('abcdefghij');
    const delta = Uint8Array.from([10, 5, 0x91, 0x05, 0x05]);

    expect(text(applyDelta(base, delta))).toBe('fghij');
  });
});

describe('a pack holding a reference delta', () => {
  it('resolves the delta against the base the pack carries', () => {
    const base = bytes('the quick brown fox');
    const baseOid = objectId('blob', base);
    const delta = Uint8Array.from([19, 23, 0x90, 0x0a, 4, ...bytes('red '), 0x91, 0x0a, 0x09]);

    // A pack written by hand, because writePack never produces deltas.
    const header = Buffer.alloc(12);
    header.write('PACK', 0, 'latin1');
    header.writeUInt32BE(2, 4);
    header.writeUInt32BE(2, 8);
    const baseEntry = Buffer.concat([
      Buffer.from([(3 << 4) | (base.length & 0x0f)]),
      deflateSync(Buffer.from(base)),
    ]);
    const deltaEntry = Buffer.concat([
      Buffer.from([(7 << 4) | (delta.length & 0x0f)]),
      Buffer.from(baseOid, 'hex'),
      deflateSync(Buffer.from(delta)),
    ]);
    const pack = new Uint8Array(Buffer.concat([header, baseEntry, deltaEntry, Buffer.alloc(20)]));

    const read = readPack(pack);

    expect(read).toHaveLength(2);
    expect(text(read[0]?.data ?? new Uint8Array())).toBe('the quick brown fox');
    expect(text(read[1]?.data ?? new Uint8Array())).toBe('the quick red brown fox');
    expect(read[1]?.type).toBe('blob');
  });
});
