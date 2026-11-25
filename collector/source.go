package collector

import (
	"fmt"
	"net/url"
	"time"

	"webapi/api"
	"webapi/config"
)

// Source adapts one audit-log API to the engine. Across the pages of one
// window it must yield events in non-decreasing timestamp order, so that a
// truncated fetch is a safe prefix.
type Source interface {
	// FirstPage builds the first request of a window starting at floorMS.
	FirstPage(floorMS int64, now time.Time) (*url.URL, error)
	// ParsePage returns the page's events and the next page URL, or nil when
	// the window is complete.
	ParsePage(cur *url.URL, page *api.Page) ([]event, *url.URL, error)
}

// event is one record from a Source. data is delivered unchanged.
type event struct {
	id   string
	ts   int64 // milliseconds since the epoch
	data []byte
}

// sourceFor picks the adapter for the entry's collector.type.
func sourceFor(w *config.WebAPI) (Source, error) {
	if w.Collector == nil {
		return nil, fmt.Errorf("source %q has no collector", w.Name)
	}
	switch w.Collector.Type {
	case config.CollectorTypeGitHubAudit:
		return &githubSource{source: w}, nil
	default:
		return nil, fmt.Errorf("unknown collector.type %q", w.Collector.Type)
	}
}
