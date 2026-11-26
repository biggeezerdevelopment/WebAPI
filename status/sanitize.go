// Package status tracks poll and delivery outcomes for the heartbeat.
package status

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxErrorBytes bounds error text stored for a heartbeat.
const MaxErrorBytes = 512

var urlPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)

// Sanitize renders err for a heartbeat. It removes URL queries, fragments and
// userinfo, which can carry credentials, replaces control characters with
// spaces, and truncates to MaxErrorBytes. A nil error gives "".
func Sanitize(err error) string {
	if err == nil {
		return ""
	}
	s := urlPattern.ReplaceAllStringFunc(err.Error(), redactURL)
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return truncate(s, MaxErrorBytes)
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		if i := strings.Index(raw, "://"); i >= 0 {
			return raw[:i+3] + "[redacted]"
		}
		return "[redacted]"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const suffix = "…"
	cut := limit - len(suffix)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}
