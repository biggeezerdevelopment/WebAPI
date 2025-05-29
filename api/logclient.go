package api

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
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

func (c *LogClient) SendLog(agentID, agentName, source string, apiResults interface{}) error {
	payload := LogPayload{
		AgentID:    agentID,
		AgentName:  agentName,
		Timestamp:  time.Now().Unix(),
		Source:     source,
		APIResults: apiResults,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("error marshaling log payload: %v", err)
	}

	req, err := http.NewRequest(
		c.config.APIMethod,
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
