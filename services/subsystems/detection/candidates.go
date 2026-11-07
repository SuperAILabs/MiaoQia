package detection

import (
	"context"

	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
	searchapi "github.com/superailabs/miaoqia/services/subsystems/search/api"
)

type searchCandidates struct{ percolator searchapi.Percolator }

func (c searchCandidates) Tenants(ctx context.Context, cell string, ref api.FactRef) ([]string, error) {
	return c.percolator.Percolate(ctx, searchapi.Fact{Cell: cell, FactID: ref.ID, Version: ref.Version,
		ObjectType: ref.ObjectType, InfoDomain: ref.InfoDomain})
}

func (c searchCandidates) TenantsFor(ctx context.Context, cell string, ref api.FactRef, units []api.Unit) ([]string, error) {
	projected := make([]searchapi.FactUnit, 0, len(units))
	for _, unit := range units {
		projected = append(projected, searchapi.FactUnit{FactType: unit.FactType, Domain: unit.Domain,
			Fields: unit.Fields, Region: unit.Display.Location, Categories: unit.Display.Categories})
	}
	return c.percolator.Percolate(ctx, searchapi.Fact{Cell: cell, FactID: ref.ID, Version: ref.Version,
		ObjectType: ref.ObjectType, InfoDomain: ref.InfoDomain, Units: projected})
}
