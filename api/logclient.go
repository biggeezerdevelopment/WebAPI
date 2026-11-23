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
			InsecureSkipVerify: true, // Consider making this configurable
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

func (c *LogClient) SendLog(agentID, agentName, source string, apiResults interface{}) error {
	payload := LogPayload{
		AgentID:    agentID,
		AgentName:  agentName,
		Timestamp:  time.Now().Unix(),
		Source:     source,
		APIResults: normalizeAPIResults(apiResults),
	}

	jsonData, err := json.Marshal(payload)
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
