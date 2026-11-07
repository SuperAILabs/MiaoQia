package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/superailabs/miaoqia/services/kernel/dataread"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// NewFacts connects detection's Facts port to the data-api bid-result view.
// Candidates and project links remain separate ports because their data
// contracts and authorization scopes are different.
func NewFacts(baseURL string) (api.Facts, error) {
	c, err := dataread.New(baseURL)
	if err != nil {
		return nil, err
	}
	return factsClient{client: c}, nil
}

// NewFactsWithHTTPClient is for transport contract tests. Production assembly
// should use New so mTLS and the shared binding transport are installed.
func NewFactsWithHTTPClient(baseURL string, httpClient *http.Client) (api.Facts, error) {
	c, err := dataread.NewWithHTTPClient(baseURL, httpClient)
	if err != nil {
		return nil, err
	}
	return factsClient{client: c}, nil
}

type factsClient struct{ client *dataread.Client }

type wirePage struct {
	Items []wireUnit `json:"items"`
}

type wireUnit struct {
	Key         string                  `json:"key"`
	FactType    string                  `json:"fact_type"`
	Domain      string                  `json:"domain"`
	Subject     wireSubject             `json:"subject"`
	Event       string                  `json:"event"`
	Fields      map[string]any          `json:"fields"`
	Items       map[string]any          `json:"items"`
	ItemDomains map[string]string       `json:"item_domains"`
	ItemRefs    []wireItemRef           `json:"item_refs"`
	Evidence    map[string]wireEvidence `json:"evidence"`
	Display     wireDisplay             `json:"display"`
}

type wireSubject struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
	PartID  string `json:"part_id"`
}

type wireItemRef struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Version int64  `json:"version"`
	Domain  string `json:"domain"`
}

type wireEvidence struct {
	Confidence float64 `json:"confidence"`
	Method     string  `json:"method"`
	Suspect    bool    `json:"suspect"`
}

type wireDisplay struct {
	Title                string   `json:"title"`
	WinnerOrgID          string   `json:"winner_org_id"`
	WinnerName           string   `json:"winner_name"`
	AmountValue          string   `json:"amount_value"`
	AmountCurrency       string   `json:"amount_currency"`
	AmountCNY            *int64   `json:"amount_cny"`
	Location             string   `json:"location"`
	Categories           []string `json:"categories"`
	LotKey               string   `json:"lot_key"`
	ProcurementProjectID string   `json:"procurement_project_id"`
	BuyerOrgID           string   `json:"buyer_org_id"`
	BuyerName            string   `json:"buyer_name"`
}

func (c factsClient) Units(ctx context.Context, ref api.FactRef) ([]api.Unit, error) {
	if ref.ID == "" || ref.Version < 1 {
		return nil, fmt.Errorf("invalid fact reference")
	}
	version := int(ref.Version)
	var page wirePage
	if err := c.client.BidResultUnits(ctx, ref.ID, dataread.Basis{View: datareadBasisCurrent}, &version, &page); err != nil {
		var denied *dataread.Error
		if errors.As(err, &denied) && denied.Status == http.StatusForbidden {
			return nil, fmt.Errorf("%w: %w", api.ErrReadDenied, err)
		}
		return nil, err
	}
	units := make([]api.Unit, 0, len(page.Items))
	for i, item := range page.Items {
		unit, err := item.convert()
		if err != nil {
			return nil, fmt.Errorf("data-api bid-result unit %d: %w", i, err)
		}
		if unit.Subject.ID != ref.ID || unit.Subject.Version != ref.Version {
			return nil, fmt.Errorf("data-api bid-result unit %d has mismatched subject", i)
		}
		// domain.fact.upserted carries the source notice object type while the
		// data-api projection exposes the evaluation unit type (bid_result).
		// Keep rejecting unrelated projections, but allow this intentional
		// notice → bid_result view boundary.
		if ref.ObjectType != "" && unit.FactType != ref.ObjectType && !(ref.ObjectType == "tendering.notice" && unit.FactType == "tendering.bid_result") {
			return nil, fmt.Errorf("data-api bid-result unit %d has mismatched fact type", i)
		}
		if ref.InfoDomain != "" && unit.Domain != ref.InfoDomain {
			return nil, fmt.Errorf("data-api bid-result unit %d has mismatched information domain", i)
		}
		units = append(units, unit)
	}
	return units, nil
}

const datareadBasisCurrent = "current"

func (w wireUnit) convert() (api.Unit, error) {
	if w.Key == "" || w.FactType == "" || w.Domain == "" || w.Subject.ID == "" || w.Subject.Version < 1 || w.Subject.PartID == "" {
		return api.Unit{}, fmt.Errorf("incomplete unit")
	}
	evidence := make(map[string]api.Evidence, len(w.Evidence))
	for key, value := range w.Evidence {
		if key == "" || value.Method == "" || value.Confidence < 0 || value.Confidence > 1 {
			return api.Unit{}, fmt.Errorf("invalid evidence")
		}
		evidence[key] = api.Evidence{Confidence: value.Confidence, Method: value.Method, Suspect: value.Suspect}
	}
	refs := make([]api.ItemRef, len(w.ItemRefs))
	for i, ref := range w.ItemRefs {
		if ref.Name == "" || ref.ID == "" || ref.Version < 1 || ref.Domain == "" {
			return api.Unit{}, fmt.Errorf("invalid item reference")
		}
		refs[i] = api.ItemRef{Name: ref.Name, ID: ref.ID, Version: ref.Version, Domain: ref.Domain}
	}
	return api.Unit{
		Key: w.Key, FactType: w.FactType, Domain: w.Domain,
		Subject: api.Subject{ID: w.Subject.ID, Version: w.Subject.Version, PartID: w.Subject.PartID},
		Event:   w.Event, Fields: w.Fields, Items: w.Items, ItemDomains: w.ItemDomains,
		ItemRefs: refs, Evidence: evidence,
		Display: api.Display{Title: w.Display.Title, WinnerOrgID: w.Display.WinnerOrgID, WinnerName: w.Display.WinnerName,
			AmountValue: w.Display.AmountValue, AmountCurrency: w.Display.AmountCurrency, AmountCNY: w.Display.AmountCNY,
			Location: w.Display.Location, Categories: w.Display.Categories, LotKey: w.Display.LotKey,
			ProcurementProjectID: w.Display.ProcurementProjectID, BuyerOrgID: w.Display.BuyerOrgID, BuyerName: w.Display.BuyerName},
	}, nil
}
