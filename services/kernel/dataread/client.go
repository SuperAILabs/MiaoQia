// Package dataread is the Go client for the data-api internal read transport.
// It contains transport policy only; callers still own the domain mapping and
// must fail closed when a response is incomplete or unavailable.
package dataread

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
)

const maxResponseBytes = 4 << 20

// Error is the stable error returned by data-api.  A 5xx response is also
// marked transient so binding.Invoke can retry idempotent reads.
type Error struct {
	Status int
	Code   string
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("data-api: %s", e.Code)
	}
	return fmt.Sprintf("data-api: %s: %s", e.Code, e.Detail)
}

// Basis is the data view used by all read operations.
type Basis struct {
	View           string     `json:"view"`
	RecordedCutoff *time.Time `json:"recorded_cutoff"`
}

// Client performs the five closed data-api operations. Responses are decoded
// into caller-owned values so each subsystem can enforce its own schema.
type Client struct {
	base *url.URL
	http *http.Client
}

// New creates an authenticated HTTPS client using the process mTLS settings.
func New(baseURL string) (*Client, error) {
	base, err := parseBase(baseURL, false)
	if err != nil {
		return nil, err
	}
	transport, err := transportsecurity.Client()
	if err != nil {
		return nil, err
	}
	return &Client{base: base, http: &http.Client{
		Transport:     binding.Transport{Base: transport},
		CheckRedirect: refuseRedirect,
	}}, nil
}

// NewWithHTTPClient is intended for contract tests. Production code should use
// New so the shared mTLS and context transport is always installed.
func NewWithHTTPClient(baseURL string, client *http.Client) (*Client, error) {
	base, err := parseBase(baseURL, true)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("data-api HTTP client is required")
	}
	return &Client{base: base, http: client}, nil
}

func parseBase(raw string, allowHTTP bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("data-api requires an absolute endpoint without credentials or query")
	}
	if !allowHTTP && !transportsecurity.Development() && u.Scheme != "https" {
		return nil, errors.New("data-api requires authenticated HTTPS")
	}
	return u, nil
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return errors.New("data-api refuses redirects")
}

func (c *Client) call(ctx context.Context, operation, path string, request any, response any) error {
	_, err := binding.Invoke(ctx, binding.Remote, binding.Op{
		Subsystem: "data-api", Name: operation, Idempotent: true,
	}, func(ctx context.Context) (struct{}, error) {
		body, err := json.Marshal(request)
		if err != nil {
			return struct{}{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base.ResolveReference(&url.URL{Path: strings.TrimSuffix(c.base.Path, "/") + path}).String(), bytes.NewReader(body))
		if err != nil {
			return struct{}{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := c.http.Do(req)
		if err != nil {
			return struct{}{}, err
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
		if err != nil {
			return struct{}{}, err
		}
		if len(raw) > maxResponseBytes {
			return struct{}{}, errors.New("data-api response exceeds limit")
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return struct{}{}, responseError(res.StatusCode, raw)
		}
		if response == nil || len(bytes.TrimSpace(raw)) == 0 {
			return struct{}{}, nil
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		if err := decoder.Decode(response); err != nil {
			return struct{}{}, fmt.Errorf("data-api response: %w", err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return struct{}{}, errors.New("data-api response contains multiple JSON values")
		}
		return struct{}{}, nil
	})
	return err
}

func responseError(status int, raw []byte) error {
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(raw, &problem)
	if problem.Code == "" {
		problem.Code = "unavailable"
	}
	err := &Error{Status: status, Code: problem.Code, Detail: problem.Detail}
	if status >= 500 {
		return fmt.Errorf("%w: %w", binding.ErrTransient, err)
	}
	return err
}

func (c *Client) GetFact(ctx context.Context, factID string, basis Basis, version *int, out any) error {
	return c.call(ctx, "GetFact", "/internal/v1/data/get-fact", map[string]any{
		"fact_id": factID, "basis": basis, "version": version,
	}, out)
}

func (c *Client) FactHistory(ctx context.Context, factID string, basis Basis, after, limit int, out any) error {
	return c.call(ctx, "FactHistory", "/internal/v1/data/fact-history", map[string]any{
		"fact_id": factID, "basis": basis, "after": after, "limit": limit,
	}, out)
}

func (c *Client) QueryFacts(ctx context.Context, domain, objectType string, basis Basis, after string, limit int, out any) error {
	return c.call(ctx, "QueryFacts", "/internal/v1/data/query-facts", map[string]any{
		"info_domain": domain, "object_type": objectType, "basis": basis, "after": after, "limit": limit,
	}, out)
}

func (c *Client) BidResultUnits(ctx context.Context, factID string, basis Basis, version *int, out any) error {
	return c.call(ctx, "BidResultUnits", "/internal/v1/data/bid-result-units", map[string]any{
		"fact_id": factID, "basis": basis, "version": version,
	}, out)
}

func (c *Client) ResolveEvidence(ctx context.Context, evidenceRef string, out any) error {
	return c.call(ctx, "ResolveEvidence", "/internal/v1/data/resolve-evidence", map[string]any{
		"evidence_ref": evidenceRef,
	}, out)
}

// ContextHeader exposes the canonical context encoding for tests and custom
// transports. Normal callers get it automatically from binding.Transport.
func ContextHeader(ctx context.Context) (string, error) {
	c, ok := reqctx.From(ctx)
	if !ok {
		return "", reqctx.ErrMissing
	}
	if err := c.Validate(); err != nil {
		return "", err
	}
	return reqctx.Encode(c), nil
}
