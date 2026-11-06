package search

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/subsystems/search/api"
)

var ErrPercolatorNotReady = errors.New("search：订阅反查索引尚未就绪")

type percolator struct {
	db      *db.DB
	mu      sync.RWMutex
	queries map[string]api.WatchQuery
}

func newPercolator(database ...*db.DB) api.Percolator {
	var handle *db.DB
	if len(database) > 0 {
		handle = database[0]
	}
	return &percolator{db: handle, queries: map[string]api.WatchQuery{}}
}

func (p *percolator) Register(ctx context.Context, query api.WatchQuery) error {
	if query.ID == "" || query.Tenant == "" || query.Cell == "" {
		return errors.New("search：订阅缺少标识、租户或 cell")
	}
	if p.db != nil {
		factTypes := query.FactTypes
		regions := query.Regions
		categories := query.Categories
		if factTypes == nil {
			factTypes = []string{}
		}
		if regions == nil {
			regions = []string{}
		}
		if categories == nil {
			categories = []string{}
		}
		rawTypes, _ := json.Marshal(factTypes)
		rawRegions, _ := json.Marshal(regions)
		rawCategories, _ := json.Marshal(categories)
		return p.db.Tx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO sea_watch_queries (id, tenant_id, cell, fact_types, regions, categories, updated_at)
VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6::jsonb, now())
ON CONFLICT (id) DO UPDATE SET tenant_id = EXCLUDED.tenant_id, cell = EXCLUDED.cell, fact_types = EXCLUDED.fact_types,
regions = EXCLUDED.regions, categories = EXCLUDED.categories, updated_at = now()`, query.ID, query.Tenant, query.Cell, rawTypes, rawRegions, rawCategories)
			return err
		})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queries[query.ID] = query
	return nil
}

func (p *percolator) Remove(ctx context.Context, id string) error {
	if p.db != nil {
		return p.db.Tx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM sea_watch_queries WHERE id = $1`, id)
			return err
		})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.queries, id)
	return nil
}

func (p *percolator) Percolate(ctx context.Context, fact api.Fact) ([]string, error) {
	if p.db != nil {
		queries := []api.WatchQuery{}
		err := p.db.Tx(ctx, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id, tenant_id, cell, fact_types, regions, categories FROM sea_watch_queries WHERE cell = $1`, fact.Cell)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var query api.WatchQuery
				var types, regions, categories []byte
				if err := rows.Scan(&query.ID, &query.Tenant, &query.Cell, &types, &regions, &categories); err != nil {
					return err
				}
				if err := json.Unmarshal(types, &query.FactTypes); err != nil {
					return err
				}
				if err := json.Unmarshal(regions, &query.Regions); err != nil {
					return err
				}
				if err := json.Unmarshal(categories, &query.Categories); err != nil {
					return err
				}
				queries = append(queries, query)
			}
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
		if len(queries) == 0 {
			return nil, ErrPercolatorNotReady
		}
		return matchQueries(queries, fact), nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.queries) == 0 {
		return nil, ErrPercolatorNotReady
	}
	queries := make([]api.WatchQuery, 0, len(p.queries))
	for _, query := range p.queries {
		queries = append(queries, query)
	}
	return matchQueries(queries, fact), nil
}

// matchQueries 在没有求值单位时（Units 为 nil）只按 cell 粗筛：返回 cell 内登记了订阅的全部租户，不读取事实内容；
// 带求值单位时再按事实类型、区域与品类过滤。
func matchQueries(queries []api.WatchQuery, fact api.Fact) []string {
	matched := map[string]struct{}{}
	for _, query := range queries {
		if query.Cell == fact.Cell && (fact.Units == nil || anyUnitMatches(query, fact)) {
			matched[query.Tenant] = struct{}{}
		}
	}
	out := make([]string, 0, len(matched))
	for tenant := range matched {
		out = append(out, tenant)
	}
	slices.Sort(out)
	return out
}

func anyUnitMatches(query api.WatchQuery, fact api.Fact) bool {
	for _, unit := range fact.Units {
		if len(query.FactTypes) > 0 && !slices.Contains(query.FactTypes, unit.FactType) {
			continue
		}
		if len(query.Regions) > 0 && !anyRegionWithin(query.Regions, unit.Region) {
			continue
		}
		if len(query.Categories) > 0 && !anyCategoryMatch(query.Categories, unit.Categories) {
			continue
		}
		return true
	}
	return false
}

func anyRegionWithin(rules []string, value string) bool {
	if value == "" {
		return false
	}
	for _, rule := range rules {
		prefix := strings.TrimRight(rule, "0")
		if rule != "" && (value == rule || strings.HasPrefix(value, rule) || (prefix != "" && strings.HasPrefix(value, prefix))) {
			return true
		}
	}
	return false
}

func anyCategoryMatch(rules, values []string) bool {
	for _, rule := range rules {
		for _, value := range values {
			if rule != "" && (value == rule || strings.HasPrefix(value, rule+"-")) {
				return true
			}
		}
	}
	return false
}
