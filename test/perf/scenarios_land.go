package perf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// The sizes of the landing scenarios.
const (
	landChanges            = 20
	landCommits            = 20
	landThroughputChanges  = 120
	landThroughputInFlight = 8
	landConcurrencyChanges = 16
	mergeabilityChanges    = 100
)

// landWait is how long, in seconds, a land request asks the hub to wait
// for its attempt to end before it answers: the most the hub allows.
const landWait = 60

// The poll intervals of an attempt still running after its land request
// answered, and of the mergeability of changes, which is no faster than
// once a second so that the polls do not load the hub they measure.
const (
	attemptPoll      = 250 * time.Millisecond
	mergeabilityPoll = time.Second
)

// statusLanded is the final status of a land attempt that succeeded.
const statusLanded = "landed"

// attempt is a land attempt as the hub shows it.
type attempt struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// landChange lands the change number of the first workspace and returns
// the final status of its attempt. An attempt that ended without landing
// is a status, not an error: the hub answers it with an error whose
// details are the attempt. A refusal that created no attempt is an error.
func (e *env) landChange(ctx context.Context, number int64) (string, error) {
	slug := e.cfg.Workspaces[0]
	path := workspacePath(slug, "/changes/"+strconv.FormatInt(number, 10)+"/land?wait="+strconv.Itoa(landWait))
	resp, err := e.client.do(ctx, http.MethodPost, path, nil)
	if err != nil {
		if re, ok := errors.AsType[*requestError](err); ok && re.status != 0 {
			var env struct {
				Error struct {
					Details attempt `json:"details"`
				} `json:"error"`
			}
			if json.Unmarshal(resp.body, &env) == nil && env.Error.Details.Status != "" {
				return env.Error.Details.Status, nil
			}
		}
		return "", err
	}
	var a attempt
	if err := json.Unmarshal(resp.body, &a); err != nil || a.Status == "" {
		return "", fmt.Errorf("POST %s: the answer is no land attempt", urlPath(apiMount+path))
	}
	// Still running when the hub stopped waiting: read it until it ends.
	for a.Status == "running" {
		t := time.NewTimer(attemptPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", ctx.Err()
		case <-t.C:
		}
		if _, err := e.client.json(ctx, http.MethodGet, workspacePath(slug, "/lands/"+url.PathEscape(a.ID)), nil, &a); err != nil {
			return "", err
		}
	}
	return a.Status, nil
}

// openChanges opens count changes of commits commits each for the scenario
// against target, one after the other, and returns their numbers.
func (e *env) openChanges(ctx context.Context, target, scenario string, count, commits int) ([]int64, error) {
	numbers := make([]int64, 0, count)
	for n := range count {
		number, err := e.openChange(ctx, target, scenario, n, commits)
		if err != nil {
			return nil, err
		}
		numbers = append(numbers, number)
	}
	return numbers, nil
}

// land opens changes of landCommits commits each and lands them one at a
// time, measuring each from the land request to the success of its
// attempt.
func land(ctx context.Context, e *env) (outcome, error) {
	target, err := e.targetBranch(ctx)
	if err != nil {
		return outcome{}, err
	}
	numbers, err := e.openChanges(ctx, target, "land", landChanges, landCommits)
	if err != nil {
		return outcome{}, err
	}
	samples := make([]float64, 0, len(numbers))
	for _, n := range numbers {
		start := time.Now()
		status, err := e.landChange(ctx, n)
		if err != nil {
			return outcome{}, err
		}
		if status != statusLanded {
			return outcome{}, fmt.Errorf("the land attempt of change %d ended %s", n, status)
		}
		samples = append(samples, ms(time.Since(start)))
	}
	return p95Outcome(samples), nil
}

// landThroughput opens one-commit changes and lands them on the target
// with at most landThroughputInFlight land requests in flight, and reports
// the changes landed per minute of the time from the first land request to
// the end of the last attempt.
func landThroughput(ctx context.Context, e *env) (outcome, error) {
	target, err := e.targetBranch(ctx)
	if err != nil {
		return outcome{}, err
	}
	numbers, err := e.openChanges(ctx, target, "land_throughput", landThroughputChanges, 1)
	if err != nil {
		return outcome{}, err
	}
	start := time.Now()
	statuses, err := e.landAll(ctx, numbers, landThroughputInFlight)
	elapsed := time.Since(start)
	if err != nil {
		return outcome{}, err
	}
	landed := 0
	for _, s := range statuses {
		if s == statusLanded {
			landed++
		}
	}
	perMinute := float64(landed) * 60 / elapsed.Seconds()
	return outcome{measured: &perMinute, samples: len(numbers),
		detail: fmt.Sprintf("%d of %d landed in %.3f s", landed, len(numbers), elapsed.Seconds())}, nil
}

// landConcurrency opens one-commit changes that do not conflict, sends
// all their land requests at once and counts the attempts that did not
// land.
func landConcurrency(ctx context.Context, e *env) (outcome, error) {
	target, err := e.targetBranch(ctx)
	if err != nil {
		return outcome{}, err
	}
	numbers, err := e.openChanges(ctx, target, "land_concurrency", landConcurrencyChanges, 1)
	if err != nil {
		return outcome{}, err
	}
	statuses, err := e.landAll(ctx, numbers, len(numbers))
	if err != nil {
		return outcome{}, err
	}
	var failed float64
	for _, s := range statuses {
		if s != statusLanded {
			failed++
		}
	}
	return outcome{measured: &failed, samples: len(numbers)}, nil
}

// landAll lands the changes with at most inFlight land requests at once,
// and returns the final status of each attempt, in the order of numbers.
// The first request that fails without an attempt ends the others.
func (e *env) landAll(ctx context.Context, numbers []int64, inFlight int) ([]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	statuses := make([]string, len(numbers))
	next := make(chan int)
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for range inFlight {
		wg.Go(func() {
			for i := range next {
				status, err := e.landChange(ctx, numbers[i])
				if err != nil {
					once.Do(func() { firstErr = err; cancel() })
					return
				}
				statuses[i] = status
			}
		})
	}
feed:
	for i := range numbers {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return statuses, ctx.Err()
}

// mergeability opens changes against the target, moves the target by
// landing one more, and measures the seconds from that land to the first
// poll at which none of the changes reports mergeable unknown. The hub
// makes them unknown in the transaction that moves the target, so the
// first poll comes right after the land.
func mergeability(ctx context.Context, e *env) (outcome, error) {
	target, err := e.targetBranch(ctx)
	if err != nil {
		return outcome{}, err
	}
	numbers, err := e.openChanges(ctx, target, "mergeability", mergeabilityChanges, 1)
	if err != nil {
		return outcome{}, err
	}
	extra, err := e.openChange(ctx, target, "mergeability", mergeabilityChanges, 1)
	if err != nil {
		return outcome{}, err
	}
	status, err := e.landChange(ctx, extra)
	if err != nil {
		return outcome{}, err
	}
	if status != statusLanded {
		return outcome{}, fmt.Errorf("the land attempt of change %d, which moves the target, ended %s", extra, status)
	}
	landed := time.Now()
	ours := make(map[int64]bool, len(numbers))
	for _, n := range numbers {
		ours[n] = true
	}
	for {
		unknown, err := e.unknownChanges(ctx, target)
		if err != nil {
			return outcome{}, err
		}
		left := 0
		for _, n := range unknown {
			if ours[n] {
				left++
			}
		}
		if left == 0 {
			s := time.Since(landed).Seconds()
			return outcome{measured: &s, samples: len(numbers)}, nil
		}
		// The next poll is sent a full interval after this one was answered,
		// so that the hub never sees two polls closer than that, however long
		// a request takes to reach it.
		t := time.NewTimer(mergeabilityPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return outcome{}, ctx.Err()
		case <-t.C:
		}
	}
}

// unknownChanges are the numbers of the open changes of the first
// workspace into target whose mergeable is unknown, read page by page.
func (e *env) unknownChanges(ctx context.Context, target string) ([]int64, error) {
	base := workspacePath(e.cfg.Workspaces[0], "/changes?limit=200&mergeable=unknown&target_branch="+url.QueryEscape(target))
	var numbers []int64
	cursor := ""
	for {
		path := base
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Items []struct {
				Number int64 `json:"number"`
			} `json:"items"`
			NextCursor *string `json:"next_cursor"`
		}
		if _, err := e.client.json(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			numbers = append(numbers, it.Number)
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return numbers, nil
		}
		cursor = *page.NextCursor
	}
}
