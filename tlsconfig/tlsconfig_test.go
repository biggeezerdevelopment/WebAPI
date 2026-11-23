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

func TestBuildAcceptsCommentedBundle(t *testing.T) {
	b := testcerts.New(t)
	good, err := os.ReadFile(b.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bundle.pem")
	content := "# First CA\n" + string(good) + "\n# Second CA\n" + string(good) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(Options{CAFile: path}); err != nil {
		t.Fatalf("commented bundle rejected: %v", err)
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
	withSuffix := func(name, suffix string) string {
		path := filepath.Join(dir, name)
		content := append(append([]byte{}, good...), suffix...)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	badBase64 := withSuffix("bad-base64.pem", "-----BEGIN CERTIFICATE-----\n!!!not base64!!!\n-----END CERTIFICATE-----\n")
	truncated := withSuffix("truncated.pem", "-----BEGIN CERTIFICATE-----\nAAAA\n")
	trailing := withSuffix("trailing.pem", secret+"\n")
	nonCert := withSuffix("non-cert.pem", "-----BEGIN TRUSTED CERTIFICATE-----\nAAAA\n-----END TRUSTED CERTIFICATE-----\n")

	// Each of these damages a certificate between two valid ones.
	goodPEM := string(good)
	_, orphanBody, _ := strings.Cut(goodPEM, "\n")
	damagedBegin := withSuffix("damaged-begin.pem", strings.Replace(goodPEM, "-----BEGIN", "----BEGIN", 1)+goodPEM)
	orphanedEnd := withSuffix("orphaned-end.pem", orphanBody+goodPEM)
	onlyEnd := withSuffix("only-end.pem", "-----END CERTIFICATE-----\n"+goodPEM)
	middleBadBase64 := withSuffix("middle-bad-base64.pem", "-----BEGIN CERTIFICATE-----\n!!!not base64!!!\n-----END CERTIFICATE-----\n"+goodPEM)
	middleNoEnd := withSuffix("middle-no-end.pem", "-----BEGIN CERTIFICATE-----\nAAAA\n"+goodPEM)
	middleText := withSuffix("middle-text.pem", "stray text\n"+goodPEM)
	indentedBegin := withSuffix("indented-begin.pem", " "+goodPEM+goodPEM)

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
		{"valid cert then invalid base64 block", Options{CAFile: badBase64}, "damaged or truncated"},
		{"valid cert then truncated block", Options{CAFile: truncated}, "damaged or truncated"},
		{"valid cert then trailing data", Options{CAFile: trailing}, "unexpected data"},
		{"valid cert then non-certificate block", Options{CAFile: nonCert}, "non-certificate"},
		{"middle cert with damaged BEGIN", Options{CAFile: damagedBegin}, "unexpected data"},
		{"middle cert with missing BEGIN", Options{CAFile: orphanedEnd}, "unexpected data"},
		{"orphaned END between certs", Options{CAFile: onlyEnd}, "unexpected data"},
		{"middle cert with invalid base64", Options{CAFile: middleBadBase64}, "damaged or truncated"},
		{"middle cert with missing END", Options{CAFile: middleNoEnd}, "damaged or truncated"},
		{"text between certs", Options{CAFile: middleText}, "unexpected data"},
		{"indented BEGIN", Options{CAFile: indentedBegin}, "unexpected data"},
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
