package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// withoutURL removes the *url.Error wrapper that net/http adds to transport
// errors. Its text holds the method and the full URL, whose query can carry
// credentials. The cause is kept, so errors.Is and errors.As still see it.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// ErrRedirect reports a response that redirected a send which does not follow
// redirects. It fails the same way on every attempt, so it is not retried.
var ErrRedirect = errors.New("redirect not followed")

// StatusError describes a non-2xx response from label. A redirect status
// wraps ErrRedirect. Redirects that a client follows never reach it, so only
// one that a client refused, or one without a Location, does.
func StatusError(label string, code int) error {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return fmt.Errorf("unexpected status code from %s: %d (%w)", label, code, ErrRedirect)
	}
	return fmt.Errorf("unexpected status code from %s: %d", label, code)
}
