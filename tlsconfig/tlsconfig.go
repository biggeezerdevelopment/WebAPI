// Package tlsconfig builds client TLS configurations from file paths. It is
// transport-neutral so that any output type can share it.
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
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
		cert, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading client certificate and key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

// loadCAPool parses every certificate in the bundle. Unlike
// CertPool.AppendCertsFromPEM it fails on a corrupt entry instead of skipping
// it, so a damaged bundle cannot silently narrow the trusted set.
func loadCAPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading ca_file: %w", err)
	}

	pool := x509.NewCertPool()
	count := 0
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing ca_file %q: invalid certificate", path)
		}
		pool.AddCert(cert)
		count++
	}
	if count == 0 {
		return nil, fmt.Errorf("parsing ca_file %q: no certificates found", path)
	}
	return pool, nil
}
