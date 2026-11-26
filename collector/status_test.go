package collector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"webapi/api"
	"webapi/config"
	"webapi/output"
)

type fakeStatus struct {
	mu          sync.Mutex
	started     int
	succeeded   int
	pollErrs    []error
	delivered   map[string]int
	failed      map[string][]error
	checkpoints map[string]time.Time
}

func newFakeStatus() *fakeStatus {
	return &fakeStatus{delivered: map[string]int{}, failed: map[string][]error{}, checkpoints: map[string]time.Time{}}
}

func (f *fakeStatus) PollStarted()   { f.mu.Lock(); defer f.mu.Unlock(); f.started++ }
func (f *fakeStatus) PollSucceeded() { f.mu.Lock(); defer f.mu.Unlock(); f.succeeded++ }
func (f *fakeStatus) PollFailed(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pollErrs = append(f.pollErrs, err)
}
func (f *fakeStatus) Delivered(o string) { f.mu.Lock(); defer f.mu.Unlock(); f.delivered[o]++ }
func (f *fakeStatus) DeliveryFailed(o string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed[o] = append(f.failed[o], err)
}
func (f *fakeStatus) Checkpointed(o string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkpoints[o] = at
}

type downOutput struct{}

func (downOutput) Send(context.Context, output.Record) error { return errors.New("down") }
func (downOutput) Close() error                              { return nil }

func buildWithStatus(t *testing.T, cfg *config.WebAPI, src Source, out output.Output, st StatusHook) *Collector {
	t.Helper()
	c, err := newCollectorWithSource(Options{
		Agent: config.JarvisAgent{ID: "id", Name: "agent"}, Source: cfg, StateDir: t.TempDir(),
		Fetcher: api.NewClient(cfg), Outputs: []output.NamedOutput{{Name: "o", Output: out}}, Status: st,
	}, src, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

var statusRecords = []cursorRecord{{"r1", ms(-3000)}, {"r2", ms(-2000)}, {"r3", ms(-500)}}

func TestStatusSeedsCheckpointAtStartup(t *testing.T) {
	st := newFakeStatus()
	cfg := cursorConfig("https://example.test/events")
	buildWithStatus(t, cfg, &cursorSource{endpoint: cfg.Endpoint}, &keyOutput{}, st)
	want := time.UnixMilli(testNow.Add(-time.Hour).UnixMilli())
	if got := st.checkpoints["o"]; !got.Equal(want) {
		t.Fatalf("checkpoint = %v, want %v", got, want)
	}
	if st.started != 0 || st.succeeded != 0 {
		t.Fatal("startup must not record a poll")
	}
}

func TestStatusRecordsSuccessfulPollAndDelivery(t *testing.T) {
	srv := cursorServer(t, statusRecords)
	cfg := cursorConfig(srv.URL + "/events")
	st := newFakeStatus()
	c := buildWithStatus(t, cfg, &cursorSource{endpoint: cfg.Endpoint}, &keyOutput{}, st)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Wait()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.started != 1 || st.succeeded != 1 || len(st.pollErrs) != 0 {
		t.Fatalf("started=%d succeeded=%d errs=%v", st.started, st.succeeded, st.pollErrs)
	}
	if st.delivered["o"] != 3 || len(st.failed["o"]) != 0 {
		t.Fatalf("delivered=%v failed=%v", st.delivered, st.failed)
	}
	if got := st.checkpoints["o"]; !got.Equal(time.UnixMilli(ms(-500))) {
		t.Fatalf("checkpoint = %v", got)
	}
}

func TestStatusRecordsFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	cfg := cursorConfig(srv.URL + "/events")
	st := newFakeStatus()
	c := buildWithStatus(t, cfg, &cursorSource{endpoint: cfg.Endpoint}, &keyOutput{}, st)
	if err := c.Poll(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if st.started != 1 || st.succeeded != 0 || len(st.pollErrs) != 1 {
		t.Fatalf("started=%d succeeded=%d errs=%v", st.started, st.succeeded, st.pollErrs)
	}
}

func TestStatusIgnoresSkippedPoll(t *testing.T) {
	cfg := cursorConfig("https://example.test/events")
	st := newFakeStatus()
	c := buildWithStatus(t, cfg, &cursorSource{endpoint: cfg.Endpoint}, &keyOutput{}, st)
	c.notBefore = testNow.Add(time.Hour)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.started != 0 || st.succeeded != 0 || len(st.pollErrs) != 0 {
		t.Fatal("a skipped poll must record nothing")
	}
}

func TestStatusCountsDeliveryFailureOnce(t *testing.T) {
	srv := cursorServer(t, statusRecords)
	cfg := cursorConfig(srv.URL + "/events")
	st := newFakeStatus()
	c := buildWithStatus(t, cfg, &cursorSource{endpoint: cfg.Endpoint}, downOutput{}, st)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Wait()
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.failed["o"]) != 1 || st.delivered["o"] != 0 || st.succeeded != 1 {
		t.Fatalf("failed=%v delivered=%v succeeded=%d", st.failed, st.delivered, st.succeeded)
	}
}

func TestStatusIgnoresCanceledPoll(t *testing.T) {
	srv := cursorServer(t, statusRecords)
	cfg := cursorConfig(srv.URL + "/events")
	st := newFakeStatus()
	c := buildWithStatus(t, cfg, &cursorSource{endpoint: cfg.Endpoint}, &keyOutput{}, st)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Poll(ctx); err == nil {
		t.Fatal("expected error from canceled poll")
	}
	c.Wait()
	if st.succeeded != 0 || len(st.pollErrs) != 0 || len(st.failed["o"]) != 0 {
		t.Fatalf("succeeded=%d errs=%v failed=%v", st.succeeded, st.pollErrs, st.failed)
	}
}
