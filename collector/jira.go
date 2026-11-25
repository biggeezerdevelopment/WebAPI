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
	// jiraDefaultLimit is the page size used when params.limit is omitted.
	jiraDefaultLimit = 1000
	// jiraSettleDelay keeps the window's end behind now, so records still
	// being written are left to the next window.
	jiraSettleDelay = 60 * time.Second
	// jiraTimeLayout is the from/to format sent to the API, always in UTC.
	jiraTimeLayout = "2006-01-02T15:04:05.000-0700"
	// jiraCreatedLayout parses created, which uses a +0000-style offset. The
	// fraction is optional when parsing.
	jiraCreatedLayout = "2006-01-02T15:04:05.999999999-0700"
)

// jiraSource adapts Jira Cloud audit records. The API does not document its
// sort order (in practice newest first) and pages only by offset, so each
// window fixes from and to, probes the total, and reads pages from the last
// offset back to 0. Reversing each page yields events oldest first.
//
// The adapter tracks the window's offsets itself. The engine drives one
// window at a time per collector, which is what makes this state safe.
type jiraSource struct {
	source *config.WebAPI

	limit    int  // page size for data pages
	probed   bool // the probe response has been read
	total    int  // total reported by the probe
	expected int  // offset of the page the engine should request next
	// newestSeen is the created time of the record at the lowest offset read
	// so far. Every record at a lower offset must be at least this new.
	newestSeen int64
	haveSeen   bool
	pageEnd    int     // offset of the data page read last
	edgeID     string  // id of the record at pageEnd
	probe      []event // records from the probe, reused for page 0 when equal
}

// FirstPage fixes the window [floor, now-60s] and returns the probe request.
func (j *jiraSource) FirstPage(floor int64, now time.Time) (*url.URL, error) {
	u, err := url.Parse(j.source.Endpoint)
	if err != nil {
		return nil, errors.New("invalid endpoint")
	}
	limit := jiraDefaultLimit
	if v, ok := j.source.Params["limit"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			return nil, errors.New("invalid params.limit")
		}
		limit = n
	}
	*j = jiraSource{source: j.source, limit: limit}

	to := now.Add(-jiraSettleDelay).UTC()
	from := time.UnixMilli(floor).UTC().Truncate(time.Second)
	if from.After(to) {
		from = to
	}
	q := url.Values{}
	for key, value := range j.source.Params {
		q.Set(key, value)
	}
	q.Set("from", from.Format(jiraTimeLayout))
	q.Set("to", to.Format(jiraTimeLayout))
	q.Set("offset", "0")
	q.Set("limit", "1")
	u.RawQuery = q.Encode()
	return u, nil
}

// ParsePage handles the probe and each data page, checking that offsets,
// total and order are what this window expects.
func (j *jiraSource) ParsePage(cur *url.URL, page *api.Page) ([]event, *url.URL, error) {
	q := cur.Query()
	offset, err := strconv.Atoi(q.Get("offset"))
	if err != nil || offset != j.expected {
		return nil, nil, errors.New("jira page offset is not the one this window expects")
	}
	resp, err := parseJiraPage(page.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.offset != nil && *resp.offset != offset {
		return nil, nil, fmt.Errorf("jira returned offset %d for a request at offset %d", *resp.offset, offset)
	}

	if !j.probed {
		if q.Get("limit") != "1" {
			return nil, nil, errors.New("jira probe request is not the one this window expects")
		}
		if resp.total < 0 {
			return nil, nil, errors.New("jira returned a negative total")
		}
		if err := checkJiraCount(resp.records, min(1, resp.total)); err != nil {
			return nil, nil, err
		}
		j.probed, j.total, j.probe = true, resp.total, resp.records
		if j.total == 0 {
			return nil, nil, nil
		}
		return j.dataPage(cur, max(j.total-j.limit, 0))
	}

	if resp.total != j.total {
		return nil, nil, fmt.Errorf("jira total changed from %d to %d during the window", j.total, resp.total)
	}
	if limit, err := strconv.Atoi(q.Get("limit")); err != nil || limit != j.limit {
		return nil, nil, errors.New("jira page limit is not the one this window expects")
	}
	if err := checkJiraCount(resp.records, min(j.limit, j.total-offset)); err != nil {
		return nil, nil, err
	}
	// The page at offset 0 can reach past the start of the page read before
	// it. The overlapping records were already read; the first of them must
	// be the record seen at that offset, or the records shifted.
	keep := len(resp.records)
	if j.haveSeen && offset+keep > j.pageEnd {
		keep = j.pageEnd - offset
		if resp.records[keep].id != j.edgeID {
			return nil, nil, errors.New("jira records shifted between pages")
		}
	}
	if err := checkJiraOrder(resp.records); err != nil {
		return nil, nil, err
	}
	// Only the new records continue the window's order; the overlap was
	// checked when it was first read and matched by id above.
	events, err := j.accept(resp.records[:keep])
	if err != nil {
		return nil, nil, err
	}
	j.pageEnd, j.edgeID = offset, resp.records[0].id
	if offset == 0 {
		return events, nil, nil
	}
	more, next, err := j.dataPage(cur, max(offset-j.limit, 0))
	if err != nil {
		return nil, nil, err
	}
	return append(events, more...), next, nil
}

// dataPage returns the request for the page at offset. The page at offset 0
// with limit 1 is the probe itself, and fetching it again would repeat a URL,
// so the probe's records are used instead. That holds because to is fixed and
// later pages confirmed the total did not change.
func (j *jiraSource) dataPage(cur *url.URL, offset int) ([]event, *url.URL, error) {
	j.expected = offset
	if offset == 0 && (j.limit == 1 || j.total == 1) {
		events, err := j.accept(j.probe)
		return events, nil, err
	}
	next := *cur
	q := next.Query()
	q.Set("offset", strconv.Itoa(offset))
	q.Set("limit", strconv.Itoa(j.limit))
	next.RawQuery = q.Encode()
	return nil, &next, nil
}

// accept checks that a newest-first page continues the window's order, then
// returns it oldest first.
func (j *jiraSource) accept(records []event) ([]event, error) {
	if err := checkJiraOrder(records); err != nil {
		return nil, err
	}
	if n := len(records); n > 0 {
		if j.haveSeen && records[n-1].ts < j.newestSeen {
			return nil, errors.New("jira audit records are not newest first across pages: the API order is not as expected")
		}
		j.newestSeen, j.haveSeen = records[0].ts, true
	}
	out := make([]event, len(records))
	for i, ev := range records {
		out[len(records)-1-i] = ev
	}
	return out, nil
}

// checkJiraOrder rejects a page that is not newest first (non-increasing
// created).
func checkJiraOrder(records []event) error {
	for i := 1; i < len(records); i++ {
		if records[i].ts > records[i-1].ts {
			return errors.New("jira audit records are not newest first: the API order is not as expected")
		}
	}
	return nil
}

// checkJiraCount rejects a page that would leave a gap or overlap.
func checkJiraCount(records []event, want int) error {
	if len(records) != want {
		return fmt.Errorf("jira page has %d records, want %d", len(records), want)
	}
	return nil
}

type jiraPage struct {
	offset  *int
	total   int
	records []event // in server order
}

// parseJiraPage reads one auditing/record response. A record without a usable
// id or created fails the poll, because skipping it could lose it for good.
func parseJiraPage(body []byte) (*jiraPage, error) {
	var resp struct {
		Offset  *int            `json:"offset"`
		Total   *int            `json:"total"`
		Records json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Total == nil {
		return nil, errors.New("response is not the expected JSON object with a total")
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(resp.Records, &raw); err != nil || raw == nil {
		return nil, errors.New("response records is not a JSON array")
	}
	records := make([]event, 0, len(raw))
	for i, item := range raw {
		var fields struct {
			ID      json.RawMessage `json:"id"`
			Created *string         `json:"created"`
		}
		if err := json.Unmarshal(item, &fields); err != nil {
			return nil, fmt.Errorf("record %d is malformed", i)
		}
		id, err := strconv.ParseInt(string(fields.ID), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("record %d has no valid integer id", i)
		}
		if fields.Created == nil {
			return nil, fmt.Errorf("record %d has no created", i)
		}
		ts, ok := parseJiraTime(*fields.Created)
		if !ok {
			return nil, fmt.Errorf("record %d has no valid created", i)
		}
		records = append(records, event{id: strconv.FormatInt(id, 10), ts: ts, data: item})
	}
	return &jiraPage{offset: resp.Offset, total: *resp.Total, records: records}, nil
}

// parseJiraTime accepts the +0000-style offset Jira uses and RFC 3339.
func parseJiraTime(s string) (int64, bool) {
	for _, layout := range []string{jiraCreatedLayout, time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil && t.UnixMilli() >= 0 {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}
