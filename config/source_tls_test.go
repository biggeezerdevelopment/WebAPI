package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"webapi/internal/testcerts"
)

const sourceTLSTemplate = `
webapi:
  - enabled: true
    name: "src"
    endpoint: "%ENDPOINT%"
    method: "get"
    sleeptime: 60
    timeout: 5
%EXTRA%
`

func loadSourceTLS(t *testing.T, endpoint, extra string) (*Config, error) {
	t.Helper()
	if endpoint == "" {
		endpoint = "https://api.example.com/v1"
	}
	text := strings.NewReplacer("%ENDPOINT%", endpoint, "%EXTRA%", extra).Replace(sourceTLSTemplate)
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

func TestSourceTLSBlockIsAccepted(t *testing.T) {
	b := testcerts.New(t)
	cfg, err := loadSourceTLS(t, "", `    tls:
      ca_file: "`+filepath.ToSlash(b.CAFile)+`"
      cert_file: "`+filepath.ToSlash(b.ClientCertFile)+`"
      key_file: "`+filepath.ToSlash(b.ClientKeyFile)+`"
      server_name: "api.internal"`)
	if err != nil {
		t.Fatal(err)
	}
	opts := cfg.WebAPI[0].TLSOptions()
	if opts.CAFile == "" || opts.CertFile == "" || opts.KeyFile == "" || opts.ServerName != "api.internal" || opts.SkipVerify {
		t.Fatalf("TLSOptions = %+v", opts)
	}
}

func TestSourceLegacySkipVerifyStillWorks(t *testing.T) {
	cfg, err := loadSourceTLS(t, "", "    skip_verify: true")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WebAPI[0].TLSOptions().SkipVerify {
		t.Fatal("top-level skip_verify was not applied")
	}
}

func TestSourceTLSValidation(t *testing.T) {
	b := testcerts.New(t)
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbage, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	ca := filepath.ToSlash(b.CAFile)
	tests := []struct {
		name, extra, want string
	}{
		{"skip_verify with ca_file", "    tls:\n      skip_verify: true\n      ca_file: \"" + ca + "\"", "skip_verify cannot be combined with ca_file"},
		{"top-level skip_verify with tls block", "    skip_verify: true\n    tls:\n      ca_file: \"" + ca + "\"", "skip_verify and tls cannot both be set"},
		{"top-level skip_verify false with tls block", "    skip_verify: false\n    tls:\n      skip_verify: true", "skip_verify and tls cannot both be set"},
		{"cert without key", "    tls:\n      cert_file: \"" + filepath.ToSlash(b.ClientCertFile) + "\"", "key_file"},
		{"missing ca_file", "    tls:\n      ca_file: \"" + ca + ".missing\"", "ca_file"},
		{"unparsable ca_file", "    tls:\n      ca_file: \"" + filepath.ToSlash(garbage) + "\"", "ca_file"},
		{"mismatched key", "    tls:\n      cert_file: \"" + filepath.ToSlash(b.ClientCertFile) + "\"\n      key_file: \"" + filepath.ToSlash(b.OtherKeyFile) + "\"", "client certificate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadSourceTLS(t, "", tt.extra)
			if err == nil {
				t.Fatal("LoadConfig succeeded")
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), `webapi[0] (name "src")`) {
				t.Fatalf("err = %v, want %q naming the source", err, tt.want)
			}
		})
	}
}

func TestSourceTLSIgnoredWhenDisabled(t *testing.T) {
	text := strings.Replace(sourceTLSTemplate, "enabled: true", "enabled: false", 1)
	text = strings.NewReplacer("%ENDPOINT%", "https://api.example.com", "%EXTRA%", "    tls:\n      ca_file: \"/does/not/exist.pem\"").Replace(text)
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("disabled source TLS was validated: %v", err)
	}
}

func TestSourceWarnings(t *testing.T) {
	tests := []struct {
		name, endpoint, extra, want string
	}{
		{"plaintext http", "http://api.example.com/v1", "", `source "src" uses plaintext http`},
		{"legacy skip_verify", "", "    skip_verify: true", `source "src" has TLS verification disabled (skip_verify: true)`},
		{"tls skip_verify", "", "    tls:\n      skip_verify: true", `source "src" has TLS verification disabled (skip_verify: true)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadSourceTLS(t, tt.endpoint, tt.extra)
			if err != nil {
				t.Fatal(err)
			}
			warnings := strings.Join(cfg.OutputWarnings(), "\n")
			if !strings.Contains(warnings, tt.want) {
				t.Fatalf("warnings = %q, want %q", warnings, tt.want)
			}
			if strings.Contains(warnings, "api.example.com") {
				t.Fatalf("warnings include the endpoint: %q", warnings)
			}
		})
	}
	cfg, err := loadSourceTLS(t, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.OutputWarnings(); len(w) != 0 {
		t.Fatalf("https source without skip_verify warned: %v", w)
	}
}
