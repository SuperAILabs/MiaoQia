package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

const (
	deniedTenant   = "tnt_01JA8ZXXXXXXXXXXXXXXXXXXXA"
	filteredTenant = "tnt_01JA8ZXXXXXXXXXXXXXXXXXXXB"
	matchedTenant  = "tnt_01JA8ZXXXXXXXXXXXXXXXXXXXC"
)

type tenantCandidates struct {
	mu       sync.Mutex
	coarse   []reqctx.Context
	filtered []string
}

func (c *tenantCandidates) Tenants(ctx context.Context, cell string, _ api.FactRef) ([]string, error) {
	rc, _ := reqctx.From(ctx)
	c.mu.Lock()
	c.coarse = append(c.coarse, rc)
	c.mu.Unlock()
	return []string{matchedTenant, deniedTenant, filteredTenant, matchedTenant, "not-a-tenant"}, nil
}

func (c *tenantCandidates) TenantsFor(ctx context.Context, _ string, _ api.FactRef, units []api.Unit) ([]string, error) {
	rc, _ := reqctx.From(ctx)
	c.mu.Lock()
	c.filtered = append(c.filtered, rc.Tenant)
	c.mu.Unlock()
	if len(units) == 0 {
		return nil, fmt.Errorf("filter without units")
	}
	return []string{matchedTenant}, nil
}

type tenantFacts struct {
	mu    sync.Mutex
	reads []reqctx.Context
}

func (f *tenantFacts) Units(ctx context.Context, ref api.FactRef) ([]api.Unit, error) {
	rc, _ := reqctx.From(ctx)
	f.mu.Lock()
	f.reads = append(f.reads, rc)
	f.mu.Unlock()
	if rc.Tenant == deniedTenant {
		return nil, fmt.Errorf("%w: 403", api.ErrReadDenied)
	}
	return []api.Unit{{FactType: "tendering.bid_result", Domain: "INF-PROC", Subject: api.Subject{ID: ref.ID, Version: ref.Version}}}, nil
}

func TestFactUpsertedReadsPerCandidateTenantWithMonitoringPurpose(t *testing.T) {
	candidates, facts := &tenantCandidates{}, &tenantFacts{}
	// Policies are deliberately not wired: reaching evaluation fails with a
	// recognizable error, which proves which tenant was evaluated.
	s := &Service{Candidates: candidates, Facts: facts, Cell: "cell-01"}
	data, _ := json.Marshal(map[string]any{"fact_id": "ntc_1", "fact_version": 2, "object_type": "tendering.notice", "info_domain": "INF-PROC"})
	consumer := reqctx.Service(context.Background(), api.Name)
	err := s.FactUpserted(consumer, nil, &outbox.Envelope{ID: "evt_1", Time: time.Now(), Data: data})
	if err == nil || !strings.Contains(err.Error(), "租户 "+matchedTenant) || !strings.Contains(err.Error(), "生效策略读取未组装") {
		t.Fatalf("expected evaluation of the matched tenant only, err=%v", err)
	}
	if len(candidates.coarse) != 1 || candidates.coarse[0].Tenant != "" {
		t.Fatalf("coarse candidates must use the tenant-less consumer context: %#v", candidates.coarse)
	}
	var tenants []string
	for _, rc := range facts.reads {
		if rc.Purpose != "monitoring" || rc.Principal.ID != "svc:detection" || rc.Principal.Kind != "service" || rc.Cell != "cell-01" {
			t.Fatalf("fact read context = %#v", rc)
		}
		tenants = append(tenants, rc.Tenant)
	}
	// Sorted and de-duplicated; the invalid tenant is never read; the denied tenant is skipped.
	if !slices.Equal(tenants, []string{deniedTenant, filteredTenant, matchedTenant}) {
		t.Fatalf("read tenants = %v", tenants)
	}
	if !slices.Equal(candidates.filtered, []string{filteredTenant, matchedTenant}) {
		t.Fatalf("filtered tenants = %v", candidates.filtered)
	}
}
