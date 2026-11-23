package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"webapi/config"
)

// maxPageBytes bounds one paginated response held in memory.
const maxPageBytes = 64 << 20

type Client struct {
	httpClient *http.Client
	pageClient *http.Client
	config     *config.WebAPI
	apiKey     string
}

func NewClient(apiConfig *config.WebAPI) *Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: apiConfig.SkipVerify,
		},
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   time.Duration(apiConfig.Timeout) * time.Second,
	}
	pageClient := &http.Client{
		Transport: tr,
		Timeout:   client.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// A validated config cannot fail here. An unvalidated one sends no key.
	apiKey, _ := apiConfig.ResolveAPIKey()

	return &Client{
		httpClient: client,
		pageClient: pageClient,
		config:     apiConfig,
		apiKey:     apiKey,
	}
}

func (c *Client) SendRequest() ([]byte, error) {
	payload, err := c.config.Body.Payload()
	if err != nil {
		return nil, fmt.Errorf("error encoding body: %v", err)
	}
	// The reader stays a nil interface when there is no payload so that the
	// request carries no body.
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequest(
		strings.ToUpper(c.config.Method),
		c.config.Endpoint,
		body,
	)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %v", err)
	}

	// Add headers
	for _, header := range c.config.Headers {
		req.Header.Set(header.Key, header.Value)
	}

	// Add parameters
	if c.config.Params != nil {
		q := req.URL.Query()
		for key, value := range c.config.Params {
			q.Add(key, value)
		}
		req.URL.RawQuery = q.Encode()
	}

	c.authorize(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error sending request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// authorize adds the configured credentials to req.
func (c *Client) authorize(req *http.Request) {
	if c.config.AuthType == "apikey" && c.apiKey != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))
	}
	if c.config.AuthType == "basic" && c.config.APIUsername != "" && c.config.APIPassword != "" {
		req.SetBasicAuth(c.config.APIUsername, c.config.APIPassword)
	}
}

// Page is one HTTP response, returned whatever its status so that callers can
// read pagination and rate-limit headers.
type Page struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// FetchPage GETs an exact URL with the configured headers and credentials.
// Redirects are not followed, so the token cannot be forwarded to another
// host. Returned errors never contain the URL.
func (c *Client) FetchPage(ctx context.Context, pageURL string) (*Page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, errors.New("error creating page request")
	}
	for _, header := range c.config.Headers {
		req.Header.Set(header.Key, header.Value)
	}
	c.authorize(req)

	resp, err := c.pageClient.Do(req)
	if err != nil {
		var netErr net.Error
		switch {
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("page request canceled: %w", context.Canceled)
		case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
			return nil, fmt.Errorf("page request timed out: %w", context.DeadlineExceeded)
		}
		return nil, errors.New("page request failed")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	if err != nil {
		return nil, errors.New("error reading page response")
	}
	if len(body) > maxPageBytes {
		return nil, fmt.Errorf("page response exceeds %d bytes", maxPageBytes)
	}
	return &Page{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}, nil
}
