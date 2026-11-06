package search

import (
	"context"
	"testing"

	"github.com/superailabs/miaoqia/services/subsystems/search/api"
)

func TestPercolatorFiltersByCellFactTypeRegionAndCategory(t *testing.T) {
	p := newPercolator()
	ctx := context.Background()
	if err := p.Register(ctx, api.WatchQuery{ID: "watch-a", Tenant: "tnt_a", Cell: "cell-01",
		FactTypes: []string{"tendering.bid_result"}, Regions: []string{"330000"}, Categories: []string{"15-01"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Register(ctx, api.WatchQuery{ID: "watch-b", Tenant: "tnt_b", Cell: "cell-02"}); err != nil {
		t.Fatal(err)
	}
	matched, err := p.Percolate(ctx, api.Fact{Cell: "cell-01", FactID: "ntc_1", Version: 1, Units: []api.FactUnit{{
		FactType: "tendering.bid_result", Region: "330100", Categories: []string{"15-01-03"},
	}}})
	if err != nil || len(matched) != 1 || matched[0] != "tnt_a" {
		t.Fatalf("matched=%v err=%v", matched, err)
	}
	matched, err = p.Percolate(ctx, api.Fact{Cell: "cell-01", FactID: "ntc_2", Version: 1, Units: []api.FactUnit{{
		FactType: "tendering.bid_result", Region: "310000", Categories: []string{"15-01-03"},
	}}})
	if err != nil || len(matched) != 0 {
		t.Fatalf("mismatched region returned=%v err=%v", matched, err)
	}
}

func TestPercolatorFailsClosedBeforeAnyQueryIsRegistered(t *testing.T) {
	_, err := newPercolator().Percolate(context.Background(), api.Fact{Cell: "cell-01"})
	if err != ErrPercolatorNotReady {
		t.Fatalf("err=%v", err)
	}
}
