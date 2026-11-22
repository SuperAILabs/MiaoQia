package dataread

import (
	"context"
	"time"

	dataapi "github.com/superailabs/miaoqia/services/contracts/apis/data-api"
)

// Basis preserves the basis accepted by the original client methods.
type Basis struct {
	View           string     `json:"view"`
	RecordedCutoff *time.Time `json:"recorded_cutoff"`
}

func (basis Basis) dto() dataapi.Basis {
	b := dataapi.Basis{View: dataapi.BasisView(basis.View)}
	b.RecordedCutoff.SetToNull()
	if basis.RecordedCutoff != nil {
		b.RecordedCutoff.SetTo(*basis.RecordedCutoff)
	}
	return b
}

func factRequest(factID string, basis Basis, version *int) dataapi.FactRequest {
	request := dataapi.FactRequest{FactID: factID, Basis: basis.dto()}
	request.Version.SetToNull()
	if version != nil {
		request.Version.SetTo(int64(*version))
	}
	return request
}

func (c *Client) GetFact(ctx context.Context, factID string, basis Basis, version *int, out any) error {
	value, err := c.ReadFact(ctx, factRequest(factID, basis, version))
	return decodeForConsumer(value, err, out)
}

func (c *Client) FactHistory(ctx context.Context, factID string, basis Basis, after, limit int, out any) error {
	value, err := c.ReadFactHistory(ctx, dataapi.FactHistoryRequest{FactID: factID, Basis: basis.dto(), After: int64(after), Limit: limit})
	return decodeForConsumer(value, err, out)
}

func (c *Client) QueryFacts(ctx context.Context, domain, objectType string, basis Basis, after string, limit int, out any) error {
	value, err := c.ReadFacts(ctx, dataapi.QueryFactsRequest{InfoDomain: domain, ObjectType: objectType, Basis: basis.dto(), After: after, Limit: limit})
	return decodeForConsumer(value, err, out)
}

func (c *Client) BidResultUnits(ctx context.Context, factID string, basis Basis, version *int, out any) error {
	value, err := c.ReadBidResultUnits(ctx, factRequest(factID, basis, version))
	return decodeForConsumer(value, err, out)
}

func (c *Client) ResolveEvidence(ctx context.Context, evidenceRef string, out any) error {
	value, err := c.ReadEvidence(ctx, dataapi.EvidenceRequest{EvidenceRef: evidenceRef})
	return decodeForConsumer(value, err, out)
}
