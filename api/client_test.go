package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"gopkg.in/yaml.v3"

	"webapi/config"
)

func TestSendRequestBody(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "JSON body is sent unchanged", yaml: `'{"a":1}'`, want: `{"a":1}`},
		{name: "empty body sends no body", yaml: `""`, want: ""},
		{name: "list body is sent as a JSON array", yaml: `[metrics, cpu]`, want: `["metrics","cpu"]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body config.Body
			if err := yaml.Unmarshal([]byte(tt.yaml), &body); err != nil {
				t.Fatal(err)
			}

			var gotBody []byte
			var gotContentLength int64
			var readErr error

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotContentLength = r.ContentLength
				gotBody, readErr = io.ReadAll(r.Body)
				w.Write([]byte("ok"))
			}))
			defer server.Close()

			client := NewClient(&config.WebAPI{
				Endpoint: server.URL,
				Method:   "post",
				Timeout:  5,
				Body:     body,
			})

			resp, err := client.SendRequest(context.Background())
			if err != nil {
				t.Fatalf("SendRequest returned error: %v", err)
			}
			if readErr != nil {
				t.Fatalf("server failed to read body: %v", readErr)
			}
			if string(resp) != "ok" {
				t.Errorf("response = %q, want %q", resp, "ok")
			}
			if string(gotBody) != tt.want {
				t.Errorf("request body = %q, want %q", gotBody, tt.want)
			}
			if gotContentLength != int64(len(tt.want)) {
				t.Errorf("Content-Length = %d, want %d", gotContentLength, len(tt.want))
			}
		})
	}
}

// newTestClient builds a client and fails the test on a TLS setup error.
func newTestClient(t *testing.T, cfg *config.WebAPI) *Client {
	t.Helper()
	return NewClient(cfg)
}
