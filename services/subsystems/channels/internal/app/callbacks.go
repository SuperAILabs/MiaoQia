package app

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/domain"
)

// 渠道侧入口（PRD 第1段 F5 第1–2步、第9节）：回调先验签，再按渠道事件标识去重并核对时间窗；验签失败、重放与超出时间窗的回调
// 丢弃并计入渠道健康（企业路由上的计数），不改变任何业务状态。未绑定的渠道账号只收到绑定引导（一次性绑定链接），不回答业务内容。

// corpRoute 是已授权且未停用的安装所属企业 → 租户。
type corpRoute struct {
	tenant, cell, installation string
}

// route 查找企业路由（不带租户隔离的表）；没有时返回 nil。查询失败返回错误，不当作“未安装”。
func (s *Service) route(ctx context.Context, corpID string) (*corpRoute, error) {
	var r *corpRoute
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var v corpRoute
		err := tx.QueryRow(ctx, `SELECT tenant_id, cell, installation_id FROM chn_corp_routes WHERE channel = $1 AND corp_id = $2`, Channel, corpID).Scan(&v.tenant, &v.cell, &v.installation)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		r = &v
		return nil
	})
	return r, err
}

// rejectCallback 把被拒绝的回调计入该企业的渠道健康（企业未登记时只记日志）。
func (s *Service) rejectCallback(ctx context.Context, corpID, reason string) {
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE chn_corp_routes SET rejected_callbacks = rejected_callbacks + 1, last_rejected_at = now() WHERE channel = $1 AND corp_id = $2`, Channel, corpID)
		return err
	})
	s.logger().WarnContext(ctx, "渠道回调被拒绝", "channel", Channel, "corp", corpID, "reason", reason, "record_error", err)
}

// boundAccount 读取渠道账号的有效绑定（不加锁）；没有时返回 nil。
func (s *Service) boundAccount(ctx context.Context, corpID, userID string) (*binding, error) {
	var out *binding
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		b, err := scanBinding(tx.QueryRow(ctx, `SELECT `+bindingColumns+` FROM chn_bindings WHERE channel = $1 AND corp_id = $2 AND channel_user_id = $3 AND state = 'bound'`,
			Channel, corpID, userID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out = b
		return err
	})
	return out, err
}

// ReceiveInbound 处理一次消息回调（Callbacks）。
func (s *Service) ReceiveInbound(ctx context.Context, channel string, cb *api.Callback) (*api.InboundResult, error) {
	if channel != Channel || cb == nil || cb.CorpID == "" || cb.EventID == "" {
		return nil, failure(400, "invalid_request")
	}
	ev, err := s.provider().VerifyCallback(ctx, cb)
	if errors.Is(err, api.ErrBadSignature) {
		s.rejectCallback(ctx, cb.CorpID, "bad_signature")
		return nil, failure(401, "callback_signature_invalid")
	}
	if err != nil {
		return nil, failure(503, "channel_unavailable")
	}
	if ev.CorpID != cb.CorpID || ev.EventID != cb.EventID {
		s.rejectCallback(ctx, cb.CorpID, "inconsistent_event")
		return nil, failure(401, "callback_signature_invalid")
	}
	if d := s.now().Sub(ev.At); d > domain.CallbackWindow || d < -domain.CallbackWindow {
		s.rejectCallback(ctx, cb.CorpID, "outside_window")
		return nil, failure(401, "callback_replayed")
	}
	r, err := s.route(ctx, ev.CorpID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return &api.InboundResult{Outcome: api.InboundIgnored}, nil
	}
	out := &api.InboundResult{}
	var token string
	tctx := serviceContext(ctx, r.tenant, r.cell)
	// 账号已有有效绑定时，先经 identity 核对它仍属于成员当前的成员资格（成员退出后重新加入、退出事件尚未送达时是旧绑定）。
	// identity 不可用时失败关闭：返回 503 且不写入任何状态（含去重记录），渠道重发后再处理；不当作已绑定，也不发绑定引导。
	pre, err := s.boundAccount(tctx, ev.CorpID, ev.UserID)
	if err != nil {
		return nil, err
	}
	current := false
	if pre != nil {
		if current, err = s.bindingCurrent(tctx, pre); err != nil {
			return nil, err
		}
	}
	var retired []*binding
	err = s.tx(tctx, func(ctx context.Context, tx pgx.Tx) error {
		// 去重与业务写入在同一事务中：处理失败时事件可由渠道重发
		tag, err := tx.Exec(ctx, `INSERT INTO chn_callback_events (channel, corp_id, event_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, Channel, ev.CorpID, ev.EventID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			out.Outcome = api.InboundDuplicate
			return nil
		}
		var state string
		err = tx.QueryRow(ctx, `SELECT state FROM chn_installations WHERE id = $1 FOR SHARE`, r.installation).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			out.Outcome = api.InboundIgnored
			return nil
		}
		if err != nil {
			return err
		}
		if state != "pending_verification" && state != "enabled" {
			out.Outcome = api.InboundIgnored
			return nil
		}
		b, err := scanBinding(tx.QueryRow(ctx, `SELECT `+bindingColumns+` FROM chn_bindings WHERE channel = $1 AND corp_id = $2 AND channel_user_id = $3 AND state = 'bound' FOR UPDATE`,
			Channel, ev.CorpID, ev.UserID))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		case pre == nil || b.id != pre.id || b.memberVersion != pre.memberVersion:
			// 核对之后绑定发生了变化：回滚，由渠道重发后重新核对
			return failure(503, "channel_state_changed")
		case current:
			// 已绑定：交给会话处理（渠道内对话尚未实现）
			out.Outcome = api.InboundBound
			return nil
		default:
			// 退出前的旧绑定：以 member_left 失效，随后按未绑定发出绑定引导（须重新绑定）
			if retired, err = s.invalidate(ctx, tx, `id = $1 AND member_version = $2`, []any{b.id, b.memberVersion}, "member_left", ""); err != nil {
				return err
			}
		}
		// 未绑定：发出新的一次性绑定链接；同一账号此前尚未打开的链接作废
		if _, err := tx.Exec(ctx, `UPDATE chn_binding_requests SET state = 'voided', void_reason = 'superseded', updated_at = now()
 WHERE corp_id = $1 AND channel_user_id = $2 AND state = 'initiated'`, ev.CorpID, ev.UserID); err != nil {
			return err
		}
		var digest string
		token, digest, err = newToken()
		if err != nil {
			return err
		}
		id := ids.New("cbr")
		if _, err := tx.Exec(ctx, `INSERT INTO chn_binding_requests (id, tenant_id, installation_id, channel, corp_id, channel_user_id, display_name, token_digest, state, expires_at)
 VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, 'initiated', $9)`, id, r.tenant, r.installation, Channel, ev.CorpID, ev.UserID, ev.Name, digest, s.businessNow().Add(domain.BindingLinkTTL)); err != nil {
			return err
		}
		out.Outcome = api.InboundGuideSent
		return record(ctx, tx, "channels.binding_request.created", id, "channel.inbound")
	})
	if err != nil {
		return nil, err
	}
	for _, b := range retired {
		s.revokeAfterCommit(ctx, r.tenant, r.cell, b)
	}
	if out.Outcome == api.InboundDuplicate {
		s.rejectCallback(ctx, ev.CorpID, "duplicate")
	}
	if token != "" {
		if err := s.provider().Send(ctx, ev.CorpID, ev.UserID, api.Message{Kind: "binding_guide", Text: "请先绑定妙洽账号", URL: "/channels/bind?link=" + token}); err != nil {
			s.logger().WarnContext(ctx, "绑定引导发送失败", "corp", ev.CorpID, "error", err.Error())
		}
	}
	return out, nil
}
