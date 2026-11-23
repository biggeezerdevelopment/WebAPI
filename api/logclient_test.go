package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"webapi/config"
)

// sendAndCaptureResults calls SendLog against a test server and returns the
// raw JSON the server received in the api_results field.
func sendAndCaptureResults(t *testing.T, apiResults interface{}) string {
	t.Helper()

	received := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewLogClient(&config.APILogRemote{
		APIURL:    server.URL,
		APIMethod: http.MethodPost,
	})
	if err := client.SendLog("agent-1", "agent", "source", apiResults); err != nil {
		t.Fatalf("SendLog returned error: %v", err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(<-received, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	results, ok := payload["api_results"]
	if !ok {
		t.Fatal("payload has no api_results field")
	}
	return string(results)
}

func TestSendLogForwardsJSONResponseAsJSON(t *testing.T) {
	got := sendAndCaptureResults(t, []byte(`{"status":"ok","count":12345678901234567890}`))

	// The large number also verifies the value is not rounded through float64.
	want := `{"status":"ok","count":12345678901234567890}`
	if got != want {
		t.Errorf("api_results = %s, want %s", got, want)
	}
}

func TestSendLogForwardsNonJSONResponseAsString(t *testing.T) {
	got := sendAndCaptureResults(t, []byte("<html>service unavailable</html>"))

	var decoded string
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("api_results is not a JSON string: %s", got)
	}
	if want := "<html>service unavailable</html>"; decoded != want {
		t.Errorf("api_results = %q, want %q", decoded, want)
	}
}

func TestSendLogKeepsNilBodyNull(t *testing.T) {
	var body []byte // SendRequest returns a nil slice when the request fails.

	if got := sendAndCaptureResults(t, body); got != "null" {
		t.Errorf("api_results = %s, want null", got)
	}
}

// newSelfSignedServer starts a TLS server using httptest's self-signed
// certificate and counts the requests that reach its handler.
func newSelfSignedServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var hits atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	// Rejected handshakes are expected here, so keep them out of test output.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)

	return server, &hits
}

// loadLogConfig parses the apilogremote section through config.LoadConfig so
// the tests also cover the skip_verify YAML key and its default.
func loadLogConfig(t *testing.T, url, skipVerifyLine string) *config.APILogRemote {
	t.Helper()

	yaml := fmt.Sprintf("apilogremote:\n  enabled: true\n  api_url: %q\n  api_method: POST\n", url)
	if skipVerifyLine != "" {
		yaml += "  " + skipVerifyLine + "\n"
	}

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	return &cfg.APILogRemote
}

func TestLogClientRejectsSelfSignedCertificateByDefault(t *testing.T) {
	tests := []struct {
		name           string
		skipVerifyLine string
	}{
		{name: "skip_verify omitted"},
		{name: "skip_verify false", skipVerifyLine: "skip_verify: false"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, hits := newSelfSignedServer(t)
			client := NewLogClient(loadLogConfig(t, server.URL, tt.skipVerifyLine))

			err := client.SendLog("agent-id", "agent-name", "test", nil)
			if err == nil {
				t.Fatal("SendLog succeeded against a self-signed server, want certificate error")
			}
			if !strings.Contains(err.Error(), "certificate") {
				t.Fatalf("SendLog error = %q, want certificate error", err)
			}
			if got := hits.Load(); got != 0 {
				t.Fatalf("server received %d requests, want 0", got)
			}
		})
	}
}

func TestLogClientAcceptsSelfSignedCertificateWhenSkipVerifyTrue(t *testing.T) {
	server, hits := newSelfSignedServer(t)
	client := NewLogClient(loadLogConfig(t, server.URL, "skip_verify: true"))

	if err := client.SendLog("agent-id", "agent-name", "test", nil); err != nil {
		t.Fatalf("SendLog error = %v, want nil", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server received %d requests, want 1", got)
	}
}
