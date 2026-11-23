package collector

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"webapi/api"
)

// floorStuckFetcher never passes one floor but serves any other normally.
type floorStuckFetcher struct {
	stuck  *stuckFetcher
	normal PageFetcher
	floor  string
}

func (f *floorStuckFetcher) FetchPage(ctx context.Context, pageURL string) (*api.Page, error) {
	if strings.Contains(pageURL, "after=") || strings.Contains(pageURL, f.floor) {
		return f.stuck.FetchPage(ctx, pageURL)
	}
	return f.normal.FetchPage(ctx, pageURL)
}

func TestStalledOutputBacksOffWithoutBlockingOthers(t *testing.T) {
	stuckTS := ms(-2 * time.Hour.Milliseconds())
	a := newAuditServer(t, testEvent{ID: "new", TS: ms(-1000)})
	stalled, healthy := &fakeOutput{}, &fakeOutput{}
	c := build(t, a, t.TempDir(), map[string]*fakeOutput{"stalled": stalled, "healthy": healthy})
	c.state["stalled"] = checkpoint{TimestampMS: stuckTS}
	c.state["healthy"] = checkpoint{TimestampMS: ms(-5000)}
	stamp := time.UnixMilli(stuckTS).UTC().Format("2006-01-02T15:04:05Z")
	fetcher := &floorStuckFetcher{
		stuck: &stuckFetcher{ts: stuckTS}, normal: c.opts.Fetcher,
		floor: url.QueryEscape("created:>=" + stamp),
	}
	c.opts.Fetcher = fetcher

	err := c.Poll(context.Background())
	c.Wait()
	if !errors.Is(err, errNoProgress) {
		t.Fatalf("err = %v, want errNoProgress", err)
	}
	wantIDs(t, healthy, "new")
	wantIDs(t, stalled)

	// The stalled output waits, so the next poll does not repeat the fetch.
	calls := fetcher.stuck.calls
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	c.Wait()
	if fetcher.stuck.calls != calls {
		t.Fatalf("stalled fetch repeated during backoff: %d more pages", fetcher.stuck.calls-calls)
	}
}
