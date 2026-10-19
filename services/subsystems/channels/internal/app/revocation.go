package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/domain"
)

// 绑定失效（PRD 第1段 F6 第4–5步，用例11a–11e）。解绑、强制解绑、成员停用或退出、渠道停用都先在本子系统把绑定置为失效中
// （版本加一，立即不再签发免登凭据、不再投递），提交后同步请 identity 撤销到新版本；同步失败时由 channels.binding.changed.v1
// 的消费（channels 与 identity 各一路）与定时重试（RetryRevocations）送达。identity 确认撤销到当前版本、且最后一张签发凭据已过期
// （含5秒偏差）后才标为已失效；在此之前对外显示“失效中”。任何一步失败都不会把失效中的绑定当作已失效。

// revokeAfterCommit 在失效事务提交后同步撤销（尽力而为：失败只记录，由事件与定时重试送达）。
func (s *Service) revokeAfterCommit(ctx context.Context, tenant, cell string, b *binding) {
	sctx := serviceContext(ctx, tenant, cell)
	if err := s.confirmRevocation(sctx, b.id, b.version); err != nil {
		s.logger().WarnContext(ctx, "绑定失效的同步撤销未完成，等待重试", "binding", b.id, "version", b.version, "error", err.Error())
	}
}

// revokeInIdentity 请 identity 把该绑定的撤销水位抬高到 version。
func (s *Service) revokeInIdentity(ctx context.Context, id string, version int64) error {
	if s.Sessions == nil {
		return failure(503, "identity_unavailable")
	}
	_, err := s.Sessions.RevokeChannelSessions(ctx, &identityapi.RevokeChannelSessionsRequest{BindingID: identityapi.ChannelBindingID(id), BindingVersion: version})
	return identityError(err)
}

// confirmRevocation 撤销到 version 并登记结果；无论成败都记录本次尝试时间（定时重试据此轮转）。
func (s *Service) confirmRevocation(ctx context.Context, id string, version int64) error {
	revokeErr := s.revokeInIdentity(ctx, id, version)
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if revokeErr != nil {
			_, err := tx.Exec(ctx, `UPDATE chn_bindings SET revoke_attempted_at = now(), updated_at = now() WHERE id = $1`, id)
			return err
		}
		return s.recordConfirmed(ctx, tx, id, version)
	})
	if revokeErr != nil {
		return revokeErr
	}
	return err
}

// recordConfirmed 在事务中登记 identity 已撤销到 version（只升不降），并在条件满足时标为已失效。
func (s *Service) recordConfirmed(ctx context.Context, tx pgx.Tx, id string, version int64) error {
	if _, err := tx.Exec(ctx, `UPDATE chn_bindings SET revoke_confirmed_version = GREATEST(revoke_confirmed_version, $2), revoke_attempted_at = now(), updated_at = now() WHERE id = $1`,
		id, version); err != nil {
		return err
	}
	_, err := s.completeIfDue(ctx, tx, id)
	return err
}

// completeIfDue 锁定绑定行，在“已确认撤销到当前版本且最后一张凭据已过期”时标为已失效（版本与原因不变），发布事件。
// 免登签发在同一行锁下登记凭据到期时间，因此这里看到的到期时间不会被并发签发越过。
func (s *Service) completeIfDue(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	b, err := scanBinding(tx.QueryRow(ctx, `SELECT `+bindingColumns+` FROM chn_bindings WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !domain.RevocationComplete(b.state, b.version, b.confirmed, b.lastCredential, s.now()) {
		return false, nil
	}
	at := s.businessNow()
	if _, err := tx.Exec(ctx, `UPDATE chn_bindings SET state = 'invalidated', invalidated_at = $2, updated_at = now() WHERE id = $1 AND state = 'revoking' AND version = $3`,
		id, at, b.version); err != nil {
		return false, err
	}
	b.state, b.invalidatedAt = domain.Invalidated, &at
	return true, emitBinding(ctx, tx, b, "channels.revocation_completed")
}

// RetryRevocations 是 Maintenance.RetryRevocations。
func (s *Service) RetryRevocations(ctx context.Context, limit int) (int, error) {
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Validate() != nil || rc.Tenant == "" || rc.Principal.Kind != "service" {
		return 0, failure(403, "forbidden")
	}
	if limit < 1 || limit > 500 {
		return 0, failure(400, "invalid_request")
	}
	type pending struct {
		id                 string
		version, confirmed int64
	}
	var batch []pending
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, version, revoke_confirmed_version FROM chn_bindings WHERE state = 'revoking'
 ORDER BY revoke_attempted_at NULLS FIRST, id LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.version, &p.confirmed); err != nil {
				return err
			}
			batch = append(batch, p)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	completed := 0
	for _, p := range batch {
		confirmed := p.confirmed >= p.version
		if !confirmed {
			// 尚未确认：重试撤销（失败只记录尝试时间，不挡住其他绑定）
			if err := s.revokeInIdentity(ctx, p.id, p.version); err != nil {
				s.logger().WarnContext(ctx, "绑定撤销重试失败", "binding", p.id, "version", p.version, "error", err.Error())
			} else {
				confirmed = true
			}
		}
		var done bool
		err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if confirmed {
				if _, err := tx.Exec(ctx, `UPDATE chn_bindings SET revoke_confirmed_version = GREATEST(revoke_confirmed_version, $2) WHERE id = $1`, p.id, p.version); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE chn_bindings SET revoke_attempted_at = now(), updated_at = now() WHERE id = $1`, p.id); err != nil {
				return err
			}
			var err error
			done, err = s.completeIfDue(ctx, tx, p.id)
			return err
		})
		if err != nil {
			return completed, err
		}
		if done {
			completed++
		}
	}
	return completed, nil
}

// ReconcileChannelSessions 是 Maintenance.ReconcileChannelSessions：identity 中仍有有效免登会话、而本子系统中绑定不是 bound
// （或不存在）的，撤销到两边版本的较大者。本子系统查询失败时中止并返回错误，不把未知当作失效。
func (s *Service) ReconcileChannelSessions(ctx context.Context) (int, error) {
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Validate() != nil || rc.Tenant == "" || rc.Principal.Kind != "service" {
		return 0, failure(403, "forbidden")
	}
	if s.Sessions == nil {
		return 0, failure(503, "identity_unavailable")
	}
	revoked, cursor := 0, ""
	for {
		page, err := s.Sessions.ListLiveChannelSessions(ctx, cursor, 0)
		if err != nil {
			return revoked, identityError(err)
		}
		for _, item := range page.Bindings {
			id := string(item.BindingID)
			state, version := "", int64(0)
			err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				err := tx.QueryRow(ctx, `SELECT state, version FROM chn_bindings WHERE id = $1`, id).Scan(&state, &version)
				if errors.Is(err, pgx.ErrNoRows) {
					state = ""
					return nil
				}
				return err
			})
			if err != nil {
				return revoked, err
			}
			if state == domain.Bound && version >= item.BindingVersion {
				continue
			}
			target := max(version, item.BindingVersion)
			if err := s.revokeInIdentity(ctx, id, target); err != nil {
				return revoked, err
			}
			if state != "" && state != domain.Bound {
				if err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error { return s.recordConfirmed(ctx, tx, id, version) }); err != nil {
					return revoked, err
				}
			}
			revoked++
		}
		if !page.Truncated {
			return revoked, nil
		}
		next, ok := page.NextCursor.Get()
		if !ok || string(next) == cursor {
			return revoked, fmt.Errorf("channels：identity 对账分页缺少下一页游标")
		}
		cursor = string(next)
	}
}

type bindingEvent struct {
	BindingID string `json:"binding_id"`
	Version   int64  `json:"version"`
	State     string `json:"state"`
}

// BindingChangedEvent 消费本子系统的 channels.binding.changed.v1（经发件箱）：失效中的绑定重试同步撤销。按事件版本撤销，
// 迟到的旧事件只会把水位抬到已达到的版本（identity 只升不降）。identity 不可用时返回错误，由消费者重试。
func (s *Service) BindingChangedEvent(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var data bindingEvent
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return err
	}
	if data.State != domain.Revoking {
		return nil
	}
	if env.Tenant == "" {
		return fmt.Errorf("channels：绑定事件 %s 缺少租户", env.ID)
	}
	if err := s.revokeInIdentity(ctx, data.BindingID, data.Version); err != nil {
		return err
	}
	return s.recordConfirmed(ctx, tx, data.BindingID, data.Version)
}

// MemberChanged 消费 identity.member.changed.v1：成员停用或退出时，其全部有效绑定以 member_left 失效，固定到该成员的未完成
// 绑定请求作废。撤销经各绑定的失效事件送达（identity 也会因成员无效拒绝其免登会话）。
func (s *Service) MemberChanged(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var data struct {
		TenantID string `json:"tenant_id"`
		Subject  string `json:"subject"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return err
	}
	if data.State == "active" {
		return nil
	}
	if env.Tenant == "" || env.Cell == "" || env.Tenant != data.TenantID {
		// 不带租户上下文无法按行级安全定位绑定；不静默跳过
		return fmt.Errorf("channels：成员事件 %s 的租户上下文缺失或不一致", env.ID)
	}
	if _, err := tx.Exec(ctx, `UPDATE chn_binding_requests SET state = 'voided', void_reason = 'member_left', updated_at = now()
 WHERE pinned_subject = $1 AND state IN ('initiated', 'pinned')`, data.Subject); err != nil {
		return err
	}
	_, err := s.invalidate(ctx, tx, `subject = $1`, []any{data.Subject}, "member_left", "")
	return err
}
