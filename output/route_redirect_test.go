package output

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"webapi/config"
)

func TestCollectorRouteRefusesRedirectButDispatcherFollows(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetHits.Add(1)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer srv.Close()

	cfg := &config.Config{APILogRemote: config.APILogRemote{Enabled: true, APIURL: srv.URL, APIMethod: http.MethodPost}}
	source := &config.WebAPI{Name: "src", Outputs: []string{config.APILogRemoteOutputName}}
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()

	routes, err := router.Routes(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := routes[0].Output.Send(context.Background(), testRecord()); err == nil {
		t.Fatal("collector route reported a redirected send as delivered")
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("collector route followed the redirect %d time(s)", got)
	}

	dispatcher, err := router.Dispatcher(source)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.Deliver(context.Background(), testRecord())
	if got := targetHits.Load(); got != 1 {
		t.Fatalf("plain source reached the target %d time(s), want 1", got)
	}
}
