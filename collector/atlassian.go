package collector

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"webapi/api"
	"webapi/config"
)

const (
	// atlassianDefaultLimit is the page size used when params.limit is omitted.
	atlassianDefaultLimit = 500
	// atlassianOverlapMS widens the first request of a window. The API orders
	// by processing time, and the docs do not say which timestamp from filters
	// on; an event's time can precede its processedAt. The engine filters
	// exactly by checkpoint, so the overlap only costs re-reading.
	atlassianOverlapMS = int64(time.Hour / time.Millisecond)
)

// atlassianSource adapts an Atlassian organization events-stream. Pages are
// requested with sortOrder=asc and followed through meta.next.
type atlassianSource struct {
	source *config.WebAPI
}

// FirstPage builds the query for a window starting at floor.
func (a *atlassianSource) FirstPage(floor int64, _ time.Time) (*url.URL, error) {
	u, err := url.Parse(a.source.Endpoint)
	if err != nil {
		return nil, errors.New("invalid endpoint")
	}
	q := url.Values{}
	for key, value := range a.source.Params {
		q.Set(key, value)
	}
	if q.Get("limit") == "" {
		q.Set("limit", strconv.Itoa(atlassianDefaultLimit))
	}
	from := max(floor-atlassianOverlapMS, 0)
	q.Set("from", strconv.FormatInt(from, 10))
	q.Set("sortOrder", "asc")
	u.RawQuery = q.Encode()
	return u, nil
}

// ParsePage reads one events-stream response. meta.next can be present on the
// last page, since the API offers it for polling, so a page shorter than limit
// also ends the window.
func (a *atlassianSource) ParsePage(cur *url.URL, page *api.Page) ([]event, *url.URL, error) {
	events, next, err := parseAtlassianPage(page.Body)
	if err != nil {
		return nil, nil, err
	}
	q := cur.Query()
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit < 1 {
		limit = atlassianDefaultLimit
	}
	if next == "" || len(events) < limit {
		return events, nil, nil
	}
	nextURL := *cur
	q.Set("cursor", next)
	nextURL.RawQuery = q.Encode()
	return events, &nextURL, nil
}

// parseAtlassianPage splits a response object into events and returns
// meta.next. An event without a usable ID or processedAt fails the poll,
// because skipping it could lose it for good.
func parseAtlassianPage(body []byte) ([]event, string, error) {
	var resp struct {
		Data json.RawMessage `json:"data"`
		Meta struct {
			Next string `json:"next"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "", errors.New("response is not the expected JSON object")
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(resp.Data, &raw); err != nil || raw == nil {
		return nil, "", errors.New("response data is not a JSON array")
	}
	events := make([]event, 0, len(raw))
	var last int64
	for i, item := range raw {
		var fields struct {
			ID         *string `json:"id"`
			Attributes struct {
				ProcessedAt *string `json:"processedAt"`
			} `json:"attributes"`
		}
		if err := json.Unmarshal(item, &fields); err != nil {
			return nil, "", fmt.Errorf("event %d is malformed", i)
		}
		if fields.ID == nil || *fields.ID == "" {
			return nil, "", fmt.Errorf("event %d has no id", i)
		}
		if fields.Attributes.ProcessedAt == nil {
			return nil, "", fmt.Errorf("event %d has no attributes.processedAt", i)
		}
		t, err := time.Parse(time.RFC3339, *fields.Attributes.ProcessedAt)
		if err != nil || t.UnixMilli() < 0 {
			return nil, "", fmt.Errorf("event %d has no valid attributes.processedAt", i)
		}
		ts := t.UnixMilli()
		if i > 0 && ts < last {
			return nil, "", fmt.Errorf("event %d is out of processedAt order", i)
		}
		last = ts
		events = append(events, event{id: *fields.ID, ts: ts, data: item})
	}
	return events, resp.Meta.Next, nil
}
