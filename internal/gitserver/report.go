package gitserver

import (
	"net/http"
	"strings"

	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
)

// receivePackResult is the content type of every answer of the push
// exchange.
const receivePackResult = "application/x-git-receive-pack-result"

// The texts of the status report the rules do not word: git's own for a
// pack it cannot unpack, and the hub's for a pack over [git].max_push_size,
// for a push that outlasted [git].timeout and for a failure before the
// rules ran.
const (
	unpackOK        = "ok"
	unpackInvalid   = "invalid pack"
	unpackTooLarge  = "pack exceeds the maximum push size"
	unpackTimedOut  = "push timed out"
	msgUnpackerErr  = "unpacker error"
	msgPushInternal = "internal error; try again"
)

// maxBand is the most data one side-band-64k packet carries: the largest
// pkt-line less its length and its band byte.
const maxBand = 65520 - 4 - 1

// capabilities are what the client of a push asked for on its first
// command.
type capabilities struct {
	reportStatus, sideBand, pushOptions bool
}

// parseCapabilities reads the capability list of a push's first command.
// Those the hub does not act on, such as atomic, which every push is, or
// delete-refs, which it always allows, are ignored.
func parseCapabilities(list string) capabilities {
	var c capabilities
	for _, name := range strings.Fields(list) {
		switch name {
		case "report-status", "report-status-v2":
			// Version 2 adds option lines after a ref's status for a ref the
			// server rewrote; the hub rewrites none, so its report reads the
			// same under both.
			c.reportStatus = true
		case "side-band-64k":
			c.sideBand = true
		case "push-options":
			c.pushOptions = true
		}
	}
	return c
}

// refStatus is one ref of the status report: ok when ng is "", otherwise
// refused with the reason ng.
type refStatus struct{ ref, ng string }

// report is what a push is answered: the unpack status and one status per
// command, in command order, and the lines for the side band, each without
// its line end: the error lines of a refusal and the lines of the mapping
// stage.
type report struct {
	unpack string
	refs   []refStatus
	band   []string
}

// reportOf reports what the ref writer made of a push: every ref ok for an
// accepted push; otherwise each ref with its message and an error line on
// the side band for each refused ref, but not for the refs refused only
// because another was.
func reportOf(res pushrule.Result) report {
	rep := report{unpack: unpackOK}
	for _, u := range res.Updates {
		if !res.Refused() {
			rep.refs = append(rep.refs, refStatus{ref: u.Ref})
			continue
		}
		rep.refs = append(rep.refs, refStatus{u.Ref, u.Message})
		if u.ErrorType != "" {
			rep.band = append(rep.band, "error: "+u.Ref+": "+u.Message)
		}
	}
	return rep
}

// refuseAll reports every ref of updates refused with message, which the
// side band carries once, as an options stage's refusal is reported.
func refuseAll(updates []pushrule.Update, message string) report {
	rep := report{unpack: unpackOK, band: []string{"error: " + message}}
	for _, u := range updates {
		rep.refs = append(rep.refs, refStatus{u.Ref, message})
	}
	return rep
}

// failAll reports that the pack failed with unpack, and every ref of
// updates with ng.
func failAll(updates []pushrule.Update, unpack, ng string) report {
	rep := report{unpack: unpack}
	for _, u := range updates {
		rep.refs = append(rep.refs, refStatus{u.Ref, ng})
	}
	return rep
}

// write answers the push with rep, 200 whatever it says, sending only what
// the client asked for: the status report with report-status, inside side
// band 1 with side-band-64k, and the side band's lines on band 2 before it
// only with side-band-64k, since a client without the side band has no
// place to show them.
func (c capabilities) write(w http.ResponseWriter, rep report) {
	s := begin(w, receivePackResult)
	var status []byte
	if c.reportStatus {
		status = append(status, pktLine("unpack "+rep.unpack+"\n")...)
		for _, r := range rep.refs {
			if r.ng == "" {
				status = append(status, pktLine("ok "+r.ref+"\n")...)
			} else {
				status = append(status, pktLine("ng "+r.ref+" "+r.ng+"\n")...)
			}
		}
		status = append(status, flushPkt...)
	}
	if !c.sideBand {
		_, _ = s.Write(status)
		return
	}
	for _, line := range rep.band {
		_, _ = s.Write(pktLine("\x02" + strings.TrimSuffix(line, "\n") + "\n"))
	}
	for len(status) > 0 {
		n := min(len(status), maxBand)
		_, _ = s.Write(pktLine("\x01" + string(status[:n])))
		status = status[n:]
	}
	_, _ = s.Write([]byte(flushPkt))
}
