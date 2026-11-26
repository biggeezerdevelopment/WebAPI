package output

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"webapi/api"
	"webapi/config"
)

// redirectServer answers every request with a 302 and counts the requests.
func redirectServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "https://elsewhere.invalid/", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func assertGaveUpOnce(t *testing.T, err error, calls *atomic.Int32) {
	t.Helper()
	if err == nil {
		t.Fatal("redirected send reported success")
	}
	if !errors.Is(err, api.ErrRedirect) {
		t.Errorf("err = %v, want api.ErrRedirect", err)
	}
	if !strings.Contains(err.Error(), "giving up after 1 attempt(s)") {
		t.Errorf("err = %v, want no retries", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1: a redirect fails the same way every time", got)
	}
}

func TestConfirmedLogRemoteDoesNotRetryRedirect(t *testing.T) {
	srv, calls := redirectServer(t)
	router := logRemoteRouter(t, srv.URL, 0)
	routes, err := router.Routes(&config.WebAPI{Name: "src", Outputs: []string{config.APILogRemoteOutputName}})
	if err != nil {
		t.Fatal(err)
	}
	assertGaveUpOnce(t, routes[0].Output.Send(context.Background(), testRecord()), calls)
}

func TestWebhookDoesNotRetryRedirect(t *testing.T) {
	srv, calls := redirectServer(t)
	router, err := NewRouter(&config.Config{LogOutputs: []config.LogOutput{*webhookConfig(srv.URL)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close() })
	routes, err := router.Routes(&config.WebAPI{Name: "src"})
	if err != nil {
		t.Fatal(err)
	}
	assertGaveUpOnce(t, routes[0].Output.Send(context.Background(), testRecord()), calls)
}
