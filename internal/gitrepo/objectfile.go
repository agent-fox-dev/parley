package gitrepo

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The reader of this file finds an object's type in git's object store in
// process: in its loose file, or through the index of a pack that holds
// it. Like the readers of refsfile.go, its ok is false for what it does not
// read the way git does: an object it does not find, which may sit in an
// alternate object store or arrive in a pack being written, an index
// other than version 2, and any file it cannot read or does not
// understand. Its caller asks git then.

// Pack object types, from the three bits of an entry's first byte.
const (
	packCommit   = 1
	packTree     = 2
	packBlob     = 3
	packTag      = 4
	packOfsDelta = 6
	packRefDelta = 7
)

// maxDeltaChain bounds the deltas followed to an entry's base; git's own
// default depth is 50.
const maxDeltaChain = 4096

// readObjectType is the type of the object of the full id id in the git
// directory dir: ObjectCommit, ObjectTree, ObjectBlob or "tag".
func readObjectType(dir, id string) (typ string, ok bool) {
	if !isHexID(id, idLen) {
		return "", false
	}
	id = strings.ToLower(id)
	objects := filepath.Join(dir, "objects")
	if typ, found, ok := looseObjectType(objects, id); found || !ok {
		return typ, ok
	}
	raw, err := hex.DecodeString(id)
	if err != nil {
		return "", false
	}
	indexes, err := filepath.Glob(filepath.Join(objects, "pack", "pack-*.idx"))
	if err != nil {
		return "", false
	}
	for _, idx := range indexes {
		typ, found, ok := packedObjectType(idx, raw)
		if !ok {
			return "", false
		}
		if found {
			return typ, true
		}
	}
	return "", false
}

// readObjectTypes is readObjectType of each of ids, in their order; ok is
// false when it leaves the type of any of them to git.
func readObjectTypes(dir string, ids []string) ([]string, bool) {
	types := make([]string, len(ids))
	for i, id := range ids {
		typ, ok := readObjectType(dir, id)
		if !ok {
			return nil, false
		}
		types[i] = typ
	}
	return types, true
}

// looseObjectType is the type of the loose object id below objects; found
// is false when there is no such file.
func looseObjectType(objects, id string) (typ string, found, ok bool) {
	f, err := os.Open(filepath.Join(objects, id[:2], id[2:]))
	if os.IsNotExist(err) {
		return "", false, true
	}
	if err != nil {
		return "", false, false
	}
	defer func() { _ = f.Close() }()
	z, err := zlib.NewReader(bufio.NewReader(f))
	if err != nil {
		return "", false, false
	}
	defer func() { _ = z.Close() }()
	// The header is "<type> <size>\0"; the longest type is six letters.
	header := make([]byte, 32)
	n, err := io.ReadAtLeast(z, header, 1)
	if err != nil && n == 0 {
		return "", false, false
	}
	typ, _, cut := strings.Cut(string(header[:n]), " ")
	switch {
	case !cut:
		return "", false, false
	case typ == ObjectCommit, typ == ObjectTree, typ == ObjectBlob, typ == "tag":
		return typ, true, true
	}
	return "", false, false
}

// packedObjectType is the type of the object of the raw id id in the pack
// whose index is the file idx; found is false when the pack does not hold
// it.
func packedObjectType(idx string, id []byte) (typ string, found, ok bool) {
	index, err := os.Open(idx)
	if err != nil {
		return "", false, false
	}
	defer func() { _ = index.Close() }()
	pack, err := os.Open(strings.TrimSuffix(idx, ".idx") + ".pack")
	if err != nil {
		return "", false, false
	}
	defer func() { _ = pack.Close() }()
	offset, found, ok := packOffset(index, id)
	if !found || !ok {
		return "", found, ok
	}
	for range maxDeltaChain {
		kind, base, baseID, ok := packEntry(pack, offset)
		switch {
		case !ok:
			return "", false, false
		case kind == packCommit:
			return ObjectCommit, true, true
		case kind == packTree:
			return ObjectTree, true, true
		case kind == packBlob:
			return ObjectBlob, true, true
		case kind == packTag:
			return "tag", true, true
		case kind == packOfsDelta:
			offset = base
		case kind == packRefDelta:
			// A thin pack is completed with its bases when it is stored, so
			// the base is in this pack; one that is not is left to git.
			if offset, found, ok = packOffset(index, baseID); !found || !ok {
				return "", false, false
			}
		default:
			return "", false, false
		}
	}
	return "", false, false
}

// packOffset is the offset in its pack of the object of the raw id id, by
// the version 2 pack index index; found is false when the pack does not
// hold it.
func packOffset(index io.ReaderAt, id []byte) (offset int64, found, ok bool) {
	const header, fanout = 8, 256 * 4
	var head [header + fanout]byte
	if _, err := index.ReadAt(head[:], 0); err != nil {
		return 0, false, false
	}
	if !bytes.Equal(head[:4], []byte("\377tOc")) || binary.BigEndian.Uint32(head[4:8]) != 2 {
		return 0, false, false
	}
	table := head[header:]
	count := int64(binary.BigEndian.Uint32(table[255*4:]))
	hi := int64(binary.BigEndian.Uint32(table[int(id[0])*4:]))
	lo := int64(0)
	if id[0] > 0 {
		lo = int64(binary.BigEndian.Uint32(table[(int(id[0])-1)*4:]))
	}
	names := int64(header + fanout)
	var name [20]byte
	for lo < hi {
		mid := lo + (hi-lo)/2
		if _, err := index.ReadAt(name[:], names+mid*20); err != nil {
			return 0, false, false
		}
		switch c := bytes.Compare(name[:], id); {
		case c < 0:
			lo = mid + 1
		case c > 0:
			hi = mid
		default:
			offsets := names + count*20 + count*4
			var word [4]byte
			if _, err := index.ReadAt(word[:], offsets+mid*4); err != nil {
				return 0, false, false
			}
			small := binary.BigEndian.Uint32(word[:])
			if small&0x80000000 == 0 {
				return int64(small), true, true
			}
			var large [8]byte
			if _, err := index.ReadAt(large[:], offsets+count*4+int64(small&0x7fffffff)*8); err != nil {
				return 0, false, false
			}
			return int64(binary.BigEndian.Uint64(large[:])), true, true
		}
	}
	return 0, false, true
}

// packEntry reads the header of the pack entry at offset: its kind, and
// for a delta the offset of its base in the same pack or its base's raw
// id.
func packEntry(pack io.ReaderAt, offset int64) (kind int, base int64, baseID []byte, ok bool) {
	h, ok := readPackHeader(pack, offset)
	return h.kind, h.base, h.baseID, ok
}

// packHeader is the header of a pack entry: its kind; the size of its
// content, a delta's own for a delta; for a delta the offset of its base in
// the same pack or its base's raw id; and the offset of its compressed
// content.
type packHeader struct {
	kind   int
	size   int64
	base   int64
	baseID []byte
	data   int64
}

// readPackHeader reads the header of the pack entry at offset.
func readPackHeader(pack io.ReaderAt, offset int64) (h packHeader, ok bool) {
	// The type and size take at most 10 bytes for a 64-bit size, an offset
	// delta's base as many, a reference delta's 20.
	var buf [32]byte
	n, err := pack.ReadAt(buf[:], offset)
	if err != nil && n == 0 {
		return packHeader{}, false
	}
	b := buf[:n]
	i := 0
	h.kind = int(b[0]>>4) & 7
	h.size = int64(b[0] & 0x0f)
	for shift := 4; b[i]&0x80 != 0; shift += 7 {
		if i++; i >= len(b) || shift > 56 {
			return packHeader{}, false
		}
		h.size |= int64(b[i]&0x7f) << shift
	}
	i++
	switch h.kind {
	case packOfsDelta:
		if i >= len(b) {
			return packHeader{}, false
		}
		c := b[i]
		back := int64(c & 0x7f)
		for c&0x80 != 0 {
			if i++; i >= len(b) || back > 1<<55 {
				return packHeader{}, false
			}
			c = b[i]
			back = (back+1)<<7 | int64(c&0x7f)
		}
		if back <= 0 || back > offset {
			return packHeader{}, false
		}
		h.base, h.data = offset-back, offset+int64(i)+1
	case packRefDelta:
		if i+20 > len(b) {
			return packHeader{}, false
		}
		h.baseID, h.data = bytes.Clone(b[i:i+20]), offset+int64(i)+20
	default:
		h.data = offset + int64(i)
	}
	return h, h.size >= 0
}
