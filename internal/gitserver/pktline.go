package gitserver

import (
	"fmt"
	"strconv"
	"strings"
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

	done bool
	// head collects the four bytes of the length of the next packet.
	head  [4]byte
	nhead int
	// left is how much of the current packet's payload is still to come,
	// and peek its first bytes, up to peekLen; judged reports that the
	// packet was looked at.
	left   int
	peek   []byte
	judged bool
}

func (s *pktScanner) feed(p []byte) {
	for len(p) > 0 && !s.done {
		if s.left == 0 {
			n := copy(s.head[s.nhead:], p)
			s.nhead, p = s.nhead+n, p[n:]
			if s.nhead < len(s.head) {
				return
			}
			s.nhead = 0
			s.startPacket(string(s.head[:]))
			continue
		}
		n := min(s.left, len(p))
		if room := peekLen - len(s.peek); room > 0 {
			s.peek = append(s.peek, p[:min(room, n)]...)
		}
		s.left, p = s.left-n, p[n:]
		if !s.judged && (s.left == 0 || len(s.peek) == peekLen) {
			s.judge()
		}
	}
}

// startPacket reads the length head of the next packet.
func (s *pktScanner) startPacket(head string) {
	if head == "PACK" {
		s.packSent, s.done = true, true
		return
	}
	n, err := strconv.ParseUint(head, 16, 16)
	switch {
	case err != nil:
		s.done = true
	case n > 4:
		s.left, s.peek, s.judged = int(n)-4, s.peek[:0], false
	}
	// A flush, delimiter or response-end packet, or an empty one, has no
	// payload.
}

// judge looks at the first bytes of the current packet.
func (s *pktScanner) judge() {
	s.judged = true
	p := string(s.peek)
	switch {
	case p == "packfile\n" || strings.HasPrefix(p, "\x01PACK"):
		s.packSent, s.done = true, true
	case strings.HasPrefix(p, "ERR ") || strings.HasPrefix(p, "\x03"):
		s.errSent = true
	}
}
