package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"webapi/api"
	"webapi/config"
	"webapi/output"
)

// cursorSource is a non-GitHub shape: the floor and cursor travel as "since"
// and "cursor" query values, and the body is {"items":[...],"next":"..."}.
type cursorSource struct {
	endpoint string
	nextBase string // when set, next links point here instead of the endpoint
}

type cursorRecord struct {
	Key  string `json:"key"`
	When int64  `json:"when"`
}

func (s *cursorSource) FirstPage(floorMS int64, _ time.Time) (*url.URL, error) {
	u, err := url.Parse(s.endpoint)
	if err != nil {
		return nil, err
	}
	u.RawQuery = url.Values{"since": {strconv.FormatInt(floorMS, 10)}}.Encode()
	return u, nil
}

func (s *cursorSource) ParsePage(cur *url.URL, page *api.Page) ([]event, *url.URL, error) {
	var body struct {
		Items []json.RawMessage `json:"items"`
		Next  string            `json:"next"`
	}
	if err := json.Unmarshal(page.Body, &body); err != nil {
		return nil, nil, err
	}
	var events []event
	for _, item := range body.Items {
		var rec cursorRecord
		if err := json.Unmarshal(item, &rec); err != nil {
			return nil, nil, err
		}
		events = append(events, event{id: rec.Key, ts: rec.When, data: item})
	}
	if body.Next == "" {
		return events, nil, nil
	}
	base := cur
	if s.nextBase != "" {
		b, err := url.Parse(s.nextBase)
		if err != nil {
			return nil, nil, err
		}
		base = b
	}
	next := *base
	next.RawQuery = url.Values{"cursor": {body.Next}}.Encode()
	return events, &next, nil
}

// keyOutput records the "key" field of each delivered record.
type keyOutput struct{ fakeOutput }

func (k *keyOutput) Send(_ context.Context, rec output.Record) error {
	var r cursorRecord
	if err := json.Unmarshal(rec.Data, &r); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.ids = append(k.ids, r.Key)
	return nil
}

func cursorServer(t *testing.T, records []cursorRecord) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			offset, _ = strconv.Atoi(c)
		} else if since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil {
			for offset < len(records) && records[offset].When < since {
				offset++
			}
		}
		end := offset + 2
		next := ""
		if end < len(records) {
			next = strconv.Itoa(end)
		} else {
			end = len(records)
		}
		items := records[offset:end]
		if items == nil {
			items = []cursorRecord{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "next": next})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cursorConfig(endpoint string) *config.WebAPI {
	hours := 1
	return &config.WebAPI{
		Enabled: true, Name: "cursor-source", Endpoint: endpoint, Method: "get",
		AuthType: "apikey", APIKey: "test-token", SleepTime: 1, Timeout: 5,
		Collector: &config.Collector{Type: "cursor_test", InitialLookbackHours: &hours},
	}
}

func buildWithSource(t *testing.T, cfg *config.WebAPI, src Source, dir string, out output.Output) *Collector {
	t.Helper()
	c, err := newCollectorWithSource(Options{
		Agent: config.JarvisAgent{ID: "id", Name: "agent"}, Source: cfg, StateDir: dir,
		Fetcher: api.NewClient(cfg), Outputs: []output.NamedOutput{{Name: "o", Output: out}},
	}, src, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func TestEngineWithFakeSource(t *testing.T) {
	records := []cursorRecord{
		{"r1", ms(-3000)}, {"r2", ms(-2000)}, {"r3", ms(-2000)}, {"r4", ms(-1000)}, {"r5", ms(-500)},
	}
	srv := cursorServer(t, records)
	cfg := cursorConfig(srv.URL + "/events")
	dir := t.TempDir()
	out := &keyOutput{}
	c := buildWithSource(t, cfg, &cursorSource{endpoint: cfg.Endpoint}, dir, out)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Wait()
	if got := strings.Join(out.got(), ","); got != "r1,r2,r3,r4,r5" {
		t.Fatalf("delivered %s", got)
	}

	state, err := loadState(statePath(dir, cfg.Name), cfg.Name, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if cp := state["o"]; cp.TimestampMS != ms(-500) || len(cp.IDs) != 1 || cp.IDs[0] != "r5" {
		t.Fatalf("checkpoint = %+v", cp)
	}

	// A restarted collector resumes from the saved checkpoint.
	again := &keyOutput{}
	c = buildWithSource(t, cfg, &cursorSource{endpoint: cfg.Endpoint}, dir, again)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Wait()
	if got := again.got(); len(got) != 0 {
		t.Fatalf("redelivered %v", got)
	}
}

func TestEngineRefusesCrossOriginNext(t *testing.T) {
	var foreign atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreign.Add(1)
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(other.Close)

	srv := cursorServer(t, []cursorRecord{{"r1", ms(-3000)}, {"r2", ms(-2000)}, {"r3", ms(-1000)}})
	cfg := cursorConfig(srv.URL + "/events")
	out := &keyOutput{}
	src := &cursorSource{endpoint: cfg.Endpoint, nextBase: other.URL + "/events"}
	c := buildWithSource(t, cfg, src, t.TempDir(), out)
	err := c.Poll(context.Background())
	c.Wait()
	if err == nil || !strings.Contains(err.Error(), "different origin") {
		t.Fatalf("err = %v", err)
	}
	if foreign.Load() != 0 {
		t.Fatal("request sent to another origin")
	}
	if got := out.got(); len(got) != 0 {
		t.Fatalf("delivered %v", got)
	}
}

func TestNewRejectsUnknownCollectorType(t *testing.T) {
	cfg := cursorConfig("https://example.test/events")
	cfg.Collector.Type = "no_such_type"
	_, err := New(Options{Source: cfg, StateDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", "no_such_type")) {
		t.Fatalf("err = %v", err)
	}
}
