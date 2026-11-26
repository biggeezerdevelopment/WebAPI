package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"webapi/config"
)

func TestBasicAuthPasswordFromEnv(t *testing.T) {
	t.Setenv("TEST_JIRA_TOKEN", "env-token-value")
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	client := newTestClient(t, &config.WebAPI{
		Endpoint:       server.URL,
		Method:         "get",
		Timeout:        5,
		AuthType:       "basic",
		APIUsername:    "svc-audit@example.com",
		APIPasswordEnv: "TEST_JIRA_TOKEN",
	})
	// The password resolves once, in NewClient, not per request.
	t.Setenv("TEST_JIRA_TOKEN", "changed-later")
	if _, err := client.SendRequest(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("svc-audit@example.com:env-token-value"))
	if got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}
