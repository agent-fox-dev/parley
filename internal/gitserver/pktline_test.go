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

func TestRequestScanner_SeesTheShallowAndFilterLinesOfEitherProtocolInPiecesOfAnySize(t *testing.T) {
	t.Parallel()
	want := string(pktLine("want 1234567890123456789012345678901234567890 side-band-64k filter\n"))
	v2 := string(pktLine("command=fetch\n")) + string(pktLine("agent=git/2.56\n")) + "0001"
	for _, c := range []struct {
		name, request, filter string
		shallow               bool
	}{
		{"version 0, deepen", want + string(pktLine("deepen 1\n")) + flushPkt, "", true},
		{"version 0, deepen-since and a filter", want + string(pktLine("deepen-since 1700000000\n")) + string(pktLine("filter blob:none\n")) + flushPkt, "blob:none", true},
		{"version 2, deepen-not", v2 + want + string(pktLine("deepen-not refs/heads/old\n")) + flushPkt, "", true},
		{"version 2, a filter alone", v2 + string(pktLine("filter tree:0\n")) + string(pktLine("done\n")) + flushPkt, "tree:0", false},
		{"version 2, neither", v2 + string(pktLine("deepen-relative\n")) + string(pktLine("done\n")) + flushPkt, "", false},
	} {
		for _, size := range []int{1, 5, len(c.request)} {
			var s requestScanner
			for rest := c.request; rest != ""; {
				n := min(size, len(rest))
				s.feed([]byte(rest[:n]))
				rest = rest[n:]
			}
			shallow, filter := s.args()
			if got := ""; shallow != c.shallow || (filter == nil) != (c.filter == "") || (filter != nil && *filter != c.filter) {
				if filter != nil {
					got = *filter
				}
				t.Errorf("%s in pieces of %d: shallow %v filter %q, want %v %q", c.name, size, shallow, got, c.shallow, c.filter)
			}
		}
	}
}
