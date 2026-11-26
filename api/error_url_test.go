package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"webapi/config"
)

const secret = "s3cr3t-token"

// closedURL returns the URL of a server that is no longer listening, so a
// request to it fails in the transport.
func closedURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

// slowURL returns the URL of a server that answers after d.
func slowURL(t *testing.T, d time.Duration) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func assertNoURL(t *testing.T, err error, base string) {
	t.Helper()
	if err == nil {
		t.Fatal("request succeeded, want an error")
	}
	if msg := err.Error(); strings.Contains(msg, secret) || strings.Contains(msg, base) {
		t.Fatalf("error text leaks the URL: %q", msg)
	}
}

func TestSendRequestErrorOmitsURL(t *testing.T) {
	for name, base := range map[string]string{
		"connection refused": closedURL(t),
		"client timeout":     slowURL(t, 3*time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			client := newTestClient(t, &config.WebAPI{
				Endpoint: base + "/poll?token=" + secret,
				Method:   "get",
				Timeout:  1,
				Params:   map[string]string{"api_key": secret},
			})
			_, err := client.SendRequest(context.Background())
			assertNoURL(t, err, base)
		})
	}
}

func TestSendRequestTimeoutStillDetectable(t *testing.T) {
	client := newTestClient(t, &config.WebAPI{Endpoint: slowURL(t, 3*time.Second), Method: "get", Timeout: 1})
	_, err := client.SendRequest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Client.Timeout") {
		t.Fatalf("err = %v, want the timeout cause kept", err)
	}
}

func TestSendLogErrorOmitsURL(t *testing.T) {
	base := closedURL(t)
	client := NewLogClient(&config.APILogRemote{APIURL: base + "/logs?key=" + secret, APIMethod: http.MethodPost})
	err := client.SendLog(context.Background(), "id", "agent", "src", []byte(`{}`))
	assertNoURL(t, err, base)
}

func TestRequestBuildErrorOmitsURL(t *testing.T) {
	bad := "http://example.invalid/%zz?token=" + secret
	client := newTestClient(t, &config.WebAPI{Endpoint: bad, Method: "get", Timeout: 1})
	_, err := client.SendRequest(context.Background())
	assertNoURL(t, err, "example.invalid")

	logClient := NewLogClient(&config.APILogRemote{APIURL: bad, APIMethod: http.MethodPost})
	assertNoURL(t, logClient.SendLog(context.Background(), "id", "agent", "src", nil), "example.invalid")
}
