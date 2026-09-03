package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
)

// DefaultMaxBodyBytes is the default maximum request body size (32MB).
const DefaultMaxBodyBytes = 32 * 1024 * 1024

// ErrRequestBodyTooLarge is returned when the request body exceeds the configured maximum byte limit.
var ErrRequestBodyTooLarge = errors.New("request body exceeds maximum allowed size")

// BufferedRequest stores the request body in memory so it can be replayed
// across multiple upstream attempts during failover.
type BufferedRequest struct {
	Body []byte
}

// NewBufferedRequest reads and stores the request body in memory using DefaultMaxBodyBytes,
// then restores the original request body for subsequent reads.
func NewBufferedRequest(req *http.Request) (*BufferedRequest, error) {
	return NewBufferedRequestWithLimit(req, DefaultMaxBodyBytes)
}

// NewBufferedRequestWithLimit reads incoming request body with an enforced upper limit.
// If the body exceeds maxBytes, ErrRequestBodyTooLarge is returned.
// A maxBytes <= 0 defaults to DefaultMaxBodyBytes.
func NewBufferedRequestWithLimit(req *http.Request, maxBytes int64) (*BufferedRequest, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBodyBytes
	}

	if req == nil || req.Body == nil || req.Body == http.NoBody {
		return &BufferedRequest{Body: nil}, nil
	}

	// Read up to maxBytes + 1 to detect whether the payload overflowed the cap
	limitReader := io.LimitReader(req.Body, maxBytes+1)
	body, err := io.ReadAll(limitReader)
	if err != nil {
		_ = req.Body.Close()
		return nil, err
	}
	_ = req.Body.Close()

	if int64(len(body)) > maxBytes {
		return nil, ErrRequestBodyTooLarge
	}

	// Reset original request body for safety
	req.Body = io.NopCloser(bytes.NewReader(body))

	return &BufferedRequest{Body: body}, nil
}

// NewReader returns a new io.ReadCloser over the buffered bytes.
// If the stored body is nil, http.NoBody is returned.
func (b *BufferedRequest) NewReader() io.ReadCloser {
	if b == nil || b.Body == nil {
		return http.NoBody
	}
	return io.NopCloser(bytes.NewReader(b.Body))
}

// ResetBody creates a new io.ReadCloser for the given request using the stored bytes
// and resets ContentLength.
func (b *BufferedRequest) ResetBody(req *http.Request) {
	if b == nil || b.Body == nil {
		req.Body = http.NoBody
		req.ContentLength = 0
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(b.Body))
	req.ContentLength = int64(len(b.Body))
}

// Size returns the size in bytes of the buffered body.
func (b *BufferedRequest) Size() int {
	if b == nil || b.Body == nil {
		return 0
	}
	return len(b.Body)
}

// Bytes returns the raw buffered slice.
func (b *BufferedRequest) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.Body
}
