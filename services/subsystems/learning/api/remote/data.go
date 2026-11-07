// Package remote binds learning's simulation inputs to the data-api read contract.
// It covers the A02 procurement result view and keeps unknown domains disabled.
package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/dataread"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/learning/api"
)

func New(baseURL string) (*Client, error) {
	c, err := dataread.New(baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{data: c, live: map[string]bool{"INF-PROC": true}}, nil
}

func NewWithHTTPClient(baseURL string, client *http.Client) (*Client, error) {
	c, err := dataread.NewWithHTTPClient(baseURL, client)
	if err != nil {
		return nil, err
	}
	return &Client{data: c, live: map[string]bool{"INF-PROC": true}}, nil
}

type Client struct {
	data *dataread.Client
	live map[string]bool
}

type page struct {
	Items      []map[string]any `json:"items"`
	NextCursor string           `json:"next_cursor"`
}

type factResponse struct {
	Fact map[string]any `json:"fact"`
}

type unit struct {
	FactType string              `json:"fact_type"`
	Domain   string              `json:"domain"`
	Event    string              `json:"event"`
	Subject  subject             `json:"subject"`
	Fields   map[string]any      `json:"fields"`
	Items    map[string]any      `json:"items"`
	Evidence map[string]evidence `json:"evidence"`
	Display  display             `json:"display"`
}

type subject struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

type evidence struct {
	Confidence float64 `json:"confidence"`
	Method     string  `json:"method"`
	Suspect    bool    `json:"suspect"`
}

type display struct {
	Title     string `json:"title"`
	AmountCNY *int64 `json:"amount_cny"`
}

// simulationContext 把仿真读取固定为草案所属租户的 purpose=simulation 服务读取（source-rights-runtime.md“服务主体按租户读取”）。
// 没有租户或不是服务主体时不读取。
func simulationContext(ctx context.Context) (context.Context, error) {
	rc, ok := reqctx.From(ctx)
	if !ok {
		return nil, reqctx.ErrMissing
	}
	if rc.Tenant == "" || rc.Cell == "" || rc.Principal.Kind != "service" {
		return nil, fmt.Errorf("learning simulation read requires a tenant-scoped service principal")
	}
	rc.Purpose = "simulation"
	return reqctx.With(ctx, rc), nil
}

func (c *Client) Load(ctx context.Context, from, to time.Time) (*api.DatasetSlice, error) {
	ctx, err := simulationContext(ctx)
	if err != nil {
		return nil, err
	}
	out := &api.DatasetSlice{Version: fmt.Sprintf("data-api:%s:%s", from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)), Fixture: false,
		CoverageStart: from.UTC(), FactDomains: map[string]string{"tendering.bid_result": "INF-PROC"},
		ViewDomains: map[string]string{"winner.history_categories": "INF-PROC", "winner.supplied_brand_owners": "INF-PROC", "winner.credit_flags": "INF-CRED"}}
	after := ""
	for {
		var facts page
		if err := c.data.QueryFacts(ctx, "INF-PROC", "tendering.notice", dataread.Basis{View: "current"}, after, 100, &facts); err != nil {
			return nil, err
		}
		for _, raw := range facts.Items {
			id := stringValue(raw["id"])
			if id == "" {
				continue
			}
			occurred := firstTime(raw, "published_at", "observed_at")
			known := firstTime(raw, "recorded_at", "published_at", "observed_at")
			if occurred.Before(from) || !occurred.Before(to) || known.After(to) {
				continue
			}
			var units page
			version := intValue(raw["version"])
			if err := c.data.BidResultUnits(ctx, id, dataread.Basis{View: "current"}, intPtr(version), &units); err != nil {
				return nil, err
			}
			for _, rawUnit := range units.Items {
				var u unit
				if err := remap(rawUnit, &u); err != nil {
					return nil, err
				}
				fv := api.FactVersion{ID: u.Subject.ID, Type: u.FactType, Event: u.Event, OccurredAt: occurred, KnownAt: known,
					Title: u.Display.Title, AmountCNY: u.Display.AmountCNY, Fields: u.Fields, Items: u.Items, Evidence: map[string]api.Evidence{}}
				for key, e := range u.Evidence {
					fv.Evidence[key] = api.Evidence{Confidence: e.Confidence, Method: e.Method, Suspect: e.Suspect}
				}
				out.Facts = append(out.Facts, fv)
			}
		}
		if facts.NextCursor == "" {
			break
		}
		after = facts.NextCursor
	}
	return out, nil
}

func (c *Client) Status(_ context.Context, domains []string) (map[string]string, error) {
	out := map[string]string{}
	for _, domain := range domains {
		if c.live[domain] {
			out[domain] = api.DomainLive
		}
	}
	return out, nil
}

func remap(raw map[string]any, out *unit) error {
	// A single map decode keeps the adapter strict about required subject and type fields.
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return err
	}
	if out.FactType == "" || out.Subject.ID == "" || out.Subject.Version < 1 {
		return fmt.Errorf("data-api learning unit is incomplete")
	}
	return nil
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func intValue(v any) int {
	if n, ok := v.(float64); ok {
		return int(n)
	}
	return 1
}

func intPtr(v int) *int { return &v }

func firstTime(m map[string]any, keys ...string) time.Time {
	for _, key := range keys {
		if value, ok := m[key].(map[string]any); ok {
			if s, ok := value["value"].(string); ok {
				if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
					return t
				}
			}
		}
		if s, ok := m[key].(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}
