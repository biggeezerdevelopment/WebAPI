// Package tlsconfig builds client TLS configurations from file paths. It is
// transport-neutral so that any output type can share it.
package tlsconfig

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Options describes a client TLS configuration. Certificates and keys are
// accepted as file paths only.
type Options struct {
	CAFile     string `yaml:"ca_file"`
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	ServerName string `yaml:"server_name"`
	SkipVerify bool   `yaml:"skip_verify"`
}

// Build validates the options and returns a client TLS configuration with a
// TLS 1.2 minimum. A nil RootCAs means the system roots are used. Errors may
// name a file path but never include file contents.
func Build(o Options) (*tls.Config, error) {
	if o.SkipVerify && o.CAFile != "" {
		return nil, errors.New("skip_verify cannot be combined with ca_file")
	}
	if (o.CertFile == "") != (o.KeyFile == "") {
		return nil, errors.New("cert_file and key_file must be set together")
	}

	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         o.ServerName,
		InsecureSkipVerify: o.SkipVerify,
	}

	if o.CAFile != "" {
		pool, err := loadCAPool(o.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}

	if o.CertFile != "" {
		cert, err := loadKeyPair(o.CertFile, o.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading client certificate and key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

// loadKeyPair loads a client certificate and key. tls.X509KeyPair parses only
// the leaf, so the whole cert_file is validated first: a damaged intermediate
// must fail at startup rather than during a mutual TLS handshake. Each file is
// read once, so the bytes validated are the bytes used.
func loadKeyPair(certFile, keyFile string) (tls.Certificate, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("reading cert_file: %w", err)
	}
	if _, err := parseCertificates("cert_file", certFile, certPEM, isKeyMaterial); err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("reading key_file: %w", err)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// isKeyMaterial reports whether a PEM block type belongs to a private key, which
// lets cert_file and key_file name the same combined file.
func isKeyMaterial(blockType string) bool {
	return blockType == "PRIVATE KEY" || blockType == "EC PARAMETERS" ||
		strings.HasSuffix(blockType, " PRIVATE KEY")
}

// loadCAPool builds a pool from a bundle that must contain only certificates.
// Unlike CertPool.AppendCertsFromPEM it fails on a corrupt entry instead of
// skipping it, so a damaged bundle cannot silently narrow the trusted set.
func loadCAPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading ca_file: %w", err)
	}
	certs, err := parseCertificates("ca_file", path, data, nil)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	for _, cert := range certs {
		pool.AddCert(cert)
	}
	return pool, nil
}

// parseCertificates parses every certificate in a PEM file and fails on any
// corrupt entry. Blocks whose type satisfies skip, if non-nil, are ignored
// without being parsed. field names the setting in errors, which never include
// file contents.
//
// pem.Decode silently skips anything it cannot decode, including a block with a
// damaged BEGIN line, so the file is walked line by line. Outside a block only
// blank lines and # comments are allowed, and each decode must consume exactly
// the block that starts at the current line.
func parseCertificates(field, path string, data []byte, skip func(blockType string) bool) ([]*x509.Certificate, error) {
	pemBegin := []byte("-----BEGIN ")
	var certs []*x509.Certificate
	for len(data) > 0 {
		line, rest, _ := bytes.Cut(data, []byte("\n"))
		if trimmed := bytes.TrimSpace(line); len(trimmed) == 0 || trimmed[0] == '#' {
			data = rest
			continue
		}
		if !bytes.HasPrefix(line, pemBegin) {
			if len(certs) == 0 && !bytes.Contains(data, pemBegin) {
				break // Reported below as a file with no certificates.
			}
			return nil, fmt.Errorf("parsing %s %q: unexpected data outside a certificate block", field, path)
		}

		block, next := pem.Decode(data)
		if block == nil || bytes.Count(data[:len(data)-len(next)], pemBegin) != 1 {
			return nil, fmt.Errorf("parsing %s %q: damaged or truncated PEM block", field, path)
		}
		data = next
		if skip != nil && skip(block.Type) {
			continue
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("parsing %s %q: contains a non-certificate PEM block", field, path)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing %s %q: invalid certificate", field, path)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("parsing %s %q: no certificates found", field, path)
	}
	return certs, nil
}
