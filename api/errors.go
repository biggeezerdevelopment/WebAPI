package api

import (
	"errors"
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
