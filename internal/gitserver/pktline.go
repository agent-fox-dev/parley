package gitserver

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// flushPkt is git's flush packet, which ends a section of pkt-lines.
const flushPkt = "0000"

// pktLine is payload framed as one pkt-line: its length, the four bytes of
// the length included, in four hexadecimal digits, then the payload.
func pktLine(payload string) []byte {
	return fmt.Appendf(nil, "%04x%s", len(payload)+4, payload)
}

// peekLen is how much of a packet's payload pktScanner looks at: enough
// for the section line packfile of protocol version 2.
const peekLen = len("packfile\n")

// pktFramer splits pkt-lines that arrive in pieces of any size. feed hands
// the first bytes of each packet with a payload to a judge once they are
// in, and the first four bytes of whatever is no pkt-line, after which the
// framer stops, as it does once a judge says so. Flush, delimiter and
// response-end packets have no payload and are not handed on.
type pktFramer struct {
	done bool
	// head collects the four bytes of the length of the next packet.
	head  [4]byte
	nhead int
	// left is how much of the current packet's payload is still to come,
	// and peek its first bytes, up to the length feed keeps; judged reports
	// that the packet was handed on.
	left   int
	peek   []byte
	judged bool
}

// feed consumes p, keeping up to keep bytes of each payload, and calls
// judge with them, or with raw true and the four bytes that are no length;
// judge returns true to stop the framer.
func (f *pktFramer) feed(p []byte, keep int, judge func(start []byte, raw bool) (stop bool)) {
	for len(p) > 0 && !f.done {
		if f.left == 0 {
			n := copy(f.head[f.nhead:], p)
			f.nhead, p = f.nhead+n, p[n:]
			if f.nhead < len(f.head) {
				return
			}
			f.nhead = 0
			n64, err := strconv.ParseUint(string(f.head[:]), 16, 16)
			switch {
			case err != nil:
				judge(f.head[:], true)
				f.done = true
			case n64 > 4:
				f.left, f.peek, f.judged = int(n64)-4, f.peek[:0], false
			}
			continue
		}
		n := min(f.left, len(p))
		if room := keep - len(f.peek); room > 0 {
			f.peek = append(f.peek, p[:min(room, n)]...)
		}
		f.left, p = f.left-n, p[n:]
		if !f.judged && (f.left == 0 || len(f.peek) == keep) {
			f.judged = true
			f.done = judge(f.peek, false)
		}
	}
}

// pktScanner follows the pkt-lines of an upload-pack answer as they are
// written, in pieces of any size, and records whether pack data or an
// error packet went by. A pack is sent under version 2 in the packfile
// section and under version 0 as a payload starting with PACK, inside the
// side band or after the pkt-lines when the client asked for no side band.
// It stops looking once a pack is on its way or the bytes are not
// pkt-lines.
type pktScanner struct {
	// packSent and errSent record that pack data, or an error packet,
	// went by: an ERR pkt-line or a message on the side band's error
	// channel.
	packSent, errSent bool

	f pktFramer
}

func (s *pktScanner) feed(p []byte) {
	s.f.feed(p, peekLen, func(start []byte, raw bool) bool {
		b := string(start)
		switch {
		case raw:
			s.packSent = s.packSent || b == "PACK"
		case b == "packfile\n" || strings.HasPrefix(b, "\x01PACK"):
			s.packSent = true
		case strings.HasPrefix(b, "ERR ") || strings.HasPrefix(b, "\x03"):
			s.errSent = true
			return false
		default:
			return false
		}
		return true
	})
}

// maxArgLen is how much of a request's pkt-line requestScanner reads: the
// arguments it looks for are short, and a filter longer than this is cut.
const maxArgLen = 1024

// requestScanner follows the pkt-lines of an upload-pack request as git
// reads them and records what the event of an audited fetch reports of it:
// whether the request asks for a shallow history, with a deepen,
// deepen-since or deepen-not line, and the text of its filter line. The
// lines read alike under protocol version 0 and 2. git reads the request
// on a goroutine of its own, so the scanner is locked.
type requestScanner struct {
	mu      sync.Mutex
	shallow bool
	filter  *string
	f       pktFramer
}

func (s *requestScanner) feed(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.feed(p, maxArgLen, func(start []byte, raw bool) bool {
		if raw {
			return true
		}
		name, value, _ := strings.Cut(strings.TrimSuffix(string(start), "\n"), " ")
		switch name {
		case "deepen", "deepen-since", "deepen-not":
			s.shallow = true
		case "filter":
			s.filter = &value
		}
		return false
	})
}

// args are what the scanner found: shallow, and the filter or nil.
func (s *requestScanner) args() (shallow bool, filter *string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shallow, s.filter
}

// scanningReader passes the reads of r on to scan.
type scanningReader struct {
	r    io.Reader
	scan *requestScanner
}

func (t scanningReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.scan.feed(p[:n])
	return n, err
}
