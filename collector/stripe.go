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

// stripeMaxLimit is Stripe's largest page size, used when params.limit is
// omitted.
const stripeMaxLimit = 100

// stripeSource adapts the Stripe Activity Logs API
// (GET /v2/iam/activity_logs). The API has no time filter, so every window
// walks the list from its oldest record and the engine drops what is older
// than the checkpoint. The list is in record-ID order, where created can step
// back by a second, so ts is the running maximum of created: events stay
// non-decreasing, and the same record gets the same ts on every walk.
//
// The engine drives one window at a time per collector, which makes the
// per-window state below safe.
type stripeSource struct {
	source  *config.WebAPI
	limit   int
	cutoff  int64 // ms; the walk ends at the first record created after it
	maxSeen int64 // running maximum of created in this window, in ms
}

// stripeRecord is one parsed list entry. data is delivered unchanged.
type stripeRecord struct {
	id      string
	created int64 // ms since the epoch
	data    []byte
}

// FirstPage requests the oldest page. floor is not sent, because the API
// cannot filter by time. The settle cutoff is fixed here for the window.
func (s *stripeSource) FirstPage(_ int64, now time.Time) (*url.URL, error) {
	u, err := url.Parse(s.source.Endpoint)
	if err != nil {
		return nil, errors.New("invalid endpoint")
	}
	limit := stripeMaxLimit
	if v, ok := s.source.Params["limit"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > stripeMaxLimit {
			return nil, errors.New("invalid params.limit")
		}
		limit = n
	}
	settle := config.DefaultSettleMinutes * time.Minute
	if s.source.Collector != nil {
		settle = s.source.Collector.SettleDelay()
	}
	*s = stripeSource{source: s.source, limit: limit, cutoff: now.Add(-settle).UnixMilli()}

	q := url.Values{}
	for key, value := range s.source.Params {
		q.Set(key, value)
	}
	q.Set("limit", strconv.Itoa(limit))
	u.RawQuery = q.Encode()
	return u, nil
}

// ParsePage clamps each record's ts to the running maximum and ends the
// window at the settle cutoff, on a short or empty page, or when there is no
// next_page_url. Stripe returns next_page_url on every page, even the last,
// and as a path relative to the API host.
func (s *stripeSource) ParsePage(cur *url.URL, page *api.Page) ([]event, *url.URL, error) {
	records, next, err := parseStripePage(page.Body)
	if err != nil {
		return nil, nil, err
	}
	events := make([]event, 0, len(records))
	for _, r := range records {
		if r.created > s.cutoff {
			// Too recent to be settled. The next window reads it again.
			return events, nil, nil
		}
		s.maxSeen = max(s.maxSeen, r.created)
		events = append(events, event{id: r.id, ts: s.maxSeen, data: r.data})
	}
	if len(records) < s.limit || next == "" {
		return events, nil, nil
	}
	ref, err := url.Parse(next)
	if err != nil {
		return nil, nil, errors.New("next_page_url is not a valid URL")
	}
	return events, cur.ResolveReference(ref), nil
}

// parseStripePage splits a list response into records and returns
// next_page_url. A record without a usable id or created fails the poll,
// because skipping it could lose it for good.
func parseStripePage(body []byte) ([]stripeRecord, string, error) {
	var resp struct {
		Data        json.RawMessage `json:"data"`
		NextPageURL *string         `json:"next_page_url"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "", errors.New("response is not the expected JSON object")
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(resp.Data, &raw); err != nil || raw == nil {
		return nil, "", errors.New("response data is not a JSON array")
	}
	records := make([]stripeRecord, 0, len(raw))
	for i, item := range raw {
		var fields struct {
			ID      *string `json:"id"`
			Created *string `json:"created"`
		}
		if err := json.Unmarshal(item, &fields); err != nil {
			return nil, "", fmt.Errorf("record %d is malformed", i)
		}
		if fields.ID == nil || *fields.ID == "" {
			return nil, "", fmt.Errorf("record %d has no id", i)
		}
		if fields.Created == nil {
			return nil, "", fmt.Errorf("record %d has no created", i)
		}
		t, err := time.Parse(time.RFC3339, *fields.Created)
		if err != nil || t.UnixMilli() < 0 {
			return nil, "", fmt.Errorf("record %d has no valid created", i)
		}
		records = append(records, stripeRecord{id: *fields.ID, created: t.UnixMilli(), data: item})
	}
	next := ""
	if resp.NextPageURL != nil {
		next = *resp.NextPageURL
	}
	return records, next, nil
}
