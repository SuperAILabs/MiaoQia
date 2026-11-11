package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxResponseBytes caps how much of a vendor response body an adapter reads.
const maxResponseBytes = 8 << 20

// vendorHTTP is the transport shared by the vendor adapters.
type vendorHTTP struct {
	client *http.Client
	vendor string
}

// StatusError is a non-success HTTP response from a vendor. Transient statuses wrap ErrUnavailable
// so the gateway fails over; the body excerpt never contains request content.
type StatusError struct {
	Vendor    string
	Status    int
	Body      string
	transient bool
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Vendor, e.Status, e.Body)
}

func (e *StatusError) Unwrap() error {
	if e.transient {
		return ErrUnavailable
	}
	return nil
}

// transientStatus reports whether a status means the endpoint is temporarily unavailable:
// timeouts, rate limits (429), server errors and Anthropic's 529 overloaded.
func transientStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}

// post sends a JSON request and decodes a 2xx JSON response into out.
// Network failures and transient statuses return errors wrapping ErrUnavailable; a context
// cancellation or deadline is returned as the context error so the gateway can tell them apart.
// Other 4xx statuses and undecodable 2xx bodies are non-transient errors.
func (h vendorHTTP) post(ctx context.Context, url string, headers map[string]string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%s: encode request: %w", h.vendor, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", h.vendor, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return h.transportErr(ctx, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return h.transportErr(ctx, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Vendor: h.vendor, Status: resp.StatusCode, Body: excerpt(data), transient: transientStatus(resp.StatusCode)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: decode response: %w", h.vendor, err)
	}
	return nil
}

func (h vendorHTTP) transportErr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", h.vendor, ctxErr)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", h.vendor, err)
	}
	return fmt.Errorf("%s: %w: %v", h.vendor, ErrUnavailable, err)
}

func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 512 {
		s = s[:512] + "…"
	}
	return s
}
