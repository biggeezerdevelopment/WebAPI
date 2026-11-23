package tlsconfig

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"webapi/internal/testcerts"
)

func TestBuildDefaults(t *testing.T) {
	cfg, err := Build(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x", cfg.MinVersion)
	}
	if cfg.InsecureSkipVerify {
		t.Error("verification must be on by default")
	}
	if cfg.RootCAs != nil {
		t.Error("RootCAs must be nil so the system roots are used")
	}
}

func TestBuildWithFiles(t *testing.T) {
	b := testcerts.New(t)
	cfg, err := Build(Options{
		CAFile: b.CAFile, CertFile: b.ClientCertFile, KeyFile: b.ClientKeyFile, ServerName: "webhook.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootCAs == nil || len(cfg.Certificates) != 1 || cfg.ServerName != "webhook.test" {
		t.Errorf("unexpected config: roots=%v certs=%d name=%q", cfg.RootCAs != nil, len(cfg.Certificates), cfg.ServerName)
	}
}

func TestBuildErrors(t *testing.T) {
	b := testcerts.New(t)
	dir := t.TempDir()
	const secret = "NOT-A-REAL-SECRET-CONTENT"
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, "corrupt.pem")
	good, err := os.ReadFile(b.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	bad := "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(corrupt, append(good, bad...), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.pem")

	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"skip verify with CA", Options{SkipVerify: true, CAFile: b.CAFile}, "skip_verify"},
		{"cert without key", Options{CertFile: b.ClientCertFile}, "key_file"},
		{"key without cert", Options{KeyFile: b.ClientKeyFile}, "cert_file"},
		{"unreadable CA", Options{CAFile: missing}, "ca_file"},
		{"unparsable CA", Options{CAFile: garbage}, "no certificates"},
		{"corrupt CA entry", Options{CAFile: corrupt}, "invalid certificate"},
		{"unreadable cert", Options{CertFile: missing, KeyFile: b.ClientKeyFile}, "client certificate"},
		{"unreadable key", Options{CertFile: b.ClientCertFile, KeyFile: missing}, "client certificate"},
		{"unparsable cert", Options{CertFile: garbage, KeyFile: b.ClientKeyFile}, "client certificate"},
		{"unparsable key", Options{CertFile: b.ClientCertFile, KeyFile: garbage}, "client certificate"},
		{"mismatched pair", Options{CertFile: b.ClientCertFile, KeyFile: b.OtherKeyFile}, "client certificate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Build(tt.opts)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks file contents: %q", err)
			}
		})
	}
}
