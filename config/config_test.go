package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validWebAPI() WebAPI {
	return WebAPI{
		Enabled:   true,
		Name:      "metrics",
		Endpoint:  "https://example.com/api",
		Method:    "post",
		SleepTime: 30,
		Timeout:   10,
	}
}

func validLogRemote() APILogRemote {
	return APILogRemote{
		Enabled:   true,
		APIURL:    "http://localhost:8080/logs",
		APIMethod: "POST",
	}
}

func TestValidateWebAPI(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*WebAPI)
		wantErr string
	}{
		{name: "valid", mutate: func(*WebAPI) {}},
		{name: "http endpoint with port", mutate: func(w *WebAPI) { w.Endpoint = "http://localhost:8080/x" }},
		{name: "uppercase scheme", mutate: func(w *WebAPI) { w.Endpoint = "HTTPS://example.com" }},
		{name: "mixed case method", mutate: func(w *WebAPI) { w.Method = "Get" }},
		{name: "disabled entry is not validated", mutate: func(w *WebAPI) { *w = WebAPI{Enabled: false} }},
		{name: "missing name", mutate: func(w *WebAPI) { w.Name = "" }, wantErr: "name is required"},
		{name: "blank name", mutate: func(w *WebAPI) { w.Name = "  " }, wantErr: "name is required"},
		{name: "missing endpoint", mutate: func(w *WebAPI) { w.Endpoint = "" }, wantErr: "endpoint"},
		{name: "relative endpoint", mutate: func(w *WebAPI) { w.Endpoint = "/api/v1" }, wantErr: "endpoint"},
		{name: "no scheme", mutate: func(w *WebAPI) { w.Endpoint = "example.com/api" }, wantErr: "endpoint"},
		{name: "ftp endpoint", mutate: func(w *WebAPI) { w.Endpoint = "ftp://example.com" }, wantErr: "endpoint"},
		{name: "opaque endpoint", mutate: func(w *WebAPI) { w.Endpoint = "http:example.com" }, wantErr: "endpoint"},
		{name: "endpoint without host", mutate: func(w *WebAPI) { w.Endpoint = "http://:8080/api" }, wantErr: "endpoint"},
		{name: "missing method", mutate: func(w *WebAPI) { w.Method = "" }, wantErr: "invalid method"},
		{name: "unknown method", mutate: func(w *WebAPI) { w.Method = "FETCH" }, wantErr: `invalid method "FETCH"`},
		{name: "zero sleeptime", mutate: func(w *WebAPI) { w.SleepTime = 0 }, wantErr: "sleeptime must be greater than 0"},
		{name: "negative sleeptime", mutate: func(w *WebAPI) { w.SleepTime = -1 }, wantErr: "sleeptime must be greater than 0"},
		{name: "zero timeout", mutate: func(w *WebAPI) { w.Timeout = 0 }, wantErr: "timeout must be greater than 0"},
		{name: "negative timeout", mutate: func(w *WebAPI) { w.Timeout = -1 }, wantErr: "timeout must be greater than 0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := validWebAPI()
			tt.mutate(&entry)
			// The bad entry sits second so the error must identify its index.
			cfg := Config{WebAPI: []WebAPI{validWebAPI(), entry}}

			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tt.wantErr)
			}
			for _, want := range []string{"webapi[1]", tt.wantErr} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestValidateAPILogRemote(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*APILogRemote)
		wantErr string
	}{
		{name: "valid", mutate: func(*APILogRemote) {}},
		{name: "lowercase method", mutate: func(l *APILogRemote) { l.APIMethod = "post" }},
		{name: "disabled is not validated", mutate: func(l *APILogRemote) { *l = APILogRemote{} }},
		{name: "missing url", mutate: func(l *APILogRemote) { l.APIURL = "" }, wantErr: "api_url"},
		{name: "relative url", mutate: func(l *APILogRemote) { l.APIURL = "/logs" }, wantErr: "api_url"},
		{name: "bad scheme", mutate: func(l *APILogRemote) { l.APIURL = "ftp://example.com" }, wantErr: "api_url"},
		{name: "missing method", mutate: func(l *APILogRemote) { l.APIMethod = "" }, wantErr: "api_method"},
		{name: "unknown method", mutate: func(l *APILogRemote) { l.APIMethod = "SEND" }, wantErr: `invalid api_method "SEND"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logRemote := validLogRemote()
			tt.mutate(&logRemote)
			cfg := Config{APILogRemote: logRemote}

			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tt.wantErr)
			}
			for _, want := range []string{"apilogremote", tt.wantErr} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestValidateErrorOmitsURLCredentials(t *testing.T) {
	entry := validWebAPI()
	entry.Endpoint = "ftp://user:s3cret@example.com"
	cfg := Config{WebAPI: []WebAPI{entry}}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaks credentials: %v", err)
	}
}

func TestLoadConfigRepositoryFile(t *testing.T) {
	cfg, err := LoadConfig("../config.yml")
	if err != nil {
		t.Fatalf("repository config.yml must load: %v", err)
	}
	if len(cfg.WebAPI) != 1 || cfg.WebAPI[0].Name != "qualys" {
		t.Fatalf("unexpected webapi entries: %+v", cfg.WebAPI)
	}
}

func TestLoadConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "valid enabled entry",
			yaml: `webapi:
  - {enabled: true, name: a, endpoint: "http://example.com", method: get, sleeptime: 1, timeout: 1}
`,
		},
		{
			name: "invalid disabled entry",
			yaml: `webapi:
  - {enabled: false, endpoint: "not a url", sleeptime: -1}
`,
		},
		{
			name: "invalid enabled entry",
			yaml: `webapi:
  - {enabled: true, name: broken, endpoint: "http://example.com", method: get, sleeptime: 1, timeout: 0}
`,
			wantErr: `webapi[0] (name "broken"): timeout must be greater than 0`,
		},
		{
			name: "invalid log remote",
			yaml: `apilogremote: {enabled: true, api_url: "nope", api_method: POST}
`,
			wantErr: "apilogremote: api_url",
		},
		{
			name: "mapping body rejected",
			yaml: `webapi:
  - {enabled: false, body: {a: b}}
`,
			wantErr: "body must be a string or a list of strings",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := LoadConfig(path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestBodyEncoding(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		wantEmpty bool
		want      string
	}{
		{name: "absent", yaml: `name: a`, wantEmpty: true},
		{name: "empty string", yaml: `body: ""`, wantEmpty: true},
		{name: "empty list", yaml: `body: []`, wantEmpty: true},
		{name: "string", yaml: `body: hello`, want: `hello`},
		{name: "serialized JSON string", yaml: `body: '{"a":1}'`, want: `{"a":1}`},
		{name: "list", yaml: `body: [metrics, cpu]`, want: `["metrics","cpu"]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			content := "webapi:\n  - " + tt.yaml + "\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			body := cfg.WebAPI[0].Body
			if body.IsEmpty() != tt.wantEmpty {
				t.Fatalf("IsEmpty() = %v, want %v", body.IsEmpty(), tt.wantEmpty)
			}
			got, err := body.Payload()
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantEmpty {
				if got != nil {
					t.Errorf("Payload() = %q, want nil", got)
				}
				return
			}
			if string(got) != tt.want {
				t.Errorf("Payload() = %s, want %s", got, tt.want)
			}
		})
	}
}
