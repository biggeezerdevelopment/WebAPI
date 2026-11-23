package api

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"webapi/config"
)

type Client struct {
	httpClient *http.Client
	config     *config.WebAPI
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

	return &Client{
		httpClient: client,
		config:     apiConfig,
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

	// Add authentication if specified
	if c.config.AuthType == "apikey" && c.config.APIKey != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.config.APIKey))
	}
	if c.config.AuthType == "basic" && c.config.APIUsername != "" && c.config.APIPassword != "" {
		req.SetBasicAuth(c.config.APIUsername, c.config.APIPassword)
	}

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
