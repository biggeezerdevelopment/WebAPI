package api

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"webapi/config"
)

type LogClient struct {
	httpClient *http.Client
	config     *config.APILogRemote
}

type LogPayload struct {
	AgentID    string      `json:"agent_id"`
	AgentName  string      `json:"agent_name"`
	Timestamp  int64       `json:"timestamp"`
	Source     string      `json:"source"`
	APIResults interface{} `json:"api_results"`
}

func NewLogClient(logConfig *config.APILogRemote) *LogClient {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: logConfig.SkipVerify,
		},
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   30 * time.Second, // Consider making timeout configurable
	}

	return &LogClient{
		httpClient: client,
		config:     logConfig,
	}
}

// WithoutRedirects returns a client that treats any redirect as a failed send.
// Go turns a redirected POST into a bodyless GET, so a 2xx from the redirect
// target would otherwise report an undelivered record as delivered. The
// transport is shared, and the receiver keeps its behavior.
func (c *LogClient) WithoutRedirects() *LogClient {
	httpClient := *c.httpClient
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &LogClient{httpClient: &httpClient, config: c.config}
}

// normalizeAPIResults stops encoding/json from base64-encoding a raw response
// body. A nil body stays null, valid JSON is embedded unchanged, and anything
// else becomes a string. Values that are not []byte pass through untouched.
func normalizeAPIResults(apiResults interface{}) interface{} {
	body, ok := apiResults.([]byte)
	if !ok {
		return apiResults
	}
	if body == nil {
		return nil
	}
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	return string(body)
}

// EncodeLogPayload renders the JSON envelope shared by all HTTP log outputs.
func EncodeLogPayload(agentID, agentName, source string, timestamp int64, apiResults interface{}) ([]byte, error) {
	return json.Marshal(LogPayload{
		AgentID:    agentID,
		AgentName:  agentName,
		Timestamp:  timestamp,
		Source:     source,
		APIResults: normalizeAPIResults(apiResults),
	})
}

func (c *LogClient) SendLog(agentID, agentName, source string, apiResults interface{}) error {
	jsonData, err := EncodeLogPayload(agentID, agentName, source, time.Now().Unix(), apiResults)
	if err != nil {
		return fmt.Errorf("error marshaling log payload: %v", err)
	}

	req, err := http.NewRequest(
		strings.ToUpper(c.config.APIMethod),
		c.config.APIURL,
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}

	// Add headers
	for _, header := range c.config.APIHeaders {
		req.Header.Set(header.Key, header.Value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("error sending log: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code from log API: %d", resp.StatusCode)
	}

	return nil
}
