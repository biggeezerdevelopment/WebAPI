package api

import (
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
