package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
