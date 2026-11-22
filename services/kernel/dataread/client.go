// Package dataread binds the data-api owner contract through the shared call pipeline.
package dataread

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	dataapi "github.com/superailabs/miaoqia/services/contracts/apis/data-api"
	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
)

const (
	timeout          = 5 * time.Second
	maxResponseBytes = 65536
	serverPeer       = "spiffe://miaoqia/service/data-api"
)

// Error preserves the HTTP failure classification for existing consumers.
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

// Reader exposes the owner DTOs independently of the binding mode.
type Reader interface {
	ReadFact(context.Context, dataapi.FactRequest) (*dataapi.FactRead, error)
	ReadFactHistory(context.Context, dataapi.FactHistoryRequest) (*dataapi.FactHistoryPage, error)
	ReadFacts(context.Context, dataapi.QueryFactsRequest) (*dataapi.FactPage, error)
	ReadBidResultUnits(context.Context, dataapi.FactRequest) (*dataapi.BidResultPage, error)
	ReadEvidence(context.Context, dataapi.EvidenceRequest) (*dataapi.EvidenceRead, error)
}

type Client struct {
	invoker dataapi.Invoker
	mode    binding.Mode
}

var _ Reader = (*Client)(nil)

// New requires TLS 1.3, client credentials, CA and hostname verification, and the data-api server identity.
func New(baseURL string) (*Client, error) {
	if err := validateEndpoint(baseURL, false); err != nil {
		return nil, err
	}
	tls, err := transportsecurity.Config("MIAOQIA_INTERNAL_TLS", false)
	if err != nil {
		return nil, err
	}
	tls.VerifyConnection = verifyPeer
	return newRemote(baseURL, &http.Client{
		Transport: binding.Transport{Base: &http.Transport{TLSClientConfig: tls}},
	})
}

// NewWithHTTPClient binds an explicitly supplied fixture transport.
func NewWithHTTPClient(baseURL string, client *http.Client) (*Client, error) {
	if err := validateEndpoint(baseURL, true); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("data-api HTTP client is required")
	}
	return newRemote(baseURL, client)
}

// NewLocal binds an explicitly supplied same-process owner implementation.
// The invoker must perform the same current-source authorization as the remote owner.
func NewLocal(owner dataapi.Invoker) (*Client, error) {
	if owner == nil {
		return nil, errors.New("data-api local owner is required")
	}
	return &Client{invoker: owner, mode: binding.Local}, nil
}

func newRemote(baseURL string, client *http.Client) (*Client, error) {
	httpClient := *client
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("data-api refuses redirects")
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	invoker, err := dataapi.NewClient(baseURL, dataapi.WithClient(boundedClient{client: &httpClient, endpoint: endpoint}))
	if err != nil {
		return nil, err
	}
	return &Client{invoker: invoker, mode: binding.Remote}, nil
}

func validateEndpoint(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("data-api requires an absolute endpoint without credentials or query")
	}
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return errors.New("data-api requires authenticated HTTPS")
	}
	return nil
}

func verifyPeer(state tls.ConnectionState) error {
	if len(state.VerifiedChains) > 0 && len(state.PeerCertificates) > 0 {
		identities := state.PeerCertificates[0].URIs
		if len(identities) == 1 && identities[0].String() == serverPeer {
			return nil
		}
	}
	return errors.New("data-api server identity rejected")
}

type boundedClient struct {
	client   *http.Client
	endpoint *url.URL
}

func (c boundedClient) Do(req *http.Request) (*http.Response, error) {
	// Ogen supports per-request server overrides. A binding must only forward
	// the caller context to its configured owner.
	if req.URL.Scheme != c.endpoint.Scheme || req.URL.Host != c.endpoint.Host || req.URL.User != nil {
		return nil, errors.New("data-api endpoint override rejected")
	}
	response, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxResponseBytes {
		return nil, errors.New("data-api response exceeds limit")
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	return response, nil
}

func invoke[T any](ctx context.Context, c *Client, op string, call func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := binding.Invoke(ctx, c.mode, binding.Op{Subsystem: "data-api", Name: op, Timeout: timeout, Idempotent: true},
		func(attempt context.Context) (T, error) {
			var zero T
			if attempt.Err() != nil {
				return zero, errors.New("data-api deadline expired")
			}
			value, err := call(attempt)
			if attempt.Err() != nil {
				return zero, errors.New("data-api deadline expired")
			}
			return value, translate(err)
		})
	if err != nil && ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, err
}

func translate(err error) error {
	var p *dataapi.ProblemStatusCode
	if !errors.As(err, &p) {
		return err
	}
	code := "invalid_request"
	switch p.StatusCode {
	case http.StatusUnauthorized:
		code = "unauthenticated"
	case http.StatusForbidden:
		code = "forbidden"
	case http.StatusNotFound:
		code = "not_found"
	}
	if p.StatusCode >= 500 {
		code = "unavailable"
	}
	if p.StatusCode != p.Response.Status {
		return fmt.Errorf("%w: data-api problem status mismatch", binding.ErrTransient)
	}
	out := &Error{Status: p.StatusCode, Code: p.Response.Code.Or(code), Detail: p.Response.Detail.Or("")}
	if p.StatusCode >= 500 {
		return fmt.Errorf("%w: %w", binding.ErrTransient, out)
	}
	return out
}

// ContextHeader exposes the canonical context encoding for tests and custom transports.
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

func decodeForConsumer[T any](value T, err error, out any) error {
	if err != nil || out == nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
