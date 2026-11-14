package gitserver

import (
	"strings"
	"testing"
)

func TestPktScanner_SeesPackDataAndErrorsInPiecesOfAnySize(t *testing.T) {
	t.Parallel()
	nak := string(pktLine("NAK\n"))
	progress := string(pktLine("\x02Enumerating objects: 3\n"))
	for _, c := range []struct {
		name          string
		answer        string
		pack, errSent bool
	}{
		{"version 0 without side band", nak + "PACK\x00\x00\x00\x02", true, false},
		{"version 0 on the side band", nak + progress + string(pktLine("\x01PACK\x00\x00")), true, false},
		{"version 2 packfile section", string(pktLine("acknowledgments\n")) + "0001" + string(pktLine("packfile\n")) + progress, true, false},
		{"progress alone", nak + progress + flushPkt, false, false},
		{"error packet of git", string(pktLine("ERR upload-pack: not our ref 1234")), false, true},
		{"error on the side band", nak + string(pktLine("\x03fatal: out of memory\n")), false, true},
		{"not pkt-lines", "HTTP/1.1 500\r\n", false, false},
	} {
		for _, size := range []int{1, 3, len(c.answer)} {
			var s pktScanner
			for rest := c.answer; rest != ""; {
				n := min(size, len(rest))
				s.feed([]byte(rest[:n]))
				rest = rest[n:]
			}
			if s.packSent != c.pack || s.errSent != c.errSent {
				t.Errorf("%s in pieces of %d: pack %v error %v, want %v %v (%q)", c.name, size, s.packSent, s.errSent,
					c.pack, c.errSent, strings.ToValidUTF8(c.answer, "?"))
			}
		}
	}
}
