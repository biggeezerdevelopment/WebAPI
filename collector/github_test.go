package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"webapi/api"
	"webapi/config"
	"webapi/output"
)

var testNow = time.UnixMilli(1_700_000_000_000)

func ms(offset int64) int64 { return testNow.UnixMilli() + offset }

type testEvent struct {
	ID string `json:"_document_id"`
	TS int64  `json:"@timestamp"`
}

// fakeOutput records delivered IDs and fails while failAt says so.
type fakeOutput struct {
	mu     sync.Mutex
	ids    []string
	failAt func(id string) bool
}

func (f *fakeOutput) Send(_ context.Context, rec output.Record) error {
	var ev testEvent
	if err := json.Unmarshal(rec.Data, &ev); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAt != nil && f.failAt(ev.ID) {
		return errors.New("down")
	}
	f.ids = append(f.ids, ev.ID)
	return nil
}

func (f *fakeOutput) Close() error { return nil }

func (f *fakeOutput) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ids...)
}

func (f *fakeOutput) setFail(fn func(string) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAt = fn
}

// auditServer serves its events filtered by nothing, paged by perPage.
type auditServer struct {
	mu      sync.Mutex
	events  []testEvent
	perPage int
	handler func(w http.ResponseWriter, r *http.Request) bool
	queries []string
	tokens  []string
	srv     *httptest.Server
}

func newAuditServer(t *testing.T, events ...testEvent) *auditServer {
	t.Helper()
	a := &auditServer{events: events, perPage: 2}
	a.srv = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *auditServer) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queries = append(a.queries, r.URL.RawQuery)
	a.tokens = append(a.tokens, r.Header.Get("Authorization"))
	if a.handler != nil && a.handler(w, r) {
		return
	}
	offset := 0
	if after := r.URL.Query().Get("after"); after != "" {
		fmt.Sscanf(after, "%d", &offset)
	} else if _, value, ok := strings.Cut(r.URL.Query().Get("phrase"), "created:>="); ok {
		// Honor the qualifier the way GitHub does.
		if floor, err := time.Parse(time.RFC3339, value); err == nil {
			for offset < len(a.events) && a.events[offset].TS < floor.UnixMilli() {
				offset++
			}
		}
	}
	end := offset + a.perPage
	if end >= len(a.events) {
		end = len(a.events)
	} else {
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?after=%d>; rel="next"`, r.Host, r.URL.Path, end))
	}
	page := a.events[offset:end]
	if page == nil {
		page = []testEvent{}
	}
	_ = json.NewEncoder(w).Encode(page)
}

func (a *auditServer) setEvents(events ...testEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = events
}

func (a *auditServer) setHandler(fn func(http.ResponseWriter, *http.Request) bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handler = fn
}

func (a *auditServer) requestCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queries)
}

func testSource(endpoint string) *config.WebAPI {
	hours := 1
	return &config.WebAPI{
		Enabled: true, Name: "github-audit", Endpoint: endpoint, Method: "get",
		AuthType: "apikey", APIKey: "test-token", SleepTime: 1, Timeout: 5,
		Params:    map[string]string{"include": "all", "per_page": "100", "order": "asc"},
		Collector: &config.Collector{Type: config.CollectorTypeGitHubAudit, InitialLookbackHours: &hours},
	}
}

func build(t *testing.T, a *auditServer, dir string, outs map[string]*fakeOutput) *Collector {
	t.Helper()
	source := testSource(a.srv.URL + "/orgs/example/audit-log")
	var routes []output.NamedOutput
	for name, out := range outs {
		routes = append(routes, output.NamedOutput{Name: name, Output: out})
	}
	c, err := newCollector(Options{
		Agent: config.JarvisAgent{ID: "id", Name: "agent"}, Source: source,
		StateDir: dir, Fetcher: api.NewClient(source), Outputs: routes,
	}, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func poll(t *testing.T, c *Collector) {
	t.Helper()
	if err := c.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Wait()
}

func wantIDs(t *testing.T, out *fakeOutput, want ...string) {
	t.Helper()
	if got := out.got(); !reflect.DeepEqual(got, want) && (len(got) != 0 || len(want) != 0) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
}

func TestPaginationOrderAndQuery(t *testing.T) {
	a := newAuditServer(t,
		testEvent{"c", ms(-1000)}, testEvent{"a", ms(-3000)},
		testEvent{"b", ms(-2000)}, testEvent{"d", ms(-500)}, testEvent{"e", ms(-100)})
	out := &fakeOutput{}
	c := build(t, a, t.TempDir(), map[string]*fakeOutput{"o": out})
	poll(t, c)
	wantIDs(t, out, "a", "b", "c", "d", "e")

	if a.requestCount() != 3 {
		t.Fatalf("requests = %d", a.requestCount())
	}
	if !strings.Contains(a.queries[0], "phrase=created%3A%3E%3D2023-11-14T21%3A13%3A20Z") {
		t.Errorf("query = %s", a.queries[0])
	}
	for _, token := range a.tokens {
		if token != "Bearer test-token" {
			t.Errorf("token header = %q", token)
		}
	}
}

func TestUserPhraseIsKept(t *testing.T) {
	c := &Collector{opts: Options{Source: testSource("https://example.test/a")}}
	c.opts.Source.Params["phrase"] = "action:org.add_member"
	u, err := c.firstPageURL(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("phrase"); got != "action:org.add_member created:>=1970-01-01T00:00:00Z" {
		t.Errorf("phrase = %q", got)
	}
}

func TestSameTimestampEventsAcrossPolls(t *testing.T) {
	ts := ms(-1000)
	a := newAuditServer(t, testEvent{"a", ts}, testEvent{"b", ts})
	out := &fakeOutput{}
	c := build(t, a, t.TempDir(), map[string]*fakeOutput{"o": out})
	poll(t, c)
	wantIDs(t, out, "a", "b")

	a.setEvents(testEvent{"a", ts}, testEvent{"b", ts}, testEvent{"c", ts})
	poll(t, c)
	wantIDs(t, out, "a", "b", "c")
}

func TestNoDuplicatesAcrossPollsAndRestarts(t *testing.T) {
	dir := t.TempDir()
	a := newAuditServer(t, testEvent{"a", ms(-3000)}, testEvent{"b", ms(-2000)})
	out := &fakeOutput{}
	c := build(t, a, dir, map[string]*fakeOutput{"o": out})
	poll(t, c)
	poll(t, c)
	wantIDs(t, out, "a", "b")

	a.setEvents(testEvent{"a", ms(-3000)}, testEvent{"b", ms(-2000)}, testEvent{"c", ms(-1000)})
	restarted := build(t, a, dir, map[string]*fakeOutput{"o": out})
	poll(t, restarted)
	wantIDs(t, out, "a", "b", "c")
}

func TestOutageRecoveryWithHealthyOutputAdvancing(t *testing.T) {
	dir := t.TempDir()
	a := newAuditServer(t, testEvent{"a", ms(-3000)}, testEvent{"b", ms(-2000)})
	healthy := &fakeOutput{}
	down := &fakeOutput{failAt: func(string) bool { return true }}
	outs := map[string]*fakeOutput{"healthy": healthy, "down": down}
	c := build(t, a, dir, outs)
	poll(t, c)
	wantIDs(t, healthy, "a", "b")
	wantIDs(t, down)

	a.setEvents(testEvent{"a", ms(-3000)}, testEvent{"b", ms(-2000)}, testEvent{"c", ms(-1000)})
	poll(t, c)
	wantIDs(t, healthy, "a", "b", "c")
	wantIDs(t, down)

	// The backlog survives a restart while the output is still down.
	down.setFail(nil)
	restarted := build(t, a, dir, outs)
	poll(t, restarted)
	wantIDs(t, down, "a", "b", "c")
	wantIDs(t, healthy, "a", "b", "c")
}

func TestBatchStopsAtFirstFailedRecord(t *testing.T) {
	a := newAuditServer(t, testEvent{"a", ms(-3000)}, testEvent{"b", ms(-2000)}, testEvent{"c", ms(-1000)})
	out := &fakeOutput{failAt: func(id string) bool { return id == "b" }}
	c := build(t, a, t.TempDir(), map[string]*fakeOutput{"o": out})
	poll(t, c)
	wantIDs(t, out, "a")
	if cp := c.state["o"]; cp.TimestampMS != ms(-3000) || !reflect.DeepEqual(cp.IDs, []string{"a"}) {
		t.Fatalf("checkpoint = %+v", cp)
	}

	out.setFail(nil)
	poll(t, c)
	wantIDs(t, out, "a", "b", "c")
}

func TestFailedPollForwardsNothing(t *testing.T) {
	cases := map[string]func(http.ResponseWriter, *http.Request) bool{
		"server error": func(w http.ResponseWriter, _ *http.Request) bool {
			w.WriteHeader(http.StatusBadGateway)
			return true
		},
		"auth failure": func(w http.ResponseWriter, _ *http.Request) bool {
			w.WriteHeader(http.StatusForbidden)
			return true
		},
		"not an array": func(w http.ResponseWriter, _ *http.Request) bool {
			fmt.Fprint(w, `{"message":"nope"}`)
			return true
		},
		"null body": func(w http.ResponseWriter, _ *http.Request) bool {
			fmt.Fprint(w, `null`)
			return true
		},
		"missing id": func(w http.ResponseWriter, _ *http.Request) bool {
			fmt.Fprintf(w, `[{"@timestamp":%d}]`, ms(-1))
			return true
		},
		"second page fails": func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Query().Get("after") != "" {
				w.WriteHeader(http.StatusInternalServerError)
				return true
			}
			return false
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			a := newAuditServer(t, testEvent{"a", ms(-3000)}, testEvent{"b", ms(-2000)}, testEvent{"c", ms(-1000)})
			a.setHandler(handler)
			out := &fakeOutput{}
			c := build(t, a, t.TempDir(), map[string]*fakeOutput{"o": out})
			if err := c.Poll(context.Background()); err == nil {
				t.Fatal("expected an error")
			}
			c.Wait()
			wantIDs(t, out)
		})
	}
}

func TestRetryAfterIsHonored(t *testing.T) {
	a := newAuditServer(t, testEvent{"a", ms(-1000)})
	calls := 0
	a.setHandler(func(w http.ResponseWriter, _ *http.Request) bool {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		return false
	})
	out := &fakeOutput{}
	c := build(t, a, t.TempDir(), map[string]*fakeOutput{"o": out})
	var waits []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	poll(t, c)
	wantIDs(t, out, "a")
	if !reflect.DeepEqual(waits, []time.Duration{7 * time.Second}) {
		t.Fatalf("waits = %v", waits)
	}
}

func TestPrimaryRateLimitUsesReset(t *testing.T) {
	page := &api.Page{StatusCode: http.StatusForbidden, Header: http.Header{}}
	page.Header.Set("X-RateLimit-Remaining", "0")
	page.Header.Set("X-RateLimit-Reset", fmt.Sprint(testNow.Unix()+90))
	wait, limited := rateLimitWait(page, testNow)
	if !limited || wait != 90*time.Second {
		t.Fatalf("wait = %v, limited = %v", wait, limited)
	}
}

func TestRateLimitClassification(t *testing.T) {
	plain := &api.Page{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: []byte(`{"message":"Forbidden"}`)}
	if _, limited := rateLimitWait(plain, testNow); limited {
		t.Error("plain 403 treated as rate limit")
	}
	secondary := &api.Page{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: []byte(`{"message":"secondary rate limit"}`)}
	if wait, limited := rateLimitWait(secondary, testNow); !limited || wait != defaultRateLimitWait {
		t.Errorf("secondary: wait = %v, limited = %v", wait, limited)
	}
	huge := &api.Page{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"99999999999999"}}}
	if wait, _ := rateLimitWait(huge, testNow); wait != maxRateLimitWait {
		t.Errorf("huge wait = %v", wait)
	}
}

func TestLongRateLimitDefersNextPoll(t *testing.T) {
	a := newAuditServer(t, testEvent{"a", ms(-1000)})
	a.setHandler(func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	})
	out := &fakeOutput{}
	c := build(t, a, t.TempDir(), map[string]*fakeOutput{"o": out})
	if err := c.Poll(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	before := a.requestCount()
	if err := c.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.requestCount() != before {
		t.Fatal("polled during the rate-limit window")
	}
	wantIDs(t, out)
}

func TestCrossOriginNextLinkIsRefused(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked = true }))
	defer other.Close()

	a := newAuditServer(t, testEvent{"a", ms(-1000)})
	a.setHandler(func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Link", fmt.Sprintf(`<%s/steal>; rel="next"`, other.URL))
		fmt.Fprintf(w, `[{"_document_id":"a","@timestamp":%d}]`, ms(-1000))
		return true
	})
	out := &fakeOutput{}
	c := build(t, a, t.TempDir(), map[string]*fakeOutput{"o": out})
	if err := c.Poll(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	c.Wait()
	if leaked {
		t.Fatal("token request sent to another origin")
	}
	wantIDs(t, out)
}

func TestLaggingOutputDoesNotStarveHealthyOne(t *testing.T) {
	// More events than one capped window holds.
	total := maxPagesPerWindow*2 + 10
	events := make([]testEvent, total)
	for i := range events {
		events[i] = testEvent{fmt.Sprintf("e%05d", i), ms(int64(i-total) * 1000)}
	}
	a := newAuditServer(t, events...)
	a.perPage = 1
	dir := t.TempDir()
	healthy := &fakeOutput{}
	down := &fakeOutput{failAt: func(string) bool { return true }}
	c := build(t, a, dir, map[string]*fakeOutput{"healthy": healthy, "down": down})

	// Place the healthy output past the first window.
	c.state["healthy"] = checkpoint{TimestampMS: events[total-5].TS, IDs: []string{events[total-5].ID}}
	poll(t, c)
	wantIDs(t, healthy, events[total-4].ID, events[total-3].ID, events[total-2].ID, events[total-1].ID)
}

func TestCaughtUpOutputIsServedBeforeFailedBacklogFetch(t *testing.T) {
	a := newAuditServer(t, testEvent{"old", ms(-7200_000)}, testEvent{"new", ms(-1000)})
	healthy := &fakeOutput{}
	lagging := &fakeOutput{}
	c := build(t, a, t.TempDir(), map[string]*fakeOutput{"healthy": healthy, "lagging": lagging})
	c.state["healthy"] = checkpoint{TimestampMS: ms(-2000)}
	c.state["lagging"] = checkpoint{TimestampMS: ms(-7300_000)}

	// The backlog query fails the way an exhausted rate limit would.
	backlogFloor := time.UnixMilli(ms(-7300_000)).UTC().Format("2006-01-02T15:04:05Z")
	a.setHandler(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Query().Get("phrase"), backlogFloor) {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	})

	if err := c.Poll(context.Background()); err == nil {
		t.Fatal("expected the backlog fetch error")
	}
	c.Wait()
	wantIDs(t, healthy, "new")
	wantIDs(t, lagging)
}

func TestFailingOutputBacksOffAndRecovers(t *testing.T) {
	a := newAuditServer(t, testEvent{"a", ms(-1000)})
	down := &fakeOutput{failAt: func(string) bool { return true }}
	c := build(t, a, t.TempDir(), map[string]*fakeOutput{"down": down})
	clock := testNow
	c.now = func() time.Time { return clock }

	poll(t, c) // First failure: retried on the next poll.
	poll(t, c) // Second failure: waits two polling intervals.
	before := a.requestCount()
	poll(t, c)
	if got := a.requestCount(); got != before {
		t.Fatalf("fetched %d pages during backoff", got-before)
	}

	down.setFail(nil)
	clock = clock.Add(2 * time.Second)
	poll(t, c)
	wantIDs(t, down, "a")
	if _, retrying := c.retries["down"]; retrying {
		t.Fatal("backoff not cleared after a successful delivery")
	}
}

func TestBackoffIsCapped(t *testing.T) {
	a := newAuditServer(t)
	c := build(t, a, t.TempDir(), map[string]*fakeOutput{"o": {}})
	for i := 0; i < 80; i++ {
		c.recordOutcome("o", true, false)
	}
	if got := c.retries["o"].notBefore.Sub(testNow); got != maxOutputBackoff {
		t.Fatalf("backoff = %s, want %s", got, maxOutputBackoff)
	}
}

func TestFutureEventIsHeldBack(t *testing.T) {
	dir := t.TempDir()
	a := newAuditServer(t, testEvent{"a", ms(-1000)}, testEvent{"future", ms(5000)})
	out := &fakeOutput{}
	c := build(t, a, dir, map[string]*fakeOutput{"o": out})
	poll(t, c)
	wantIDs(t, out, "a")
	build(t, a, dir, map[string]*fakeOutput{"o": out}) // State stays loadable.
}

func TestNewOutputLeavesOtherCheckpointAlone(t *testing.T) {
	dir := t.TempDir()
	a := newAuditServer(t, testEvent{"a", ms(-1000)})
	first := &fakeOutput{}
	c := build(t, a, dir, map[string]*fakeOutput{"first": first})
	poll(t, c)
	before := c.state["first"]

	second := &fakeOutput{}
	c2 := build(t, a, dir, map[string]*fakeOutput{"first": first, "second": second})
	if !reflect.DeepEqual(c2.state["first"], before) {
		t.Fatalf("checkpoint changed: %+v", c2.state["first"])
	}
	if c2.state["second"].TimestampMS != testNow.Add(-time.Hour).UnixMilli() {
		t.Fatalf("second start = %d", c2.state["second"].TimestampMS)
	}
	poll(t, c2)
	wantIDs(t, first, "a")
	wantIDs(t, second, "a")
}

func TestStateSaveIsAtomicAndLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := statePath(dir, "s")
	want := map[string]checkpoint{"o": {TimestampMS: 5, IDs: []string{"a"}}}
	if err := saveState(path, "s", want); err != nil {
		t.Fatal(err)
	}
	if err := saveState(path, "s", want); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries", len(entries))
	}
	got, err := loadState(path, "s", testNow)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestFailedSaveKeepsPreviousState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	want := map[string]checkpoint{"o": {TimestampMS: 5, IDs: []string{"a"}}}
	if err := saveState(path, "s", want); err != nil {
		t.Fatal(err)
	}
	// A directory in the temp file's place cannot exist, so fail the rename by
	// pointing at a missing parent instead.
	if err := saveState(filepath.Join(dir, "missing", "state.json"), "s", want); err == nil {
		t.Fatal("expected an error")
	}
	if got, err := loadState(path, "s", testNow); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestCorruptStateIsAStartupError(t *testing.T) {
	future := ms(1000)
	cases := map[string]string{
		"empty":        ``,
		"truncated":    `{"version":1,"source":"github-audit","outputs":{`,
		"trailing":     `{"version":1,"source":"github-audit","outputs":{}} x`,
		"wrong type":   `[]`,
		"bad version":  `{"version":2,"source":"github-audit","outputs":{}}`,
		"wrong source": `{"version":1,"source":"other","outputs":{}}`,
		"no outputs":   `{"version":1,"source":"github-audit"}`,
		"no timestamp": `{"version":1,"source":"github-audit","outputs":{"o":{"ids":[]}}}`,
		"negative":     `{"version":1,"source":"github-audit","outputs":{"o":{"timestamp_ms":-1,"ids":[]}}}`,
		"future":       fmt.Sprintf(`{"version":1,"source":"github-audit","outputs":{"o":{"timestamp_ms":%d,"ids":[]}}}`, future),
		"empty id":     `{"version":1,"source":"github-audit","outputs":{"o":{"timestamp_ms":1,"ids":[""]}}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := statePath(dir, "github-audit")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			source := testSource("https://example.test/a")
			_, err := newCollector(Options{Source: source, StateDir: dir}, func() time.Time { return testNow })
			if err == nil || !strings.Contains(err.Error(), "corrupt") {
				t.Fatalf("err = %v", err)
			}
			if data, _ := os.ReadFile(path); string(data) != content {
				t.Fatal("corrupt state file was modified")
			}
		})
	}
}
