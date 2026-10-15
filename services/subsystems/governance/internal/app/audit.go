// Package app 是 governance 的用例编排。
package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
)

// Ingest 存储一条审计事件；同一事件只存一次。
func Ingest(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var d audit.Data
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return err
	}
	if env.Source != "miaoqia/"+d.Subsystem || d.Tenant != env.Tenant || d.Actor == "" || d.Action == "" || d.Resource == "" {
		return errors.New("audit origin/context mismatch")
	}
	var tenant *string
	if d.Tenant != "" {
		tenant = &d.Tenant
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit_entries (id, event_id, tenant_id, actor, actor_kind, subsystem, action, resource, result, basis, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''), $11) ON CONFLICT (event_id) DO NOTHING`,
		ids.New("aud"), env.ID, tenant, d.Actor, d.ActorKind, d.Subsystem, d.Action, d.Resource, d.Result, d.Basis, d.OccurredAt)
	return err
}
