package output

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"webapi/api"
	"webapi/config"
	"webapi/tlsconfig"
)

// maxDrainBytes limits how much of a response is read to reuse the connection.
const maxDrainBytes = 1 << 20

type resolvedHeader struct {
	key   string
	value string
}

// Webhook sends each record as one HTTP request.
type Webhook struct {
	client    *http.Client
	transport *http.Transport
	url       string
	method    string
	headers   []resolvedHeader
}

// NewWebhook builds a webhook from a validated configuration entry.
func NewWebhook(cfg *config.LogOutput) (*Webhook, error) {
	tlsCfg, err := tlsconfig.Build(cfg.TLS)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}

	headers := make([]resolvedHeader, 0, len(cfg.Headers))
	for _, header := range cfg.Headers {
		value, err := header.Resolve()
		if err != nil {
			return nil, err
		}
		headers = append(headers, resolvedHeader{key: header.Key, value: value})
	}

	transport := &http.Transport{
		TLSClientConfig: tlsCfg,
		IdleConnTimeout: 90 * time.Second,
	}
	return &Webhook{
		client: &http.Client{
			Transport: transport,
			Timeout:   time.Duration(cfg.Timeout) * time.Second,
			// Redirects are not followed: they would turn POST into GET and
			// could forward custom auth headers to another host.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		transport: transport,
		url:       cfg.URL,
		method:    strings.ToUpper(cfg.Method),
		headers:   headers,
	}, nil
}

// Send posts one record. Returned errors never contain the URL.
func (w *Webhook) Send(ctx context.Context, rec Record) error {
	body, err := api.EncodeLogPayload(rec.AgentID, rec.AgentName, rec.Source, rec.Timestamp.Unix(), rec.Data)
	if err != nil {
		return fmt.Errorf("error marshaling log payload: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, w.method, w.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("error creating webhook request")
	}
	for _, header := range w.headers {
		req.Header.Set(header.key, header.value)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return sanitizeTransportError("webhook", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code from webhook: %d", resp.StatusCode)
	}
	return nil
}

// Close releases idle connections.
func (w *Webhook) Close() error {
	w.transport.CloseIdleConnections()
	return nil
}

// sanitizeTransportError maps a client error to a fixed description. The
// originals can embed the URL, host, or address, so their text is discarded.
func sanitizeTransportError(label string, err error) error {
	var (
		netErr      net.Error
		dnsErr      *net.DNSError
		opErr       *net.OpError
		verifyErr   *tls.CertificateVerificationError
		unknownErr  x509.UnknownAuthorityError
		hostnameErr x509.HostnameError
		invalidErr  x509.CertificateInvalidError
		recordErr   tls.RecordHeaderError
	)
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s request canceled: %w", label, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s request timed out: %w", label, context.DeadlineExceeded)
	case errors.As(err, &verifyErr), errors.As(err, &unknownErr),
		errors.As(err, &hostnameErr), errors.As(err, &invalidErr):
		return errors.New(label + " TLS certificate verification failed")
	case errors.As(err, &recordErr):
		return errors.New(label + " TLS handshake failed")
	case errors.As(err, &dnsErr):
		return errors.New(label + " DNS lookup failed")
	case errors.As(err, &netErr) && netErr.Timeout():
		return fmt.Errorf("%s request timed out: %w", label, context.DeadlineExceeded)
	case errors.As(err, &opErr):
		if opErr.Op == "remote error" {
			return errors.New(label + " TLS handshake rejected by server")
		}
		return errors.New(label + " connection failed")
	default:
		return errors.New(label + " request failed")
	}
}
