// Package app 是 operations 的用例编排。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/operations/api"
)

const platformObject = "platform:miaoqia"

// Probes 是链路探针用例。
type Probes struct {
	DB    *db.DB
	Authz authz.Decider
}

var (
	writer   = outbox.Writer{Subsystem: api.Name, Namespace: api.Name, Zone: reqctx.ZonePlatform}
	recorder = audit.Recorder{Subsystem: api.Name}
)

// ErrNotFound 表示探针不存在。
var ErrNotFound = errors.New("operations: 探针不存在")

// Trigger 发起一次探针：授权检查，探针记录、事件与审计同事务写入。授权拒绝同样留下审计。
func (p *Probes) Trigger(ctx context.Context) (*api.Probe, error) {
	if err := authz.Require(ctx, p.Authz, api.ProbeRelation, platformObject); err != nil {
		if errors.Is(err, authz.ErrForbidden) {
			_ = p.DB.Tx(ctx, func(tx pgx.Tx) error {
				return recorder.Record(ctx, tx, audit.Entry{Action: "operations.probe.trigger", Resource: platformObject,
					Result: "denied", Basis: api.ProbeRelation})
			})
		}
		return nil, err
	}
	rc, _ := reqctx.From(ctx)
	pr := &api.Probe{ID: ids.New("prb"), Status: "requested", RequestedBy: rc.Principal.ID, Hops: []string{api.Name},
		RequestedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	err := p.DB.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO probes (id, status, requested_by, hops, requested_at) VALUES ($1, $2, $3, $4, $5)`,
			pr.ID, pr.Status, pr.RequestedBy, pr.Hops, pr.RequestedAt); err != nil {
			return err
		}
		if _, err := writer.Append(ctx, tx, outbox.Event{Type: api.ProbeRequested, Subject: pr.ID,
			Data: map[string]any{"probe_id": pr.ID, "requested_at": pr.RequestedAt}}); err != nil {
			return err
		}
		return recorder.Record(ctx, tx, audit.Entry{Action: "operations.probe.trigger", Resource: pr.ID, Result: "succeeded",
			Basis: api.ProbeRelation})
	})
	if err != nil {
		return nil, err
	}
	return pr, nil
}

// Get 查询探针状态。
func (p *Probes) Get(ctx context.Context, id string) (*api.Probe, error) {
	var pr api.Probe
	var req time.Time
	var done *time.Time
	err := p.DB.Tx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id, status, requested_by, hops, requested_at, completed_at FROM probes WHERE id = $1`, id).
			Scan(&pr.ID, &pr.Status, &pr.RequestedBy, &pr.Hops, &req, &done)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	pr.RequestedAt = req.UTC().Format(time.RFC3339Nano)
	if done != nil {
		pr.CompletedAt = done.UTC().Format(time.RFC3339Nano)
	}
	return &pr, nil
}

// Complete 处理探针回到 operations 的事件（evidence.probe.forwarded.v1）。
func (p *Probes) Complete(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var d struct {
		ProbeID string   `json:"probe_id"`
		Hops    []string `json:"hops"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return err
	}
	hops := append(d.Hops, api.Name)
	tag, err := tx.Exec(ctx, `UPDATE probes SET status = 'completed', hops = $2, completed_at = now() WHERE id = $1 AND status = 'requested'`,
		d.ProbeID, hops)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // 未知或已完成的探针：忽略
	}
	return recorder.Record(ctx, tx, audit.Entry{Action: "operations.probe.complete", Resource: d.ProbeID, Result: "succeeded"})
}
