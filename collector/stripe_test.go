package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"webapi/api"
	"webapi/config"
	"webapi/output"
)

// goldenStripePage follows the observed response shape. Values are fake.
// The second record steps back one second, as Stripe's list can.
const goldenStripePage = `{"data":[
 {"id":"accact_test_0001","object":"v2.iam.activity_log","created":"2026-10-20T20:37:32.000Z","type":"user_access_started","livemode":false,"context":"acct_test_000000000001","actor":{"type":"user","user":{"email":"admin@example.com"}},"details":{"type":"user_access","user_access":{"network":{"ip_address":"192.0.2.10"}}},"related_object":null,"request":null},
 {"id":"accact_test_0002","object":"v2.iam.activity_log","created":"2026-10-20T20:37:31.000Z","type":"api_key_created","livemode":false,"context":"acct_test_000000000001","actor":{"type":"user","user":{"email":"admin@example.com"}},"details":{"type":"api_key","api_key":{"id":"mk_test_0001","new_key":null}},"related_object":null,"request":{"id":"req_test_0001"}}
],"next_page_url":"/v2/iam/activity_logs?page=page_test_2&limit=2","previous_page_url":null}`

func TestStripeParseGolden(t *testing.T) {
	records, next, err := parseStripePage([]byte(goldenStripePage))
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 10, 20, 20, 37, 32, 0, time.UTC).UnixMilli()
	if next != "/v2/iam/activity_logs?page=page_test_2&limit=2" || len(records) != 2 ||
		records[0].id != "accact_test_0001" || records[0].created != first ||
		records[1].id != "accact_test_0002" || records[1].created != first-1000 {
		t.Fatalf("records = %+v, next = %q", records, next)
	}
	var round map[string]any
	if err := json.Unmarshal(records[1].data, &round); err != nil || round["object"] != "v2.iam.activity_log" {
		t.Fatalf("record data not kept unchanged: %s", records[1].data)
	}
}

func TestStripeParseFailures(t *testing.T) {
	cases := map[string]string{
		"missing id":        `{"data":[{"created":"2026-10-20T20:37:32Z"}]}`,
		"empty id":          `{"data":[{"id":"","created":"2026-10-20T20:37:32Z"}]}`,
		"numeric id":        `{"data":[{"id":7,"created":"2026-10-20T20:37:32Z"}]}`,
		"missing created":   `{"data":[{"id":"a"}]}`,
		"bad created":       `{"data":[{"id":"a","created":"yesterday"}]}`,
		"numeric created":   `{"data":[{"id":"a","created":1700000000}]}`,
		"pre-epoch created": `{"data":[{"id":"a","created":"1969-12-31T23:59:59Z"}]}`,
		"array body":        `[]`,
		"null body":         `null`,
		"no data":           `{"next_page_url":null}`,
		"null data":         `{"data":null}`,
		"object data":       `{"data":{}}`,
		"not json":          `<html>`,
		"record string":     `{"data":["x"]}`,
	}
	for name, body := range cases {
		if _, _, err := parseStripePage([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func newStripeSource(params map[string]string, settle *int) *stripeSource {
	return &stripeSource{source: &config.WebAPI{
		Endpoint: "https://api.example.test/v2/iam/activity_logs", Params: params,
		Collector: &config.Collector{Type: config.CollectorTypeStripeActivityLogs, SettleMinutes: settle},
	}}
}

func TestStripeFirstPage(t *testing.T) {
	for _, tc := range []struct {
		params map[string]string
		want   url.Values
	}{
		{nil, url.Values{"limit": {"100"}}},
		{map[string]string{"limit": "50"}, url.Values{"limit": {"50"}}},
		{map[string]string{"actions[0]": "api_key_created", "actions[1]": "api_key_deleted"},
			url.Values{"limit": {"100"}, "actions[0]": {"api_key_created"}, "actions[1]": {"api_key_deleted"}}},
	} {
		u, err := newStripeSource(tc.params, nil).FirstPage(ms(-5000), testNow)
		if err != nil {
			t.Fatal(err)
		}
		if got := u.Query(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("query = %v, want %v", got, tc.want)
		}
		if strings.Contains(u.RawQuery, "[") {
			t.Errorf("brackets are not percent-encoded: %s", u.RawQuery)
		}
	}

	ten := 10
	for settle, want := range map[*int]time.Duration{nil: 30 * time.Minute, &ten: 10 * time.Minute} {
		src := newStripeSource(nil, settle)
		if _, err := src.FirstPage(0, testNow); err != nil {
			t.Fatal(err)
		}
		if src.cutoff != testNow.Add(-want).UnixMilli() {
			t.Errorf("cutoff = %d, want now - %s", src.cutoff, want)
		}
	}
	if _, err := newStripeSource(map[string]string{"limit": "101"}, nil).FirstPage(0, testNow); err == nil {
		t.Error("limit 101 accepted")
	}
}

func TestStripeParsePageClampsAndResolves(t *testing.T) {
	src := newStripeSource(map[string]string{"limit": "2"}, nil)
	cur, err := src.FirstPage(0, time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	events, next, err := src.ParsePage(cur, &api.Page{StatusCode: 200, Body: []byte(goldenStripePage)})
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 10, 20, 20, 37, 32, 0, time.UTC).UnixMilli()
	if len(events) != 2 || events[0].ts != first || events[1].ts != first {
		t.Fatalf("events = %+v, want both at the running maximum %d", events, first)
	}
	if next == nil || next.String() != "https://api.example.test/v2/iam/activity_logs?page=page_test_2&limit=2" {
		t.Fatalf("next = %v", next)
	}
}

// stripeRec is one served record. created is in ms.
type stripeRec struct {
	id      string
	created int64
}

// stripeServer serves its records in slice order, paged by an offset token.
// Like Stripe, every page has a relative next_page_url that keeps the
// request's filters, including the last page.
type stripeServer struct {
	mu      sync.Mutex
	records []stripeRec
	handler func(w http.ResponseWriter, r *http.Request) bool
	queries []url.Values
	headers []http.Header
	srv     *httptest.Server
}

func newStripeServer(t *testing.T, records ...stripeRec) *stripeServer {
	t.Helper()
	s := &stripeServer{records: records}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stripeServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := r.URL.Query()
	s.queries = append(s.queries, q)
	s.headers = append(s.headers, r.Header.Clone())
	if s.handler != nil && s.handler(w, r) {
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset := 0
	if p := q.Get("page"); p != "" {
		offset, _ = strconv.Atoi(strings.TrimPrefix(p, "page_"))
	}
	offset = min(offset, len(s.records))
	end := min(offset+limit, len(s.records))
	data := []map[string]any{}
	for _, rec := range s.records[offset:end] {
		data = append(data, stripeRecordJSON(rec))
	}
	next := url.Values{}
	for k, v := range q {
		next[k] = v
	}
	next.Set("page", "page_"+strconv.Itoa(end))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": data, "next_page_url": r.URL.Path + "?" + next.Encode(), "previous_page_url": nil,
	})
}

func stripeRecordJSON(rec stripeRec) map[string]any {
	return map[string]any{
		"id": rec.id, "object": "v2.iam.activity_log",
		"created": time.UnixMilli(rec.created).UTC().Format("2006-01-02T15:04:05.000Z"),
		"type":    "api_key_created", "livemode": false, "context": "acct_test_000000000001",
		"actor":          map[string]any{"type": "user", "user": map[string]any{"email": "admin@example.com"}},
		"details":        map[string]any{"type": "api_key", "api_key": map[string]any{"id": "mk_test_0001"}},
		"related_object": nil, "request": nil,
	}
}

func (s *stripeServer) setRecords(records ...stripeRec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = records
}

func (s *stripeServer) setHandler(fn func(http.ResponseWriter, *http.Request) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handler = fn
}

func (s *stripeServer) seen() ([]url.Values, []http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.queries...), append([]http.Header(nil), s.headers...)
}

// fakeStripeKey is a placeholder credential sent only to the local test server.
const fakeStripeKey = "placeholder-not-a-real-key"

func buildStripe(t *testing.T, s *stripeServer, dir string, params map[string]string, out *idOutput, now func() time.Time) *Collector {
	t.Helper()
	hours := 2
	source := &config.WebAPI{
		Enabled: true, Name: "stripe-activity", Endpoint: s.srv.URL + "/v2/iam/activity_logs",
		Method: "get", AuthType: "apikey", APIKey: fakeStripeKey, SleepTime: 1, Timeout: 5,
		Headers:   []config.Header{{Key: "Stripe-Version", Value: "2026-11-18.preview"}},
		Params:    params,
		Collector: &config.Collector{Type: config.CollectorTypeStripeActivityLogs, InitialLookbackHours: &hours},
	}
	if now == nil {
		now = func() time.Time { return testNow }
	}
	c, err := newCollector(Options{
		Agent: config.JarvisAgent{ID: "id", Name: "agent"}, Source: source, StateDir: dir,
		Fetcher: testClient(t, source), Outputs: []output.NamedOutput{{Name: "o", Output: out}},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

// stripeRecs returns n records one second apart, starting an hour ago:
// newer than the 2 h lookback and older than the 30 min settle cutoff.
func stripeRecs(n int) []stripeRec {
	recs := make([]stripeRec, n)
	for i := range recs {
		recs[i] = stripeRec{id: fmt.Sprintf("r%03d", i), created: ms(-3_600_000 + int64(i)*1000)}
	}
	return recs
}

func stripeIDs(recs []stripeRec) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.id
	}
	return out
}

func TestStripePaginationAndEndDetection(t *testing.T) {
	cases := map[string]struct{ records, requests int }{
		"short last page":           {5, 3}, // 2, 2, 1
		"empty first page":          {0, 1},
		"full last page then empty": {4, 3}, // 2, 2, 0
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			recs := stripeRecs(tc.records)
			s := newStripeServer(t, recs...)
			out := &idOutput{}
			c := buildStripe(t, s, t.TempDir(), map[string]string{"limit": "2", "action_groups[0]": "api_key"}, out, nil)
			poll(t, c)
			wantAtlassianIDs(t, out, stripeIDs(recs)...)
			queries, headers := s.seen()
			if len(queries) != tc.requests {
				t.Fatalf("requests = %d, want %d: %v", len(queries), tc.requests, queries)
			}
			for i, q := range queries {
				if q.Get("limit") != "2" || q.Get("action_groups[0]") != "api_key" {
					t.Errorf("request %d query = %v", i, q)
				}
				if i == 0 && q.Has("page") {
					t.Error("first request carries a page token")
				}
				if want := "page_" + strconv.Itoa(2*i); i > 0 && q.Get("page") != want {
					t.Errorf("request %d page = %q, want %q", i, q.Get("page"), want)
				}
			}
			if h := headers[0]; h.Get("Authorization") != "Bearer "+fakeStripeKey || h.Get("Stripe-Version") != "2026-11-18.preview" {
				t.Errorf("headers = %v", h)
			}
		})
	}
}

func TestStripeClampsCreatedStepBack(t *testing.T) {
	base := ms(-3_600_000)
	s := newStripeServer(t, stripeRec{"r1", base}, stripeRec{"r2", base + 2000}, stripeRec{"r3", base + 1000}, stripeRec{"r4", base + 3000})
	out := &idOutput{}
	c := buildStripe(t, s, t.TempDir(), nil, out, nil)
	poll(t, c)
	wantAtlassianIDs(t, out, "r1", "r2", "r3", "r4")
	if cp := c.state["o"]; cp.TimestampMS != base+3000 || !reflect.DeepEqual(cp.IDs, []string{"r4"}) {
		t.Fatalf("checkpoint = %+v", cp)
	}
}

// A record that appears after the checkpoint in list order, but with created
// a second before it, is still delivered. With raw created as ts it would sit
// behind the checkpoint and be dropped.
func TestStripeLateRecordBehindCheckpointIsDelivered(t *testing.T) {
	base := ms(-3_600_000)
	s := newStripeServer(t, stripeRec{"r1", base}, stripeRec{"r2", base + 5000})
	out := &idOutput{}
	c := buildStripe(t, s, t.TempDir(), nil, out, nil)
	poll(t, c)
	wantAtlassianIDs(t, out, "r1", "r2")
	s.setRecords(stripeRec{"r1", base}, stripeRec{"r2", base + 5000}, stripeRec{"r3", base + 4000})
	poll(t, c)
	wantAtlassianIDs(t, out, "r1", "r2", "r3")
}

func TestStripeSettleCutoffHoldsBackNewRecords(t *testing.T) {
	clock := testNow
	now := func() time.Time { return clock }
	// IDs ascend in list order, as Stripe's do. r3 is listed after r2, so it
	// waits for r2 even though it is older, and shares r2's clamped ts.
	s := newStripeServer(t,
		stripeRec{"r1", ms(-3_600_000)},
		stripeRec{"r2", ms(-10 * 60_000)},
		stripeRec{"r3", ms(-3_500_000)},
	)
	out := &idOutput{}
	c := buildStripe(t, s, t.TempDir(), nil, out, now)
	poll(t, c)
	wantAtlassianIDs(t, out, "r1")
	if cp := c.state["o"]; cp.TimestampMS != ms(-3_600_000) {
		t.Fatalf("checkpoint passed the settle cutoff: %+v", cp)
	}
	clock = clock.Add(30 * time.Minute)
	poll(t, c)
	wantAtlassianIDs(t, out, "r1", "r2", "r3")
}

func TestStripeRefusesCrossOriginNextPage(t *testing.T) {
	s := newStripeServer(t)
	record, err := json.Marshal(stripeRecordJSON(stripeRec{"r1", ms(-3_600_000)}))
	if err != nil {
		t.Fatal(err)
	}
	s.setHandler(func(w http.ResponseWriter, _ *http.Request) bool {
		fmt.Fprintf(w, `{"data":[%s],"next_page_url":"https://other.example.test/v2/iam/activity_logs?page=x"}`, record)
		return true
	})
	out := &idOutput{}
	c := buildStripe(t, s, t.TempDir(), map[string]string{"limit": "1"}, out, nil)
	if err := c.Poll(context.Background()); err == nil || !strings.Contains(err.Error(), "different origin") {
		t.Fatalf("err = %v, want a different-origin refusal", err)
	}
	c.Wait()
	wantAtlassianIDs(t, out)
}

func TestStripeTruncatedWindowResumes(t *testing.T) {
	recs := stripeRecs(150)
	s := newStripeServer(t, recs...)
	out := &idOutput{}
	c := buildStripe(t, s, t.TempDir(), map[string]string{"limit": "1"}, out, nil)
	poll(t, c)
	if got := len(out.got()); got != maxPagesPerWindow {
		t.Fatalf("first poll delivered %d, want %d", got, maxPagesPerWindow)
	}
	poll(t, c)
	wantAtlassianIDs(t, out, stripeIDs(recs)...)
}

func TestStripeMalformedRecordFailsPoll(t *testing.T) {
	s := newStripeServer(t)
	s.setHandler(func(w http.ResponseWriter, _ *http.Request) bool {
		fmt.Fprint(w, `{"data":[{"id":"r1"}],"next_page_url":null}`)
		return true
	})
	out := &idOutput{}
	c := buildStripe(t, s, t.TempDir(), nil, out, nil)
	if err := c.Poll(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	c.Wait()
	wantAtlassianIDs(t, out)
}
