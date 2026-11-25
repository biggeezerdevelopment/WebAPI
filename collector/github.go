package collector

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"webapi/api"
	"webapi/config"
)

// githubSource adapts a GitHub organization audit log. Pages are requested
// with order=asc and followed through the Link header.
type githubSource struct {
	source *config.WebAPI
}

// FirstPage builds the query. The created qualifier is rounded down to a
// whole second so it can only widen the result; exact filtering is local.
func (g *githubSource) FirstPage(floor int64, _ time.Time) (*url.URL, error) {
	u, err := url.Parse(g.source.Endpoint)
	if err != nil {
		return nil, errors.New("invalid endpoint")
	}
	q := url.Values{}
	for key, value := range g.source.Params {
		q.Set(key, value)
	}
	created := "created:>=" + time.UnixMilli(floor).UTC().Format("2006-01-02T15:04:05Z")
	if phrase := strings.TrimSpace(q.Get("phrase")); phrase != "" {
		created = phrase + " " + created
	}
	q.Set("phrase", created)
	q.Set("order", "asc")
	u.RawQuery = q.Encode()
	return u, nil
}

// ParsePage reads one response array and its rel="next" link.
func (g *githubSource) ParsePage(cur *url.URL, page *api.Page) ([]event, *url.URL, error) {
	events, err := parseEvents(page.Body)
	if err != nil {
		return nil, nil, err
	}
	next, err := nextPageURL(cur, page.Header)
	if err != nil {
		return nil, nil, err
	}
	return events, next, nil
}

// parseEvents splits a response array. An event without a usable ID or
// timestamp fails the poll, because skipping it could lose it for good.
func parseEvents(body []byte) ([]event, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return nil, errors.New("response is not a JSON array")
	}
	events := make([]event, 0, len(raw))
	for i, item := range raw {
		var fields struct {
			Timestamp  *int64  `json:"@timestamp"`
			DocumentID *string `json:"_document_id"`
		}
		if err := json.Unmarshal(item, &fields); err != nil {
			return nil, fmt.Errorf("event %d is malformed", i)
		}
		if fields.DocumentID == nil || *fields.DocumentID == "" {
			return nil, fmt.Errorf("event %d has no _document_id", i)
		}
		if fields.Timestamp == nil || *fields.Timestamp < 0 {
			return nil, fmt.Errorf("event %d has no valid @timestamp", i)
		}
		events = append(events, event{id: *fields.DocumentID, ts: *fields.Timestamp, data: item})
	}
	return events, nil
}

// nextPageURL returns the rel="next" target, or nil on the last page. The
// engine refuses a target on another origin.
func nextPageURL(current *url.URL, header http.Header) (*url.URL, error) {
	for _, value := range header.Values("Link") {
		rest := value
		for {
			start := strings.IndexByte(rest, '<')
			if start < 0 {
				break
			}
			end := strings.IndexByte(rest[start:], '>')
			if end < 0 {
				return nil, errors.New("malformed Link header")
			}
			target := rest[start+1 : start+end]
			rest = rest[start+end+1:]
			params := rest
			if next := strings.IndexByte(rest, '<'); next >= 0 {
				params = rest[:next]
			}
			if !hasRelNext(params) {
				continue
			}
			parsed, err := current.Parse(target)
			if err != nil {
				return nil, errors.New("malformed Link header")
			}
			return parsed, nil
		}
	}
	return nil, nil
}

func hasRelNext(params string) bool {
	for _, param := range strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ',' }) {
		key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "rel") {
			continue
		}
		for _, rel := range strings.Fields(strings.Trim(strings.TrimSpace(value), `"`)) {
			if strings.EqualFold(rel, "next") {
				return true
			}
		}
	}
	return false
}
