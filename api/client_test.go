package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"webapi/config"
)

func TestSendRequestBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "JSON body is sent unchanged", body: `{"a":1}`},
		{name: "empty body sends no body", body: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
				Body:     tt.body,
			})

			resp, err := client.SendRequest()
			if err != nil {
				t.Fatalf("SendRequest returned error: %v", err)
			}
			if readErr != nil {
				t.Fatalf("server failed to read body: %v", readErr)
			}
			if string(resp) != "ok" {
				t.Errorf("response = %q, want %q", resp, "ok")
			}
			if string(gotBody) != tt.body {
				t.Errorf("request body = %q, want %q", gotBody, tt.body)
			}
			if gotContentLength != int64(len(tt.body)) {
				t.Errorf("Content-Length = %d, want %d", gotContentLength, len(tt.body))
			}
		})
	}
}
