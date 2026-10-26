// Package app 是 commerce 的用例编排：对象查看与原文查看的计量与额度（PRD 第4段 F9）。
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/commerce/api"
)

// ViewDeferred 是 gateway 在计量服务不可用时放行查看后发布的补记事件（contracts/events/gateway）。
const ViewDeferred = "gateway.view_meter.deferred.v1"

// DefaultDailyQuota 是每租户、每种查看、每计量日的缺省额度（套餐权益接入前由部署配置覆盖）。
const DefaultDailyQuota = 2000

var (
	targetPattern = regexp.MustCompile(`^(pp|prj|org|ev)_[0-9A-HJKMNP-TV-Z]{26}$`)
	keyPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
)

// Service 是 commerce 的查看计量与覆盖判断。Quotas 为种类 → 每计量日额度；Zone 为计量日的时区（租户时区登记前全部租户共用，
// 缺省 Asia/Shanghai）；Coverage 为数据覆盖包（产品目录接入前来自部署配置，nil 为未配置）。
type Service struct {
	DB       *db.DB
	Log      *slog.Logger
	Now      func() time.Time
	Quotas   map[string]int64
	Zone     *time.Location
	Coverage CoveragePlan
}

func failure(status int, code string) error { return &api.Error{Status: status, Code: code} }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) zone() *time.Location {
	if s.Zone != nil {
		return s.Zone
	}
	return time.UTC
}

// day 返回 t 所在的计量日（租户时区的日期，UTC 零点表示）与下一计量日的开始时间。
func (s *Service) day(t time.Time) (time.Time, time.Time) {
	local := t.In(s.zone())
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.zone())
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC), start.AddDate(0, 0, 1).UTC()
}

// meterKey 是计量操作标识（租户、查看者、种类、对象或证据、计量日）的摘要。
func meterKey(tenant, member, kind, target string, day time.Time) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{tenant, member, kind, target, day.Format("2006-01-02")}, "\x00")))
	return "mtr:" + hex.EncodeToString(sum[:])
}

func validKind(kind string) bool { return kind == api.ObjectView || kind == api.EvidenceView }

// tx 在租户事务中执行；基础设施错误不外泄（503 commerce_unavailable）。
func (s *Service) tx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	if s == nil || s.DB == nil {
		return failure(503, "commerce_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := s.DB.Tx(ctx, func(tx pgx.Tx) error { return fn(ctx, tx) })
	var safe *api.Error
	if err == nil || errors.As(err, &safe) {
		return err
	}
	s.logger().WarnContext(ctx, "commerce 数据库事务失败", "error", err)
	return failure(503, "commerce_unavailable")
}

// receipt 是一次尝试的固定结果（成功为 Receipt 的 JSON，额度用完为 Error 的字段）。
type stored struct {
	Receipt  *api.Receipt `json:"receipt,omitempty"`
	Used     int64        `json:"used,omitempty"`
	Limit    int64        `json:"limit,omitempty"`
	ResetsAt time.Time    `json:"resets_at,omitzero"`
}

// RecordView 计一次查看（api.ViewMetering）。顺序：重放同键尝试 → 锁定当日用量 → 已计过为 unchanged（额度用完后也照常放行）→
// 额度已用完为 quota.exhausted（不写入计量操作）→ 写入计量操作并递增用量；尝试的结果与计入在同一事务中登记。
func (s *Service) RecordView(ctx context.Context, kind, target, requestKey string) (*api.Receipt, error) {
	rc, ok := reqctx.From(ctx)
	if !ok {
		return nil, reqctx.ErrMissing
	}
	if rc.Validate() != nil || rc.Tenant == "" {
		return nil, failure(403, "invalid_tenant_context")
	}
	if rc.Principal.Kind != "user" {
		return nil, failure(403, "forbidden")
	}
	if !validKind(kind) || !targetPattern.MatchString(target) {
		return nil, &api.Error{Status: 400, Code: "invalid_request", Detail: "kind 或 target 无效"}
	}
	if !keyPattern.MatchString(requestKey) {
		return nil, failure(400, "invalid_idempotency_key")
	}
	limit := s.Quotas[kind]
	if limit <= 0 {
		limit = DefaultDailyQuota
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + target))
	fingerprint := hex.EncodeToString(sum[:])
	var out *api.Receipt
	var outErr error
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "com-view/"+rc.Tenant+"/"+rc.Principal.ID+"/"+requestKey); err != nil {
			return err
		}
		var prior string
		var status int
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT fingerprint, status, response FROM com_view_attempts WHERE actor = $1 AND request_key = $2`,
			rc.Principal.ID, requestKey).Scan(&prior, &status, &raw)
		switch {
		case err == nil:
			if prior != fingerprint {
				return failure(409, "idempotency_conflict")
			}
			var st stored
			if err := json.Unmarshal(raw, &st); err != nil {
				return err
			}
			if status == 200 && st.Receipt != nil {
				out = st.Receipt
			} else {
				outErr = &api.Error{Status: 429, Code: "quota.exhausted", Used: st.Used, Limit: st.Limit, ResetsAt: st.ResetsAt}
			}
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		day, resets := s.day(s.now())
		key := meterKey(rc.Tenant, rc.Principal.ID, kind, target, day)
		if _, err := tx.Exec(ctx, `INSERT INTO com_view_usage (tenant_id, kind, metering_day) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			rc.Tenant, kind, day); err != nil {
			return err
		}
		var used int64
		if err := tx.QueryRow(ctx, `SELECT used FROM com_view_usage WHERE kind = $1 AND metering_day = $2 FOR UPDATE`, kind, day).Scan(&used); err != nil {
			return err
		}
		receipt := &api.Receipt{MeterKey: key, Kind: commerceKind(kind), Target: target, MeteringDay: day, TimeZone: s.zone().String(),
			Outcome: "unchanged", Used: used, Limit: limit, ResetsAt: resets}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_view_meters WHERE meter_key = $1)`, key).Scan(&exists); err != nil {
			return err
		}
		st := stored{}
		status = 200
		switch {
		case exists:
			st.Receipt = receipt
		case used >= limit:
			status = 429
			st = stored{Used: used, Limit: limit, ResetsAt: resets}
			outErr = &api.Error{Status: 429, Code: "quota.exhausted", Used: used, Limit: limit, ResetsAt: resets}
		default:
			if _, err := tx.Exec(ctx, `INSERT INTO com_view_meters (tenant_id, meter_key, kind, target, member, metering_day, source) VALUES ($1,$2,$3,$4,$5,$6,'online')`,
				rc.Tenant, key, kind, target, rc.Principal.ID, day); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE com_view_usage SET used = used + 1 WHERE kind = $1 AND metering_day = $2`, kind, day); err != nil {
				return err
			}
			receipt.Outcome, receipt.Used = "counted", used+1
			st.Receipt = receipt
		}
		encoded, err := json.Marshal(st)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO com_view_attempts (tenant_id, actor, request_key, fingerprint, status, response) VALUES ($1,$2,$3,$4,$5,$6)`,
			rc.Tenant, rc.Principal.ID, requestKey, fingerprint, status, encoded); err != nil {
			return err
		}
		if status == 200 {
			out = receipt
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if outErr != nil {
		return nil, outErr
	}
	return out, nil
}

// deferred 是补记事件的载荷（gateway.view_meter.deferred.v1）。
type deferred struct {
	Kind       string    `json:"kind"`
	Target     string    `json:"target"`
	Member     string    `json:"member"`
	OccurredAt time.Time `json:"occurred_at"`
	RequestID  string    `json:"request_id"`
}

// ViewDeferred 补记计量服务不可用期间放行的查看（消费 gateway.view_meter.deferred.v1）：按查看发生时的计量日与同一计量操作标识
// 去重，只补计数，不检查额度（查看已经放行；额度与安全上限的保护见 PRD 第4段第13节第3项）。租户来自信封。
func (s *Service) ViewDeferred(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	if env.Tenant == "" {
		return fmt.Errorf("补记事件缺少租户")
	}
	var d deferred
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return err
	}
	if !validKind(d.Kind) || !targetPattern.MatchString(d.Target) || d.Member == "" || d.OccurredAt.IsZero() {
		return fmt.Errorf("补记事件载荷无效")
	}
	day, _ := s.day(d.OccurredAt)
	key := meterKey(env.Tenant, d.Member, d.Kind, d.Target, day)
	tag, err := tx.Exec(ctx, `INSERT INTO com_view_meters (tenant_id, meter_key, kind, target, member, metering_day, source) VALUES ($1,$2,$3,$4,$5,$6,'backfill')
 ON CONFLICT DO NOTHING`, env.Tenant, key, d.Kind, d.Target, d.Member, day)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO com_view_usage (tenant_id, kind, metering_day, used) VALUES ($1, $2, $3, 1)
 ON CONFLICT (tenant_id, kind, metering_day) DO UPDATE SET used = com_view_usage.used + 1`, env.Tenant, d.Kind, day)
	return err
}
