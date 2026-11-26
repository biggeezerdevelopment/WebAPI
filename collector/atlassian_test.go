package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"webapi/config"
	"webapi/output"
)

const goldenAtlassianPage = `{"data":[
 {"id":"ev-1","type":"events","attributes":{"time":"2025-02-27T18:50:12.281Z","processedAt":"2025-02-27T18:50:13.004Z","action":"user_login"}},
 {"id":"ev-2","type":"events","attributes":{"time":"2025-02-27T18:50:12.000Z","processedAt":"2025-02-27T18:50:13.004Z","action":"user_logout"}}
],"meta":{"next":"abc","page_size":200},"links":{"self":"x","next":"y"}}`

func TestAtlassianParseGolden(t *testing.T) {
	events, next, err := parseAtlassianPage([]byte(goldenAtlassianPage))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2025, 2, 27, 18, 50, 13, 4_000_000, time.UTC).UnixMilli()
	if next != "abc" || len(events) != 2 || events[0].id != "ev-1" || events[1].id != "ev-2" ||
		events[0].ts != want || events[1].ts != want {
		t.Fatalf("events = %+v, next = %q", events, next)
	}
	var round map[string]any
	if err := json.Unmarshal(events[0].data, &round); err != nil || round["type"] != "events" {
		t.Fatalf("event data not delivered unchanged: %s", events[0].data)
	}
}

func TestAtlassianParseFailures(t *testing.T) {
	cases := map[string]string{
		"missing id":          `{"data":[{"attributes":{"processedAt":"2025-02-27T18:50:13Z"}}]}`,
		"empty id":            `{"data":[{"id":"","attributes":{"processedAt":"2025-02-27T18:50:13Z"}}]}`,
		"missing processedAt": `{"data":[{"id":"a","attributes":{"time":"2025-02-27T18:50:13Z"}}]}`,
		"bad processedAt":     `{"data":[{"id":"a","attributes":{"processedAt":"yesterday"}}]}`,
		"numeric processedAt": `{"data":[{"id":"a","attributes":{"processedAt":1700000000000}}]}`,
		"out of order": `{"data":[{"id":"a","attributes":{"processedAt":"2025-02-27T18:50:14Z"}},
			{"id":"b","attributes":{"processedAt":"2025-02-27T18:50:13Z"}}]}`,
		"array body":   `[]`,
		"null body":    `null`,
		"no data":      `{"meta":{}}`,
		"null data":    `{"data":null}`,
		"object data":  `{"data":{}}`,
		"not json":     `<html>`,
		"event string": `{"data":["x"]}`,
	}
	for name, body := range cases {
		if _, _, err := parseAtlassianPage([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestAtlassianFirstPageQuery(t *testing.T) {
	floor := ms(-5000)
	for _, tc := range []struct {
		params map[string]string
		limit  string
	}{{nil, "500"}, {map[string]string{"limit": "50"}, "50"}} {
		src := &atlassianSource{source: &config.WebAPI{Endpoint: "https://api.example.test/admin/v1/orgs/o/events-stream", Params: tc.params}}
		u, err := src.FirstPage(floor, testNow)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if q.Get("from") != strconv.FormatInt(floor-3_600_000, 10) || q.Get("sortOrder") != "asc" ||
			q.Get("limit") != tc.limit || q.Has("cursor") || q.Has("to") {
			t.Errorf("query = %s", u.RawQuery)
		}
	}
}

// atlassianEvent is one served record; ts is processedAt in ms.
type atlassianEvent struct {
	id string
	ts int64
}

// atlassianServer pages its events by cursor offset and filters from on
// processedAt. It always returns meta.next, as the API may on the last page.
type atlassianServer struct {
	mu      sync.Mutex
	events  []atlassianEvent
	handler func(w http.ResponseWriter, r *http.Request) bool
	queries []map[string]string
	tokens  []string
	srv     *httptest.Server
}

func newAtlassianServer(t *testing.T, events ...atlassianEvent) *atlassianServer {
	t.Helper()
	a := &atlassianServer{events: events}
	a.srv = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *atlassianServer) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	q := r.URL.Query()
	seen := map[string]string{}
	for k := range q {
		seen[k] = q.Get(k)
	}
	a.queries = append(a.queries, seen)
	a.tokens = append(a.tokens, r.Header.Get("Authorization"))
	if a.handler != nil && a.handler(w, r) {
		return
	}
	from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	var matched []atlassianEvent
	for _, ev := range a.events {
		if ev.ts >= from {
			matched = append(matched, ev)
		}
	}
	offset, _ := strconv.Atoi(q.Get("cursor"))
	if offset > len(matched) {
		offset = len(matched)
	}
	end := offset + limit
	if end > len(matched) {
		end = len(matched)
	}
	data := []map[string]any{}
	for _, ev := range matched[offset:end] {
		data = append(data, map[string]any{"id": ev.id, "type": "events", "attributes": map[string]any{
			"processedAt": time.UnixMilli(ev.ts).UTC().Format(time.RFC3339Nano), "action": "test",
		}})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": data,
		"meta": map[string]any{"next": strconv.Itoa(end), "page_size": limit},
	})
}

func (a *atlassianServer) setEvents(events ...atlassianEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = events
}

func (a *atlassianServer) setHandler(fn func(http.ResponseWriter, *http.Request) bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handler = fn
}

func (a *atlassianServer) seen() []map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string]string(nil), a.queries...)
}

// idOutput records the delivered data[].id values.
type idOutput struct {
	mu  sync.Mutex
	ids []string
}

func (o *idOutput) Send(_ context.Context, rec output.Record) error {
	var ev struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Data, &ev); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ids = append(o.ids, ev.ID)
	return nil
}

func (o *idOutput) Close() error { return nil }

func (o *idOutput) got() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.ids...)
}

// fakeAtlassianKey is a placeholder credential sent only to the local test server.
const fakeAtlassianKey = "placeholder-not-a-real-key"

func buildAtlassian(t *testing.T, a *atlassianServer, dir, limit string, out *idOutput) *Collector {
	t.Helper()
	hours := 1
	source := &config.WebAPI{
		Enabled: true, Name: "atlassian-org-audit", Endpoint: a.srv.URL + "/admin/v1/orgs/o/events-stream",
		Method: "get", AuthType: "apikey", APIKey: fakeAtlassianKey, SleepTime: 1, Timeout: 5,
		Collector: &config.Collector{Type: config.CollectorTypeAtlassianOrgAudit, InitialLookbackHours: &hours},
	}
	if limit != "" {
		source.Params = map[string]string{"limit": limit}
	}
	c, err := newCollector(Options{
		Agent: config.JarvisAgent{ID: "id", Name: "agent"}, Source: source, StateDir: dir,
		Fetcher: testClient(t, source), Outputs: []output.NamedOutput{{Name: "o", Output: out}},
	}, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func wantAtlassianIDs(t *testing.T, out *idOutput, want ...string) {
	t.Helper()
	if got := out.got(); !reflect.DeepEqual(got, want) && (len(got) != 0 || len(want) != 0) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
}

func atlassianEvents(n int) []atlassianEvent {
	events := make([]atlassianEvent, n)
	for i := range events {
		events[i] = atlassianEvent{id: fmt.Sprintf("e%d", i), ts: ms(int64(-1000 * (n - i)))}
	}
	return events
}

func ids(events []atlassianEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.id
	}
	return out
}

func TestAtlassianPaginationAndEndDetection(t *testing.T) {
	cases := map[string]struct {
		events, requests int
	}{
		"short last page":           {5, 3}, // 2, 2, 1
		"empty first page":          {0, 1},
		"full last page then empty": {4, 3}, // 2, 2, 0
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			events := atlassianEvents(tc.events)
			a := newAtlassianServer(t, events...)
			out := &idOutput{}
			c := buildAtlassian(t, a, t.TempDir(), "2", out)
			poll(t, c)
			wantAtlassianIDs(t, out, ids(events)...)
			seen := a.seen()
			if len(seen) != tc.requests {
				t.Fatalf("requests = %d, want %d: %v", len(seen), tc.requests, seen)
			}
			for i, q := range seen {
				if q["limit"] != "2" || q["sortOrder"] != "asc" || q["from"] != seen[0]["from"] {
					t.Errorf("request %d query = %v", i, q)
				}
				if wantCursor := strconv.Itoa(2 * i); i > 0 && q["cursor"] != wantCursor {
					t.Errorf("request %d cursor = %q, want %q", i, q["cursor"], wantCursor)
				}
			}
			if _, ok := seen[0]["cursor"]; ok {
				t.Error("first request carries a cursor")
			}
			a.mu.Lock()
			token := a.tokens[0]
			a.mu.Unlock()
			if token != "Bearer "+fakeAtlassianKey {
				t.Errorf("authorization = %q", token)
			}
		})
	}
}

func TestAtlassianOutOfOrderPageFails(t *testing.T) {
	a := newAtlassianServer(t)
	a.setHandler(func(w http.ResponseWriter, _ *http.Request) bool {
		fmt.Fprintf(w, `{"data":[{"id":"a","attributes":{"processedAt":%q}},{"id":"b","attributes":{"processedAt":%q}}]}`,
			time.UnixMilli(ms(-1000)).UTC().Format(time.RFC3339Nano), time.UnixMilli(ms(-2000)).UTC().Format(time.RFC3339Nano))
		return true
	})
	out := &idOutput{}
	c := buildAtlassian(t, a, t.TempDir(), "", out)
	if err := c.Poll(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	c.Wait()
	wantAtlassianIDs(t, out)
}

func TestAtlassianEndToEndWithRestart(t *testing.T) {
	dir := t.TempDir()
	events := atlassianEvents(3)
	a := newAtlassianServer(t, events...)
	out := &idOutput{}
	c := buildAtlassian(t, a, dir, "", out)
	poll(t, c)
	poll(t, c)
	wantAtlassianIDs(t, out, "e0", "e1", "e2")
	if seen := a.seen(); seen[0]["limit"] != "500" {
		t.Errorf("default limit = %q", seen[0]["limit"])
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no checkpoint saved: %v", err)
	}
	if cp := c.state["o"]; cp.TimestampMS != events[2].ts {
		t.Fatalf("checkpoint = %+v", cp)
	}

	a.setEvents(append(events, atlassianEvent{"e3", ms(-500)})...)
	restarted := buildAtlassian(t, a, dir, "", out)
	poll(t, restarted)
	wantAtlassianIDs(t, out, "e0", "e1", "e2", "e3")
}

func TestAtlassianRetryAfterIsHonored(t *testing.T) {
	a := newAtlassianServer(t, atlassianEvents(1)...)
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
	out := &idOutput{}
	c := buildAtlassian(t, a, t.TempDir(), "", out)
	var waits []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	poll(t, c)
	wantAtlassianIDs(t, out, "e0")
	if !reflect.DeepEqual(waits, []time.Duration{7 * time.Second}) {
		t.Fatalf("waits = %v", waits)
	}
}
