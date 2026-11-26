package api

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"context"

	"webapi/config"
	"webapi/internal/testcerts"
	"webapi/tlsconfig"
)

func tlsServer(t *testing.T, b *testcerts.Bundle, requireClientCert bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{b.ServerCert}}
	if requireClientCert {
		srv.TLS.ClientAuth = tls.RequireAndVerifyClientCert
		srv.TLS.ClientCAs = b.CAPool
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestClientTrustsConfiguredCA(t *testing.T) {
	b := testcerts.New(t)
	srv := tlsServer(t, b, false)

	untrusted := newTestClient(t, &config.WebAPI{Endpoint: srv.URL, Method: "get", Timeout: 5})
	if _, err := untrusted.SendRequest(context.Background()); err == nil {
		t.Fatal("request to a private CA succeeded without ca_file")
	}

	trusted := newTestClient(t, &config.WebAPI{Endpoint: srv.URL, Method: "get", Timeout: 5,
		TLS: &tlsconfig.Options{CAFile: b.CAFile}})
	if body, err := trusted.SendRequest(context.Background()); err != nil || string(body) != "ok" {
		t.Fatalf("body = %q, err = %v", body, err)
	}
}

func TestClientPresentsClientCertificate(t *testing.T) {
	b := testcerts.New(t)
	srv := tlsServer(t, b, true)

	noCert := newTestClient(t, &config.WebAPI{Endpoint: srv.URL, Method: "get", Timeout: 5,
		TLS: &tlsconfig.Options{CAFile: b.CAFile}})
	if _, err := noCert.SendRequest(context.Background()); err == nil {
		t.Fatal("mutual TLS server accepted a client without a certificate")
	}

	withCert := newTestClient(t, &config.WebAPI{Endpoint: srv.URL, Method: "get", Timeout: 5,
		TLS: &tlsconfig.Options{CAFile: b.CAFile, CertFile: b.ClientCertFile, KeyFile: b.ClientKeyFile}})
	if _, err := withCert.SendRequest(context.Background()); err != nil {
		t.Fatalf("mutual TLS request failed: %v", err)
	}
}

func TestClientServerNameOverride(t *testing.T) {
	b := testcerts.New(t)
	srv := tlsServer(t, b, false)
	wrong := newTestClient(t, &config.WebAPI{Endpoint: srv.URL, Method: "get", Timeout: 5,
		TLS: &tlsconfig.Options{CAFile: b.CAFile, ServerName: "not-in-cert.test"}})
	if _, err := wrong.SendRequest(context.Background()); err == nil {
		t.Fatal("server_name not in the certificate was accepted")
	}
	right := newTestClient(t, &config.WebAPI{Endpoint: srv.URL, Method: "get", Timeout: 5,
		TLS: &tlsconfig.Options{CAFile: b.CAFile, ServerName: "webhook.test"}})
	if _, err := right.SendRequest(context.Background()); err != nil {
		t.Fatalf("server_name in the certificate failed: %v", err)
	}
}

func TestClientLegacySkipVerify(t *testing.T) {
	b := testcerts.New(t)
	srv := tlsServer(t, b, false)
	c := newTestClient(t, &config.WebAPI{Endpoint: srv.URL, Method: "get", Timeout: 5, SkipVerify: true})
	if _, err := c.SendRequest(context.Background()); err != nil {
		t.Fatalf("top-level skip_verify request failed: %v", err)
	}
}

func TestClientRejectsTLS11(t *testing.T) {
	b := testcerts.New(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{b.ServerCert}, MaxVersion: tls.VersionTLS11}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	c := newTestClient(t, &config.WebAPI{Endpoint: srv.URL, Method: "get", Timeout: 5,
		TLS: &tlsconfig.Options{CAFile: b.CAFile}})
	if _, err := c.SendRequest(context.Background()); err == nil {
		t.Fatal("TLS 1.1 server was accepted")
	}
}

func TestNewClientReportsBadTLSFiles(t *testing.T) {
	_, err := NewClient(&config.WebAPI{Endpoint: "https://example.invalid", Method: "get", Timeout: 5,
		TLS: &tlsconfig.Options{CAFile: "/does/not/exist.pem"}})
	if err == nil || !strings.Contains(err.Error(), "ca_file") {
		t.Fatalf("err = %v", err)
	}
}
