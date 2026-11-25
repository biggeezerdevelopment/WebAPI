package collector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"webapi/api"
	"webapi/config"
	"webapi/output"
)

const goldenJiraPage = `{"offset":0,"limit":1000,"total":2,"records":[
 {"id":2,"summary":"User deleted","created":"2014-03-19T18:45:43.000+0000","category":"user management"},
 {"id":1,"summary":"User created","created":"2014-03-19T18:45:42.967+0000","category":"user management"}]}`

func TestJiraParseGolden(t *testing.T) {
	page, err := parseJiraPage([]byte(goldenJiraPage))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2014, 3, 19, 18, 45, 42, 967_000_000, time.UTC).UnixMilli()
	if page.total != 2 || *page.offset != 0 || len(page.records) != 2 || page.records[1].id != "1" || page.records[1].ts != want {
		t.Fatalf("page = %+v", page)
	}
	var round map[string]any
	if err := json.Unmarshal(page.records[1].data, &round); err != nil || round["summary"] != "User created" {
		t.Fatalf("record not delivered unchanged: %s", page.records[1].data)
	}
	if ts, ok := parseJiraTime("2014-03-19T18:45:42.967Z"); !ok || ts != want {
		t.Errorf("RFC 3339 created not accepted")
	}
	if ts, ok := parseJiraTime("2014-03-19T20:45:42.967+0200"); !ok || ts != want {
		t.Errorf("non-UTC offset not accepted")
	}
}

func TestJiraParseFailures(t *testing.T) {
	rec := func(fields string) string { return `{"total":1,"records":[{` + fields + `}]}` }
	cases := map[string]string{
		"missing id":      rec(`"created":"2014-03-19T18:45:42.967+0000"`),
		"string id":       rec(`"id":"1","created":"2014-03-19T18:45:42.967+0000"`),
		"float id":        rec(`"id":1.5,"created":"2014-03-19T18:45:42.967+0000"`),
		"null id":         rec(`"id":null,"created":"2014-03-19T18:45:42.967+0000"`),
		"huge id":         rec(`"id":99999999999999999999,"created":"2014-03-19T18:45:42.967+0000"`),
		"missing created": rec(`"id":1`),
		"bad created":     rec(`"id":1,"created":"yesterday"`),
		"numeric created": rec(`"id":1,"created":1700000000000`),
		"array body":      `[]`,
		"null body":       `null`,
		"no total":        `{"records":[]}`,
		"no records":      `{"total":0}`,
		"null records":    `{"total":0,"records":null}`,
		"object records":  `{"total":0,"records":{}}`,
		"not json":        `<html>`,
		"record string":   `{"total":1,"records":["x"]}`,
	}
	for name, body := range cases {
		if _, err := parseJiraPage([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestJiraFirstPageRange(t *testing.T) {
	src := &jiraSource{source: &config.WebAPI{Endpoint: "https://example.atlassian.net/rest/api/3/auditing/record",
		Params: map[string]string{"filter": "user management"}}}
	now := time.Date(2025, 6, 1, 12, 0, 30, 0, time.FixedZone("x", 2*3600))
	floor := time.Date(2025, 6, 1, 9, 15, 7, 891_000_000, time.UTC).UnixMilli()
	u, err := src.FirstPage(floor, now)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("from") != "2025-06-01T09:15:07.000+0000" || q.Get("to") != "2025-06-01T09:59:30.000+0000" ||
		q.Get("offset") != "0" || q.Get("limit") != "1" || q.Get("filter") != "user management" {
		t.Errorf("query = %s", u.RawQuery)
	}
	if !strings.Contains(u.RawQuery, "%2B0000") {
		t.Errorf("offset sign is not escaped: %s", u.RawQuery)
	}
}

// jiraRecord is one served record; ts is created in ms.
type jiraRecord struct {
	id int64
	ts int64
}

// jiraServer serves records newest first, filtered by from/to, paged by
// offset and limit.
type jiraServer struct {
	mu      sync.Mutex
	records []jiraRecord
	handler func(w http.ResponseWriter, r *http.Request) bool
	queries []map[string]string
	auth    []string
	srv     *httptest.Server
}

func newJiraServer(t *testing.T, records ...jiraRecord) *jiraServer {
	t.Helper()
	j := &jiraServer{records: records}
	j.srv = httptest.NewServer(http.HandlerFunc(j.serve))
	t.Cleanup(j.srv.Close)
	return j
}

func jiraRecordJSON(r jiraRecord) map[string]any {
	return map[string]any{"id": r.id, "summary": "test", "category": "user management",
		"created": time.UnixMilli(r.ts).UTC().Format(jiraTimeLayout)}
}

func (j *jiraServer) serve(w http.ResponseWriter, r *http.Request) {
	j.mu.Lock()
	defer j.mu.Unlock()
	q := r.URL.Query()
	seen := map[string]string{}
	for k := range q {
		seen[k] = q.Get(k)
	}
	j.queries = append(j.queries, seen)
	j.auth = append(j.auth, r.Header.Get("Authorization"))
	if j.handler != nil && j.handler(w, r) {
		return
	}
	from, err1 := time.Parse(jiraTimeLayout, q.Get("from"))
	to, err2 := time.Parse(jiraTimeLayout, q.Get("to"))
	if err1 != nil || err2 != nil {
		http.Error(w, "bad range", http.StatusBadRequest)
		return
	}
	var matched []jiraRecord
	for _, rec := range j.records {
		if rec.ts >= from.UnixMilli() && rec.ts <= to.UnixMilli() {
			matched = append(matched, rec)
		}
	}
	sort.SliceStable(matched, func(a, b int) bool { return matched[a].ts > matched[b].ts })
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset = min(offset, len(matched))
	end := min(offset+limit, len(matched))
	records := []map[string]any{}
	for _, rec := range matched[offset:end] {
		records = append(records, jiraRecordJSON(rec))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"offset": offset, "limit": limit, "total": len(matched), "records": records})
}

func (j *jiraServer) setRecords(records ...jiraRecord) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.records = records
}

func (j *jiraServer) setHandler(fn func(http.ResponseWriter, *http.Request) bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.handler = fn
}

func (j *jiraServer) seen() []map[string]string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]map[string]string(nil), j.queries...)
}

// numOutput records the delivered records[].id values.
type numOutput struct {
	mu  sync.Mutex
	ids []int64
}

func (o *numOutput) Send(_ context.Context, rec output.Record) error {
	var ev struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Data, &ev); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ids = append(o.ids, ev.ID)
	return nil
}

func (o *numOutput) Close() error { return nil }

func (o *numOutput) got() []int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]int64(nil), o.ids...)
}

// fakeJiraToken is a placeholder credential sent only to the local test server.
const fakeJiraToken = "placeholder-not-a-real-token"

func buildJira(t *testing.T, j *jiraServer, dir, limit string, out *numOutput) *Collector {
	t.Helper()
	t.Setenv("TEST_JIRA_API_TOKEN", fakeJiraToken)
	hours := 1
	source := &config.WebAPI{
		Enabled: true, Name: "jira-audit", Endpoint: j.srv.URL + "/rest/api/3/auditing/record",
		Method: "get", AuthType: "basic", APIUsername: "svc-audit@example.com", APIPasswordEnv: "TEST_JIRA_API_TOKEN",
		SleepTime: 1, Timeout: 5,
		Collector: &config.Collector{Type: config.CollectorTypeJiraAudit, InitialLookbackHours: &hours},
	}
	if limit != "" {
		source.Params = map[string]string{"limit": limit}
	}
	c, err := newCollector(Options{
		Agent: config.JarvisAgent{ID: "id", Name: "agent"}, Source: source, StateDir: dir,
		Fetcher: api.NewClient(source), Outputs: []output.NamedOutput{{Name: "o", Output: out}},
	}, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

// jiraRecords returns n records, oldest first, ending before the settle delay.
func jiraRecords(n int) []jiraRecord {
	records := make([]jiraRecord, n)
	for i := range records {
		records[i] = jiraRecord{id: int64(100 + i), ts: ms(int64(-60_000 - 1000*(n-i)))}
	}
	return records
}

func jiraIDs(records []jiraRecord) []int64 {
	out := make([]int64, len(records))
	for i, rec := range records {
		out[i] = rec.id
	}
	return out
}

func wantJiraIDs(t *testing.T, out *numOutput, want ...int64) {
	t.Helper()
	if got := out.got(); !reflect.DeepEqual(got, want) && (len(got) != 0 || len(want) != 0) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
}

func TestJiraProbeAndBackwardPaging(t *testing.T) {
	cases := map[string]struct {
		total int
		limit string
		pages []string // offset/limit of each request after the probe
	}{
		"total 0":       {0, "3", nil},
		"total 1":       {1, "3", nil}, // the probe holds the only record
		"total limit":   {3, "3", []string{"0/3"}},
		"total limit+1": {4, "3", []string{"1/3", "0/3"}},
		"several pages": {8, "3", []string{"5/3", "2/3", "0/3"}},
		"limit 1":       {3, "1", []string{"2/1", "1/1"}}, // offset 0 is the probe
		"default limit": {2, "", []string{"0/1000"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			records := jiraRecords(tc.total)
			j := newJiraServer(t, records...)
			out := &numOutput{}
			c := buildJira(t, j, t.TempDir(), tc.limit, out)
			poll(t, c)
			wantJiraIDs(t, out, jiraIDs(records)...)
			seen := j.seen()
			if len(seen) != 1+len(tc.pages) {
				t.Fatalf("requests = %v, want probe then %v", seen, tc.pages)
			}
			if seen[0]["offset"] != "0" || seen[0]["limit"] != "1" {
				t.Errorf("probe query = %v", seen[0])
			}
			for i, want := range tc.pages {
				q := seen[i+1]
				if got := q["offset"] + "/" + q["limit"]; got != want {
					t.Errorf("request %d = %s, want %s", i+1, got, want)
				}
			}
			for i, q := range seen {
				if q["from"] != seen[0]["from"] || q["to"] != seen[0]["to"] {
					t.Errorf("request %d range changed: %v", i, q)
				}
			}
			wantTo := testNow.Add(-60 * time.Second).UTC().Format(jiraTimeLayout)
			if seen[0]["to"] != wantTo {
				t.Errorf("to = %q, want %q", seen[0]["to"], wantTo)
			}
			wantFrom := testNow.Add(-time.Hour).UTC().Truncate(time.Second).Format(jiraTimeLayout)
			if seen[0]["from"] != wantFrom {
				t.Errorf("from = %q, want %q", seen[0]["from"], wantFrom)
			}
		})
	}
}

func TestJiraSettleDelayExcludesRecentRecords(t *testing.T) {
	records := []jiraRecord{{1, ms(-61_000)}, {2, ms(-30_000)}}
	j := newJiraServer(t, records...)
	out := &numOutput{}
	c := buildJira(t, j, t.TempDir(), "", out)
	poll(t, c)
	wantJiraIDs(t, out, 1)
}

func TestJiraBasicAuthFromPasswordEnv(t *testing.T) {
	j := newJiraServer(t, jiraRecords(1)...)
	out := &numOutput{}
	c := buildJira(t, j, t.TempDir(), "", out)
	poll(t, c)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("svc-audit@example.com:"+fakeJiraToken))
	j.mu.Lock()
	defer j.mu.Unlock()
	for i, got := range j.auth {
		if got != want {
			t.Errorf("request %d authorization = %q", i, got)
		}
	}
}

// failingJira serves a fixed body for every data page after a valid probe.
func failingJira(t *testing.T, total int, body func(q map[string]string) string) error {
	t.Helper()
	j := newJiraServer(t)
	j.setHandler(func(w http.ResponseWriter, r *http.Request) bool {
		q := r.URL.Query()
		if q.Get("limit") == "1" {
			fmt.Fprintf(w, `{"offset":0,"limit":1,"total":%d,"records":[{"id":9,"created":%q}]}`,
				total, time.UnixMilli(ms(-61_000)).UTC().Format(jiraTimeLayout))
			return true
		}
		fmt.Fprint(w, body(map[string]string{"offset": q.Get("offset")}))
		return true
	})
	out := &numOutput{}
	c := buildJira(t, j, t.TempDir(), "2", out)
	err := c.Poll(context.Background())
	c.Wait()
	wantJiraIDs(t, out)
	return err
}

func jiraBody(total int, recs ...string) string {
	return fmt.Sprintf(`{"total":%d,"records":[%s]}`, total, strings.Join(recs, ","))
}

func jiraRec(id string, created int64) string {
	return fmt.Sprintf(`{"id":%s,"created":%q}`, id, time.UnixMilli(created).UTC().Format(jiraTimeLayout))
}

func TestJiraConsistencyFailures(t *testing.T) {
	cases := map[string]struct {
		total int
		body  func(q map[string]string) string
		want  string
	}{
		"ascending page": {2, func(map[string]string) string {
			return jiraBody(2, jiraRec("1", ms(-70_000)), jiraRec("2", ms(-65_000)))
		}, "order is not as expected"},
		"ascending across pages": {4, func(q map[string]string) string {
			if q["offset"] == "2" {
				return jiraBody(4, jiraRec("2", ms(-65_000)), jiraRec("1", ms(-66_000)))
			}
			return jiraBody(4, jiraRec("4", ms(-80_000)), jiraRec("3", ms(-81_000)))
		}, "order is not as expected"},
		"total changes": {2, func(map[string]string) string {
			return jiraBody(3, jiraRec("2", ms(-65_000)), jiraRec("1", ms(-66_000)))
		}, "total changed"},
		"short page": {2, func(map[string]string) string {
			return jiraBody(2, jiraRec("2", ms(-65_000)))
		}, "records, want"},
		"missing id": {1 + 1, func(map[string]string) string {
			return jiraBody(2, `{"created":"2025-01-01T00:00:00.000+0000"}`, jiraRec("1", ms(-66_000)))
		}, "id"},
		"bad id": {2, func(map[string]string) string {
			return jiraBody(2, jiraRec(`"x"`, ms(-65_000)), jiraRec("1", ms(-66_000)))
		}, "id"},
		"missing created": {2, func(map[string]string) string {
			return jiraBody(2, `{"id":2}`, jiraRec("1", ms(-66_000)))
		}, "created"},
		"bad created": {2, func(map[string]string) string {
			return jiraBody(2, `{"id":2,"created":"soon"}`, jiraRec("1", ms(-66_000)))
		}, "created"},
		"records not array": {2, func(map[string]string) string { return `{"total":2,"records":"x"}` }, "array"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := failingJira(t, tc.total, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestJiraEndToEndWithRestart(t *testing.T) {
	dir := t.TempDir()
	records := jiraRecords(5)
	j := newJiraServer(t, records...)
	out := &numOutput{}
	c := buildJira(t, j, dir, "2", out)
	poll(t, c)
	poll(t, c)
	wantJiraIDs(t, out, jiraIDs(records)...)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no checkpoint saved: %v", err)
	}
	if cp := c.state["o"]; cp.TimestampMS != records[4].ts {
		t.Fatalf("checkpoint = %+v", cp)
	}

	j.setRecords(append(records, jiraRecord{200, ms(-60_500)})...)
	restarted := buildJira(t, j, dir, "2", out)
	poll(t, restarted)
	wantJiraIDs(t, out, append(jiraIDs(records), 200)...)
}
