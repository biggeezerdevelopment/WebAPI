package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"webapi/config"
)

func TestWithoutRedirectsFailsAndKeepsOriginalClient(t *testing.T) {
	statuses := []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect}
	for _, status := range statuses {
		var targetHits atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			targetHits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		server := httptest.NewServer(http.RedirectHandler(target.URL, status))

		client := NewLogClient(&config.APILogRemote{APIURL: server.URL, APIMethod: http.MethodPost})
		if err := client.WithoutRedirects().SendLog("id", "agent", "src", []byte(`{}`)); err == nil {
			t.Errorf("status %d: redirected send reported success", status)
		}
		if got := targetHits.Load(); got != 0 {
			t.Errorf("status %d: redirect followed %d time(s)", status, got)
		}
		if err := client.SendLog("id", "agent", "src", []byte(`{}`)); err != nil {
			t.Errorf("status %d: legacy client stopped following redirects: %v", status, err)
		}
		if got := targetHits.Load(); got != 1 {
			t.Errorf("status %d: legacy client reached target %d time(s), want 1", status, got)
		}
		server.Close()
		target.Close()
	}
}
