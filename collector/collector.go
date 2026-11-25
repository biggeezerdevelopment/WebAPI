// Package collector polls sources that need a durable cursor and delivers each
// new event to every routed output, in order and at least once.
package collector

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"webapi/api"
	"webapi/config"
	"webapi/output"
)

const (
	// maxPagesPerWindow is where a fetch stops once it has passed its floor.
	maxPagesPerWindow = 100
	// maxPagesPastFloor ends a fetch that still has not passed its floor. The
	// poll then fails loudly instead of refetching a delivered prefix forever.
	maxPagesPastFloor = 10000
	// maxRateLimitRetries is the number of waits for one page within a poll.
	maxRateLimitRetries = 3
	// maxInlineWait is the longest rate-limit wait spent inside a poll. Longer
	// waits end the poll and defer the next one.
	maxInlineWait = 5 * time.Minute
	// maxRateLimitWait caps a server-supplied delay.
	maxRateLimitWait = 24 * time.Hour
	// defaultRateLimitWait applies when a limit response gives no delay.
	defaultRateLimitWait = time.Minute
	// maxOutputBackoff caps the wait before retrying a failing output, which
	// bounds what its backlog fetches cost against the rate limit.
	maxOutputBackoff = time.Hour
	// fetchGroupSpan is how far apart checkpoints can be and still share a fetch.
	fetchGroupSpan = time.Hour
)

// errNoProgress means a fetch hit maxPagesPastFloor without passing its floor.
var errNoProgress = errors.New("no progress possible")

// outputRetry tracks consecutive failed delivery attempts for one output.
type outputRetry struct {
	failures  int
	notBefore time.Time
}

// PageFetcher retrieves one URL. api.Client implements it.
type PageFetcher interface {
	FetchPage(ctx context.Context, pageURL string) (*api.Page, error)
}

// Options configures a Collector.
type Options struct {
	Agent    config.JarvisAgent
	Source   *config.WebAPI
	StateDir string
	Fetcher  PageFetcher
	Outputs  []output.NamedOutput
}

// Collector polls one checkpointed audit-log source.
type Collector struct {
	opts      Options
	src       Source
	statePath string
	now       func() time.Time
	sleep     func(context.Context, time.Duration) error
	notBefore time.Time // used only by the polling goroutine

	mu      sync.Mutex // guards state, busy and retries, and serializes saves
	state   map[string]checkpoint
	busy    map[string]bool
	retries map[string]outputRetry
	wg      sync.WaitGroup
}

// New loads the source's state and gives every routed output without a
// checkpoint one at the initial lookback. Saving it means a restart during an
// outage cannot move that output's starting point forward.
func New(opts Options) (*Collector, error) {
	return newCollector(opts, time.Now)
}

func newCollector(opts Options, now func() time.Time) (*Collector, error) {
	src, err := sourceFor(opts.Source)
	if err != nil {
		return nil, err
	}
	return newCollectorWithSource(opts, src, now)
}

// newCollectorWithSource builds a Collector around an already chosen adapter.
func newCollectorWithSource(opts Options, src Source, now func() time.Time) (*Collector, error) {
	path := statePath(opts.StateDir, opts.Source.Name)
	current := now()
	state, err := loadState(path, opts.Source.Name, current)
	if err != nil {
		return nil, err
	}

	start := current.Add(-time.Duration(opts.Source.Collector.LookbackHours()) * time.Hour).UnixMilli()
	added := false
	for _, out := range opts.Outputs {
		if _, ok := state[out.Name]; !ok {
			state[out.Name] = checkpoint{TimestampMS: start}
			added = true
		}
	}
	if added {
		if err := saveState(path, opts.Source.Name, state); err != nil {
			return nil, err
		}
	}
	return &Collector{
		opts:      opts,
		src:       src,
		statePath: path,
		now:       now,
		sleep:     sleepContext,
		state:     state,
		busy:      make(map[string]bool),
		retries:   make(map[string]outputRetry),
	}, nil
}

// Run polls until ctx is canceled and then waits for deliveries to stop.
func (c *Collector) Run(ctx context.Context) {
	defer c.wg.Wait()
	interval := time.Duration(c.opts.Source.SleepTime) * time.Second
	for {
		if err := c.Poll(ctx); err != nil {
			log.Printf("Error polling collector %q: %v", c.opts.Source.Name, err)
		}
		if c.sleep(ctx, interval) != nil {
			return
		}
	}
}

// Wait blocks until deliveries started by Poll have finished.
func (c *Collector) Wait() { c.wg.Wait() }

// Poll fetches new events and starts one delivery per idle output. It does not
// wait for them, so a slow output cannot hold back the others. An output still
// delivering, or waiting out a failure backoff, is skipped and picks up from
// its checkpoint on a later poll.
//
// Fetches run newest checkpoint first, with failing outputs last, so a deep
// backlog cannot spend the rate limit before caught-up outputs are served.
func (c *Collector) Poll(ctx context.Context) error {
	current := c.now()
	if current.Before(c.notBefore) {
		return nil
	}

	c.mu.Lock()
	pending := make(map[string]checkpoint)
	failing := make(map[string]bool)
	for _, out := range c.opts.Outputs {
		if c.busy[out.Name] {
			continue
		}
		retry, retrying := c.retries[out.Name]
		if retrying && current.Before(retry.notBefore) {
			continue
		}
		pending[out.Name] = c.state[out.Name]
		failing[out.Name] = retrying
	}
	c.mu.Unlock()

	var stalled error
	for len(pending) > 0 {
		floor := nextFloor(pending, failing)
		events, complete, err := c.fetchWindow(ctx, floor)
		if errors.Is(err, errNoProgress) {
			// Only outputs at this floor are stuck. Back them off so the costly
			// fetch is not repeated every poll, and keep serving the others.
			// floor is a pending checkpoint, so this always removes an output.
			stalled = errors.Join(stalled, err)
			for name, cp := range pending {
				if cp.TimestampMS == floor {
					delete(pending, name)
					c.recordStall(name)
				}
			}
			continue
		}
		if err != nil {
			return errors.Join(stalled, err)
		}
		// A truncated window always ends after floor, so the output at floor is
		// always served and the loop terminates.
		end := floor
		if len(events) > 0 && events[len(events)-1].ts > end {
			end = events[len(events)-1].ts
		}

		nowMS := c.now().UnixMilli()
		for _, out := range c.opts.Outputs {
			cp, ok := pending[out.Name]
			// A truncated window may hold only part of the events at end, so an
			// output checkpointed there gets its own fetch.
			if !ok || cp.TimestampMS < floor || (!complete && cp.TimestampMS >= end) {
				continue
			}
			delete(pending, out.Name)
			if batch := selectEvents(events, cp, nowMS); len(batch) > 0 {
				c.startDelivery(ctx, out, batch)
			}
		}
	}
	return stalled
}

// nextFloor picks where the next fetch starts: the oldest checkpoint within
// fetchGroupSpan of the newest one, preferring outputs that are not failing.
// Grouping nearby outputs lets them share one fetch.
func nextFloor(pending map[string]checkpoint, failing map[string]bool) int64 {
	onlyFailing := true
	for name := range pending {
		if !failing[name] {
			onlyFailing = false
			break
		}
	}
	newest := int64(-1)
	for name, cp := range pending {
		if failing[name] == onlyFailing && cp.TimestampMS > newest {
			newest = cp.TimestampMS
		}
	}
	floor := newest
	for name, cp := range pending {
		if failing[name] == onlyFailing && cp.TimestampMS < floor &&
			cp.TimestampMS >= newest-fetchGroupSpan.Milliseconds() {
			floor = cp.TimestampMS
		}
	}
	return floor
}

// recordOutcome updates an output's backoff after a delivery attempt. The first
// failure retries on the next poll. Repeats double the wait, and a failure
// after some progress starts the count again.
func (c *Collector) recordOutcome(name string, failed, progressed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !failed {
		delete(c.retries, name)
		return
	}
	failures := c.retries[name].failures + 1
	if progressed {
		failures = 1
	}
	c.retries[name] = outputRetry{failures: failures, notBefore: c.now().Add(c.retryWait(failures))}
}

// recordStall backs off an output whose fetch could not pass its checkpoint.
// Unlike a delivery failure it waits from the first stall, because each retry
// can cost up to maxPagesPastFloor requests.
func (c *Collector) recordStall(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	failures := c.retries[name].failures + 1
	if failures < 2 {
		failures = 2
	}
	c.retries[name] = outputRetry{failures: failures, notBefore: c.now().Add(c.retryWait(failures))}
}

// retryWait is zero for a first failure and then doubles up to maxOutputBackoff.
func (c *Collector) retryWait(failures int) time.Duration {
	if failures <= 1 {
		return 0
	}
	wait := time.Duration(c.opts.Source.SleepTime) * time.Second
	for i := 1; i < failures && wait < maxOutputBackoff; i++ {
		wait *= 2
	}
	if wait > maxOutputBackoff {
		wait = maxOutputBackoff
	}
	return wait
}

// selectEvents returns sorted events after cp. Events stamped later than the
// local clock wait for a later poll, because a checkpoint must never be in the
// future.
func selectEvents(events []event, cp checkpoint, nowMS int64) []event {
	delivered := make(map[string]bool, len(cp.IDs))
	for _, id := range cp.IDs {
		delivered[id] = true
	}
	var batch []event
	for _, ev := range events {
		if ev.ts > nowMS {
			break
		}
		if ev.ts > cp.TimestampMS || (ev.ts == cp.TimestampMS && !delivered[ev.id]) {
			batch = append(batch, ev)
		}
	}
	return batch
}

func (c *Collector) startDelivery(ctx context.Context, out output.NamedOutput, batch []event) {
	c.mu.Lock()
	c.busy[out.Name] = true
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() {
			c.mu.Lock()
			delete(c.busy, out.Name)
			c.mu.Unlock()
		}()
		c.deliver(ctx, out, batch)
	}()
}

// deliver sends in order and stops at the first failure. Each success is saved
// before the next send, so a restart repeats at most the record in flight.
func (c *Collector) deliver(ctx context.Context, out output.NamedOutput, batch []event) {
	source := c.opts.Source.Name
	for i, ev := range batch {
		if ctx.Err() != nil {
			return
		}
		err := out.Output.Send(ctx, output.Record{
			AgentID:   c.opts.Agent.ID,
			AgentName: c.opts.Agent.Name,
			Source:    source,
			Timestamp: time.UnixMilli(ev.ts),
			Data:      ev.data,
		})
		if err != nil {
			if ctx.Err() != nil {
				return // Shutdown is not an output failure.
			}
			log.Printf("Error delivering event from collector %q to output %q, will resume from its checkpoint: %v", source, out.Name, err)
			c.recordOutcome(out.Name, true, i > 0)
			return
		}
		if err := c.advance(out.Name, ev); err != nil {
			log.Printf("Error saving checkpoint for collector %q output %q: %v", source, out.Name, err)
			c.recordOutcome(out.Name, true, i > 0)
			return
		}
	}
	c.recordOutcome(out.Name, false, true)
}

func (c *Collector) advance(name string, ev event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := c.state[name]
	if ev.ts > cp.TimestampMS {
		cp = checkpoint{TimestampMS: ev.ts, IDs: []string{ev.id}}
	} else {
		// Copied because Poll may be reading the previous slice's contents.
		cp.IDs = append(append([]string(nil), cp.IDs...), ev.id)
	}
	c.state[name] = cp
	return saveState(c.statePath, c.opts.Source.Name, c.state)
}

// fetchWindow reads pages from floor and returns events sorted by timestamp
// then ID, without duplicates. complete is false when the page cap was reached,
// and a truncated window always holds an event newer than floor. Without that,
// a second with more events than the cap would be refetched forever. Events
// before floor are dropped while paging because no output served here needs them.
func (c *Collector) fetchWindow(ctx context.Context, floor int64) ([]event, bool, error) {
	endpoint, err := url.Parse(c.opts.Source.Endpoint)
	if err != nil {
		return nil, false, errors.New("invalid endpoint")
	}
	first, err := c.src.FirstPage(floor, c.now())
	if err != nil {
		return nil, false, err
	}
	if !sameOrigin(first, endpoint) {
		return nil, false, errors.New("refusing first page on a different origin")
	}

	var events []event
	seen := map[string]bool{first.String(): true}
	current := first
	complete := false
	passedFloor := false
	for pages := 1; ; pages++ {
		page, err := c.fetchPage(ctx, current.String())
		if err != nil {
			return nil, false, err
		}
		parsed, next, err := c.src.ParsePage(current, page)
		if err != nil {
			return nil, false, err
		}
		for _, ev := range parsed {
			if ev.ts >= floor {
				events = append(events, ev)
			}
			if ev.ts > floor {
				passedFloor = true
			}
		}

		if next == nil {
			complete = true
			break
		}
		// The request carries credentials, so it never leaves the endpoint's
		// origin, whatever the adapter returned.
		if !sameOrigin(next, endpoint) {
			return nil, false, errors.New("refusing pagination link to a different origin")
		}
		if pages >= maxPagesPerWindow && passedFloor {
			break
		}
		if pages >= maxPagesPastFloor {
			return nil, false, fmt.Errorf("%w: %d pages did not pass the checkpoint, increase per_page", errNoProgress, pages)
		}
		if seen[next.String()] {
			return nil, false, errors.New("pagination loop detected")
		}
		seen[next.String()] = true
		current = next
	}

	// A partial window is a safe prefix only if the server really sorted it.
	if !complete {
		for i := 1; i < len(events); i++ {
			if events[i].ts < events[i-1].ts {
				return nil, false, errors.New("events out of order across a truncated fetch")
			}
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].ts != events[j].ts {
			return events[i].ts < events[j].ts
		}
		return events[i].id < events[j].id
	})
	unique := events[:0]
	ids := make(map[string]bool, len(events))
	for _, ev := range events {
		if !ids[ev.id] {
			ids[ev.id] = true
			unique = append(unique, ev)
		}
	}
	return unique, complete, nil
}


// fetchPage retrieves one page, honoring rate limits.
func (c *Collector) fetchPage(ctx context.Context, pageURL string) (*api.Page, error) {
	for attempt := 0; ; attempt++ {
		page, err := c.opts.Fetcher.FetchPage(ctx, pageURL)
		if err != nil {
			return nil, err
		}
		if page.StatusCode >= 200 && page.StatusCode < 300 {
			return page, nil
		}
		wait, limited := rateLimitWait(page, c.now())
		if !limited {
			return nil, fmt.Errorf("unexpected status code: %d", page.StatusCode)
		}
		if wait > maxInlineWait || attempt >= maxRateLimitRetries {
			c.notBefore = c.now().Add(wait)
			return nil, fmt.Errorf("rate limited, next poll in %s", wait)
		}
		log.Printf("Collector %q rate limited, retrying in %s", c.opts.Source.Name, wait)
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// rateLimitWait reports whether a response is a rate limit and how long to
// wait. A 403 without any limit signal is an authorization failure. A server
// error counts only when it carries Retry-After.
func rateLimitWait(page *api.Page, now time.Time) (time.Duration, bool) {
	limitStatus := page.StatusCode == http.StatusTooManyRequests || page.StatusCode == http.StatusForbidden
	if !limitStatus && page.StatusCode < http.StatusInternalServerError {
		return 0, false
	}
	if wait, ok := parseRetryAfter(page.Header.Get("Retry-After"), now); ok {
		return clampWait(wait), true
	}
	if !limitStatus {
		return 0, false
	}
	if page.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(page.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if reset-now.Unix() > int64(maxRateLimitWait/time.Second) {
				return maxRateLimitWait, true
			}
			return clampWait(time.Unix(reset, 0).Sub(now)), true
		}
		return defaultRateLimitWait, true
	}
	if page.StatusCode == http.StatusTooManyRequests ||
		strings.Contains(strings.ToLower(string(page.Body)), "rate limit") {
		return defaultRateLimitWait, true
	}
	return 0, false
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		if seconds > int64(maxRateLimitWait/time.Second) {
			return maxRateLimitWait, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		return at.Sub(now), true
	}
	return 0, false
}

func clampWait(wait time.Duration) time.Duration {
	if wait < time.Second {
		return time.Second
	}
	if wait > maxRateLimitWait {
		return maxRateLimitWait
	}
	return wait
}

// sameOrigin compares scheme and host, including any port.
func sameOrigin(u, endpoint *url.URL) bool {
	return strings.EqualFold(u.Scheme, endpoint.Scheme) && strings.EqualFold(u.Host, endpoint.Host)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
