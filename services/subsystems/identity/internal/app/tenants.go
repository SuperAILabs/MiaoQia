package app

import (
	"context"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

var (
	tenantID    = regexp.MustCompile(`^tnt_[0-9A-HJKMNP-TV-Z]{26}$`)
	cellPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// ActiveTenants 实现 api.Tenants：只有不带租户上下文的 scheduler 服务主体可以枚举。
func (d *Directory) ActiveTenants(ctx context.Context, cell, after string, limit int) ([]string, error) {
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Validate() != nil || rc.Principal.Kind != "service" || rc.Principal.ID != api.SchedulerPrincipal || rc.Tenant != "" {
		return nil, failure(403, "forbidden")
	}
	if !cellPattern.MatchString(cell) || (after != "" && !tenantID.MatchString(after)) || limit < 1 || limit > api.MaxTenantsPage {
		return nil, failure(400, "invalid_request")
	}
	var out []string
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM identity_tenants WHERE state='active' AND cell=$1 AND id > $2 ORDER BY id LIMIT $3`, cell, after, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	return out, err
}
