package output

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"webapi/config"
)

// flakyServer fails the first n requests with 500 and then succeeds.
func flakyServer(t *testing.T, n int32) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var calls atomic.Int32
	var lastBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		lastBody.Store(string(data))
		if calls.Add(1) <= n {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &lastBody
}

func logRemoteRouter(t *testing.T, url string, timeout int) *Router {
	t.Helper()
	cfg := &config.Config{APILogRemote: config.APILogRemote{Enabled: true, APIURL: url, APIMethod: "POST", APITimeout: timeout}}
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close() })
	return router
}

func TestLogRemoteDispatcherRetriesAndReportsOnce(t *testing.T) {
	srv, calls, body := flakyServer(t, 2)
	router := logRemoteRouter(t, srv.URL, 0)
	obs := newRecordingObserver()
	d, err := router.ObservedDispatcher(&config.WebAPI{Name: "legacy"}, obs)
	if err != nil {
		t.Fatal(err)
	}

	rec := testRecord()
	rec.Data = nil
	d.Deliver(context.Background(), rec) // synchronous for a single output
	if got := calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want 3 (two failures, then success)", got)
	}
	if !strings.Contains(body.Load().(string), `"api_results":null`) {
		t.Errorf("body = %v", body.Load())
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.delivered[config.APILogRemoteOutputName] != 1 || len(obs.failed[config.APILogRemoteOutputName]) != 0 {
		t.Fatalf("observer delivered = %v, failed = %v", obs.delivered, obs.failed)
	}
}

func TestLogRemoteDispatcherReportsFailureAfterRetries(t *testing.T) {
	srv, calls, _ := flakyServer(t, 100)
	router := logRemoteRouter(t, srv.URL, 0)
	obs := newRecordingObserver()
	d, err := router.ObservedDispatcher(&config.WebAPI{Name: "legacy"}, obs)
	if err != nil {
		t.Fatal(err)
	}
	d.Deliver(context.Background(), testRecord())
	if got := calls.Load(); got != maxRetries+1 {
		t.Fatalf("calls = %d, want %d", got, maxRetries+1)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if errs := obs.failed[config.APILogRemoteOutputName]; len(errs) != 1 || !strings.Contains(errs[0].Error(), "giving up after 4 attempt(s)") {
		t.Fatalf("failed = %v", obs.failed)
	}
}

func TestLogRemoteCollectorRouteRetries(t *testing.T) {
	srv, calls, _ := flakyServer(t, 1)
	router := logRemoteRouter(t, srv.URL, 0)
	routes, err := router.Routes(&config.WebAPI{Name: "src", Outputs: []string{config.APILogRemoteOutputName}})
	if err != nil {
		t.Fatal(err)
	}
	if err := routes[0].Output.Send(context.Background(), testRecord()); err != nil {
		t.Fatalf("Send = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestLogRemoteRetriesBoundedByAPITimeout(t *testing.T) {
	hang := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(hang) })

	router := logRemoteRouter(t, srv.URL, 1)
	d, err := router.Dispatcher(&config.WebAPI{Name: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	d.Deliver(context.Background(), testRecord())
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("delivery took %v, want about api_timeout (1s)", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 (the timeout covers all retries)", got)
	}
}

func TestLogRemoteStopsOnCancel(t *testing.T) {
	srv, calls, _ := flakyServer(t, 100)
	router := logRemoteRouter(t, srv.URL, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	routes, err := router.Routes(&config.WebAPI{Name: "src", Outputs: []string{config.APILogRemoteOutputName}})
	if err != nil {
		t.Fatal(err)
	}
	if err := routes[0].Output.Send(ctx, testRecord()); err == nil {
		t.Fatal("canceled send succeeded")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("calls = %d after cancel, want 0", got)
	}
}
