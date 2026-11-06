// Package remote binds gateway's object page ports to the data-api read contract.
// The adapter deliberately supports the A02 procurement-project surface first;
// unsupported entity-graph objects remain unavailable instead of being guessed.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/dataread"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/api"
)

// New returns the production gateway data-api adapters. The base URL is the
// data-api internal listener, for example http://127.0.0.1:8094 in development
// or an HTTPS mTLS endpoint in production.
func New(baseURL string) (*Client, error) {
	c, err := dataread.New(baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{data: c}, nil
}

// NewWithHTTPClient is used by adapter contract tests.
func NewWithHTTPClient(baseURL string, client *http.Client) (*Client, error) {
	c, err := dataread.NewWithHTTPClient(baseURL, client)
	if err != nil {
		return nil, err
	}
	return &Client{data: c}, nil
}

type Client struct{ data *dataread.Client }

// Evidence returns the evidence resolver sharing this client's data-api transport.
func (c *Client) Evidence() api.EvidenceResolver { return evidenceClient{data: c.data} }

type evidenceClient struct{ data *dataread.Client }

type factResponse struct {
	Basis dataread.Basis `json:"basis"`
	Fact  map[string]any `json:"fact"`
}

func basisFor(b api.Basis) (dataread.Basis, *int, error) {
	switch b.View {
	case "", api.ViewCurrent:
		return dataread.Basis{View: "current"}, nil, nil
	case api.ViewSignalTime:
		if b.RecordedCutoff.IsZero() {
			return dataread.Basis{}, nil, errors.New("signal_time basis requires recorded cutoff")
		}
		return dataread.Basis{View: "known_at", RecordedCutoff: ptr(b.RecordedCutoff.UTC())}, nil, nil
	default:
		return dataread.Basis{}, nil, errors.New("unsupported gateway basis")
	}
}

func ptr[T any](v T) *T { return &v }

func (c *Client) readFact(ctx context.Context, id string, b api.Basis) (map[string]any, error) {
	wire, version, err := basisFor(b)
	if err != nil {
		return nil, err
	}
	var out factResponse
	if err := c.data.GetFact(ctx, id, wire, version, &out); err != nil {
		var de *dataread.Error
		if errors.As(err, &de) && de.Status == http.StatusNotFound {
			return nil, api.ErrNotFound
		}
		return nil, err
	}
	if out.Basis.View != wire.View || !sameCutoff(out.Basis.RecordedCutoff, wire.RecordedCutoff) {
		return nil, fmt.Errorf("data-api fact basis mismatch")
	}
	if out.Fact == nil || stringValue(out.Fact["id"]) != id {
		return nil, api.ErrNotFound
	}
	return out.Fact, nil
}

func sameCutoff(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func (c *Client) Resolve(ctx context.Context, id string, b api.Basis) (*api.Object, error) {
	if !strings.HasPrefix(id, "pp_") {
		return nil, api.ErrNotWired
	}
	fact, err := c.readFact(ctx, id, b)
	if err != nil {
		return nil, err
	}
	name := textField(fact["title"])
	if name == "" {
		return nil, fmt.Errorf("data-api project fact has no title")
	}
	obj := &api.Object{ID: id, RequestedID: id, Type: "procurement_project", Name: name,
		InfoDomain: stringValue(fact["info_domain"]), Region: locationCode(fact["location"])}
	return obj, nil
}

func (c *Client) Section(ctx context.Context, req api.SectionRequest) (*api.SectionData, error) {
	if req.Object.Type != "procurement_project" || !strings.HasPrefix(req.Object.ID, "pp_") {
		return nil, api.ErrNotWired
	}
	fact, err := c.readFact(ctx, req.Object.ID, req.Basis)
	if err != nil {
		return nil, err
	}
	out := &api.SectionData{Basis: req.Basis, Status: "empty", ReasonCode: "no_records", Items: []map[string]any{}}
	switch req.Section {
	case "summary":
		item := map[string]any{}
		for _, key := range []string{"stage", "title", "buyer", "project_code", "notice_ids", "info_domain", "version"} {
			if value, ok := fact[key]; ok {
				item[key] = value
			}
		}
		if len(item) > 0 {
			out.Items = []map[string]any{item}
			out.Status, out.ReasonCode = "ok", ""
		}
	case "notice_timeline":
		if ids, ok := fact["notice_ids"].([]any); ok {
			for _, id := range ids {
				if value := stringValue(id); value != "" {
					out.Items = append(out.Items, map[string]any{"notice_id": value})
				}
			}
		}
		if len(out.Items) > 0 {
			out.Status, out.ReasonCode = "ok", ""
		}
	case "lots":
		if lots, ok := fact["lots"].([]any); ok {
			for _, lot := range lots {
				if value, ok := lot.(map[string]any); ok {
					out.Items = append(out.Items, value)
				}
			}
		}
		if len(out.Items) > 0 {
			out.Status, out.ReasonCode = "ok", ""
		}
	case "parent_projects":
		// The current A02 project projection explicitly has no parent-project relation.
	default:
		return &api.SectionData{Basis: req.Basis, Status: "not_connected", ReasonCode: "section_not_connected", Items: []map[string]any{}}, nil
	}
	start := 0
	if req.Cursor != "" {
		start, err = strconv.Atoi(req.Cursor)
		if err != nil || start < 0 {
			return nil, fmt.Errorf("invalid data-api section cursor")
		}
	}
	if start >= len(out.Items) {
		out.Items = []map[string]any{}
	} else {
		end := len(out.Items)
		if req.Limit > 0 && start+req.Limit < end {
			end = start + req.Limit
			out.NextCursor = strconv.Itoa(end)
		}
		out.Items = out.Items[start:end]
	}
	if len(out.Items) == 0 && out.Status == "ok" {
		out.Status, out.ReasonCode = "empty", "no_records"
	}
	if value := timeField(fact["recorded_at"]); !value.IsZero() {
		out.AsOf = value
	}
	if version, ok := fact["version"].(float64); ok {
		out.StatsVersion = fmt.Sprintf("fact:%d", int64(version))
	}
	return out, nil
}

func (c evidenceClient) Resolve(ctx context.Context, id, _ string) (*api.Evidence, error) {
	var wire struct {
		ID           string         `json:"id"`
		DisplayLevel string         `json:"display_level"`
		ObjectID     string         `json:"object_id"`
		SourceID     string         `json:"source_id"`
		SourceKey    string         `json:"source_key"`
		ObservedAt   string         `json:"observed_at"`
		Locator      map[string]any `json:"locator"`
		Snippet      string         `json:"snippet"`
		Context      string         `json:"context"`
	}
	if err := c.data.ResolveEvidence(ctx, id, &wire); err != nil {
		var de *dataread.Error
		if errors.As(err, &de) && de.Status == http.StatusNotFound {
			return nil, api.ErrNotFound
		}
		return nil, err
	}
	if wire.ID != id || wire.ObjectID == "" {
		return nil, fmt.Errorf("data-api evidence missing object id")
	}
	locator, _ := json.Marshal(wire.Locator)
	observed := time.Time{}
	if wire.ObservedAt != "" {
		observed, _ = time.Parse(time.RFC3339Nano, wire.ObservedAt)
	}
	return &api.Evidence{ID: wire.ID, Kind: "document", DisplayLevel: wire.DisplayLevel, ObjectID: wire.ObjectID,
		Source:          api.EvidenceSource{Name: wire.SourceID, Title: wire.SourceKey, URL: sourceURL(wire.SourceKey), Reachable: sourceURL(wire.SourceKey) != ""},
		SnapshotSavedAt: observed, Snippet: wire.Snippet, Context: wire.Context, Locator: string(locator)}, nil
}

func sourceURL(value string) string {
	if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
		return value
	}
	return ""
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case map[string]any:
		return stringValue(v["value"])
	default:
		return ""
	}
}

func textField(value any) string { return stringValue(value) }

func locationCode(value any) string {
	if m, ok := value.(map[string]any); ok {
		return stringValue(m["code"])
	}
	return ""
}

func timeField(value any) time.Time {
	s := stringValue(value)
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
