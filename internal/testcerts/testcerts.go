// Package testcerts generates throwaway certificates for tests.
package testcerts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Bundle holds a generated CA, a server certificate, and client credentials.
type Bundle struct {
	CAFile         string
	CAPool         *x509.CertPool
	ServerCert     tls.Certificate
	ClientCertFile string
	ClientKeyFile  string
	// OtherKeyFile holds a valid key that does not match ClientCertFile.
	OtherKeyFile string
}

// New writes a fresh bundle to a test temporary directory.
func New(t testing.TB) *Bundle {
	t.Helper()
	dir := t.TempDir()

	caKey := newKey(t)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	issue := func(serial int64, usage x509.ExtKeyUsage) ([]byte, *ecdsa.PrivateKey) {
		key := newKey(t)
		template := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: "localhost"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
			DNSNames:     []string{"localhost", "webhook.test"},
			IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		}
		der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return der, key
	}

	serverDER, serverKey := issue(2, x509.ExtKeyUsageServerAuth)
	clientDER, clientKey := issue(3, x509.ExtKeyUsageClientAuth)

	b := &Bundle{
		CAFile:         filepath.Join(dir, "ca.pem"),
		CAPool:         pool,
		ServerCert:     tls.Certificate{Certificate: [][]byte{serverDER}, PrivateKey: serverKey},
		ClientCertFile: filepath.Join(dir, "client.pem"),
		ClientKeyFile:  filepath.Join(dir, "client-key.pem"),
		OtherKeyFile:   filepath.Join(dir, "other-key.pem"),
	}
	writeCert(t, b.CAFile, caDER)
	writeCert(t, b.ClientCertFile, clientDER)
	writeKey(t, b.ClientKeyFile, clientKey)
	writeKey(t, b.OtherKeyFile, newKey(t))
	return b
}

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func writeCert(t testing.TB, path string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeKey(t testing.TB, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
