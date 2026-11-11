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

// StatusError is a non-success HTTP response from a vendor. Failover statuses wrap ErrUnavailable
// so the gateway tries the next endpoint; the body excerpt is the vendor's error message.
type StatusError struct {
	Vendor   string
	Status   int
	Body     string
	failover bool
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Vendor, e.Status, e.Body)
}

func (e *StatusError) Unwrap() error {
	if e.failover {
		return ErrUnavailable
	}
	return nil
}

// failoverStatus reports whether a status means this endpoint cannot serve the call while another one may:
// temporary unavailability (408, 429, 5xx including Anthropic's 529 overloaded) and endpoint-specific
// configuration faults (401/403 credentials, 404 unknown model or path). Other 4xx describe the request
// itself and would fail on every endpoint.
func failoverStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return code >= 500
}

// post sends a JSON request and decodes a 2xx JSON response into out.
// Network failures and failover statuses return errors wrapping ErrUnavailable; a context
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
		return &StatusError{Vendor: h.vendor, Status: resp.StatusCode, Body: excerpt(data), failover: failoverStatus(resp.StatusCode)}
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
