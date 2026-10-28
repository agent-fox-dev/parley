import { Inflate, constants, deflateSync } from 'node:zlib';
import { createHash } from 'node:crypto';

/**
 * Reading and writing git pack files, for the store that keeps a bare
 * repository of its own. The Cloudflare store never needs this: its
 * binding reads objects for it.
 */
export type ObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface PackedObject {
  oid: string;
  type: ObjectType;
  data: Uint8Array;
}

const TYPES: Readonly<Record<number, ObjectType>> = {
  1: 'commit',
  2: 'tree',
  3: 'blob',
  4: 'tag',
};

const TYPE_CODES: Readonly<Record<ObjectType, number>> = {
  commit: 1,
  tree: 2,
  blob: 3,
  tag: 4,
};

/** The object id git gives a thing: SHA-1 over its header and its bytes. */
export function objectId(type: ObjectType, data: Uint8Array): string {
  const header = Buffer.from(`${type} ${String(data.length)}\u0000`, 'latin1');
  return createHash('sha1').update(header).update(data).digest('hex');
}

interface CountingInflate extends Inflate {
  // eslint-disable-next-line no-underscore-dangle -- node's only sync inflate that reports input consumed
  _processChunk(chunk: Buffer, flushFlag: number): Buffer;
}

/**
 * Inflates one zlib stream out of a longer buffer, answering with its
 * bytes and how much input it took. A pack stores one object after
 * another with no length between them, so the second number is what lets
 * the reader find the next object.
 */
function inflateOne(bytes: Uint8Array, at: number): { data: Buffer; consumed: number } {
  const slice = Buffer.from(bytes.buffer, bytes.byteOffset + at, bytes.length - at);
  const inflater = new Inflate() as CountingInflate;
  // eslint-disable-next-line no-underscore-dangle -- see the declaration above
  const data = inflater._processChunk(slice, constants.Z_SYNC_FLUSH);
  return { data, consumed: inflater.bytesWritten };
}

function readVarint(bytes: Uint8Array, at: number): { value: number; at: number } {
  let value = 0;
  let shift = 0;
  let cursor = at;
  for (;;) {
    const byte = bytes[cursor] ?? 0;
    cursor += 1;
    value |= (byte & 0x7f) << shift;
    shift += 7;
    if ((byte & 0x80) === 0) {
      return { value, at: cursor };
    }
  }
}

/** Applies a git delta to its base, as the pack format defines it. */
export function applyDelta(base: Uint8Array, delta: Uint8Array): Uint8Array {
  let at = readVarint(delta, 0).at;
  const sized = readVarint(delta, at);
  at = sized.at;

  const out = new Uint8Array(sized.value);
  let written = 0;

  while (at < delta.length) {
    const instruction = delta[at] ?? 0;
    at += 1;

    if ((instruction & 0x80) !== 0) {
      let offset = 0;
      let length = 0;
      for (let bit = 0; bit < 4; bit += 1) {
        if ((instruction & (1 << bit)) !== 0) {
          offset |= (delta[at] ?? 0) << (bit * 8);
          at += 1;
        }
      }
      for (let bit = 0; bit < 3; bit += 1) {
        if ((instruction & (0x10 << bit)) !== 0) {
          length |= (delta[at] ?? 0) << (bit * 8);
          at += 1;
        }
      }
      if (length === 0) {
        length = 0x10000;
      }
      out.set(base.subarray(offset, offset + length), written);
      written += length;
      continue;
    }

    // A small instruction inserts the bytes that follow it.
    out.set(delta.subarray(at, at + instruction), written);
    written += instruction;
    at += instruction;
  }

  return out;
}

/**
 * Every object a pack holds, with its deltas resolved. A pack is read
 * whole: the store that uses this keeps repositories of the size one
 * host can hold.
 */
export function readPack(pack: Uint8Array): PackedObject[] {
  const header = Buffer.from(pack.buffer, pack.byteOffset, 4).toString('latin1');
  if (header !== 'PACK') {
    throw new Error('this is not a pack');
  }
  const view = new DataView(pack.buffer, pack.byteOffset, pack.byteLength);
  const count = view.getUint32(8);

  const objects: PackedObject[] = [];
  const byOffset = new Map<number, PackedObject>();
  const byOid = new Map<string, PackedObject>();
  let at = 12;

  for (let index = 0; index < count; index += 1) {
    const offset = at;
    let byte = pack[at] ?? 0;
    const typeCode = (byte >> 4) & 7;
    at += 1;
    while ((byte & 0x80) !== 0) {
      byte = pack[at] ?? 0;
      at += 1;
    }

    if (typeCode === 6) {
      // An offset delta names its base by how far back it sits.
      let shifted = pack[at] ?? 0;
      at += 1;
      let distance = shifted & 0x7f;
      while ((shifted & 0x80) !== 0) {
        shifted = pack[at] ?? 0;
        at += 1;
        distance = ((distance + 1) << 7) | (shifted & 0x7f);
      }
      const inflated = inflateOne(pack, at);
      at += inflated.consumed;
      const base = byOffset.get(offset - distance);
      if (base === undefined) {
        throw new Error('a pack delta names a base that is not in the pack');
      }
      const data = applyDelta(base.data, inflated.data);
      const resolved: PackedObject = { oid: objectId(base.type, data), type: base.type, data };
      objects.push(resolved);
      byOffset.set(offset, resolved);
      byOid.set(resolved.oid, resolved);
      continue;
    }

    if (typeCode === 7) {
      const baseOid = Buffer.from(pack.subarray(at, at + 20)).toString('hex');
      at += 20;
      const inflated = inflateOne(pack, at);
      at += inflated.consumed;
      const base = byOid.get(baseOid);
      if (base === undefined) {
        throw new Error('a pack delta names a base that is not in the pack');
      }
      const data = applyDelta(base.data, inflated.data);
      const resolved: PackedObject = { oid: objectId(base.type, data), type: base.type, data };
      objects.push(resolved);
      byOffset.set(offset, resolved);
      byOid.set(resolved.oid, resolved);
      continue;
    }

    const type = TYPES[typeCode];
    if (type === undefined) {
      throw new Error(`a pack holds an object of unknown type ${String(typeCode)}`);
    }
    const inflated = inflateOne(pack, at);
    at += inflated.consumed;
    const object: PackedObject = { oid: objectId(type, inflated.data), type, data: inflated.data };
    objects.push(object);
    byOffset.set(offset, object);
    byOid.set(object.oid, object);
  }

  return objects;
}

function writeVarintHeader(type: ObjectType, size: number): Uint8Array {
  const bytes: number[] = [];
  let value = size;
  let first = (TYPE_CODES[type] << 4) | (value & 0x0f);
  value >>= 4;
  while (value > 0) {
    bytes.push(first | 0x80);
    first = value & 0x7f;
    value >>= 7;
  }
  bytes.push(first);
  return Uint8Array.from(bytes);
}

/** A pack holding exactly these objects, with no deltas between them. */
export function writePack(objects: readonly PackedObject[]): Uint8Array {
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'latin1');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);

  const parts: Buffer[] = [header];
  for (const object of objects) {
    parts.push(Buffer.from(writeVarintHeader(object.type, object.data.length)));
    parts.push(deflateSync(Buffer.from(object.data)));
  }

  const body = Buffer.concat(parts);
  const checksum = createHash('sha1').update(body).digest();
  return new Uint8Array(Buffer.concat([body, checksum]));
}
