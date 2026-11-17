package hubtest

import (
	"flag"
	"strconv"
)

// HubParallel is the number of tests LimitParallel lets a package run in
// parallel. Every hub of a test process keeps its stores in modernc.org's
// SQLite, whose C memory allocator serialises each allocation of the whole
// process on one lock: past a few hubs at work at once, a parallel test adds
// lock contention, paid for in CPU time, rather than throughput, and the
// suite runs every package's tests at once on the same cores.
const HubParallel = 4

// LimitParallel makes the test binary run at most n tests in parallel,
// unless its command line sets -test.parallel, which keeps its value. The
// TestMain of a package whose tests start many hubs calls it, with
// HubParallel, before m.Run.
func LimitParallel(n int) {
	if !flag.Parsed() {
		flag.Parse()
	}
	given := false
	flag.Visit(func(f *flag.Flag) { given = given || f.Name == "test.parallel" })
	parallel := flag.Lookup("test.parallel")
	if given || parallel == nil {
		return
	}
	if current, err := strconv.Atoi(parallel.Value.String()); err == nil && current > n {
		_ = parallel.Value.Set(strconv.Itoa(n))
	}
}
