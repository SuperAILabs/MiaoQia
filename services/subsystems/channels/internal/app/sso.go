package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	chn "github.com/superailabs/miaoqia/services/contracts/apis/channels"
	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/domain"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// 免登（PRD 第1段 F6 第1–3步、F7 第2步）：在渠道内置浏览器中打开妙洽链接时，经渠道网页授权取得当前渠道账号，在本子系统的
// 绑定表中确认绑定有效，并在同一事务、绑定行锁下记录本次签发凭据的到期时间（签发后30秒）；随后请 identity 以该凭据签发免登会话。
// 解绑等失效先锁同一绑定行：要么失效在前（此处看到 revoking，拒绝），要么凭据记录在前（失效看得到它，等它过期才算完成）。
// IdP 中不保存长期的渠道身份关联。任何不可用都转登录页并说明原因，不会被当作“未绑定”。

// ChannelSSO 是 Callbacks.ChannelSSO。
func (s *Service) ChannelSSO(ctx context.Context, channel, oauthCode string) (*api.SSOResult, error) {
	login := func(reason string) (*api.SSOResult, error) {
		return &api.SSOResult{Outcome: "login", Reason: reason}, nil
	}
	if channel != Channel || oauthCode == "" {
		return nil, failure(400, "invalid_request")
	}
	who, err := s.provider().ResolveOAuthCode(ctx, oauthCode)
	if errors.Is(err, api.ErrInvalidCode) {
		return login("channel_oauth_invalid")
	}
	if err != nil {
		return login("channel_oauth_unavailable")
	}
	r, err := s.route(ctx, who.CorpID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return login("channel_not_installed")
	}
	tctx := serviceContext(ctx, r.tenant, r.cell)
	credential, err := credentialID()
	if err != nil {
		return nil, err
	}
	expires := s.businessNow().Add(domain.CredentialLifetime)
	var b *binding
	reason := ""
	err = s.tx(tctx, func(ctx context.Context, tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM chn_installations WHERE id = $1`, r.installation).Scan(&state); err != nil {
			return err
		}
		if state != "enabled" {
			reason = "channel_not_installed"
			return nil
		}
		found, err := scanBinding(tx.QueryRow(ctx, `SELECT `+bindingColumns+` FROM chn_bindings WHERE channel = $1 AND corp_id = $2 AND channel_user_id = $3 AND state = 'bound' FOR UPDATE`,
			Channel, who.CorpID, who.UserID))
		if errors.Is(err, pgx.ErrNoRows) {
			var revoking bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chn_bindings WHERE channel = $1 AND corp_id = $2 AND channel_user_id = $3 AND state = 'revoking')`,
				Channel, who.CorpID, who.UserID).Scan(&revoking); err != nil {
				return err
			}
			reason = "not_bound"
			if revoking {
				reason = "binding_inactive"
			}
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE chn_bindings SET last_credential_expires_at = GREATEST(coalesce(last_credential_expires_at, $2), $2), updated_at = now() WHERE id = $1`,
			found.id, expires); err != nil {
			return err
		}
		b = found
		return record(ctx, tx, "channels.binding.credential_issued", found.id+"/"+credential, "channels.sso")
	})
	if err != nil {
		return login("channels_unavailable")
	}
	if reason != "" {
		return login(reason)
	}
	// 凭据只在有效期内交给 identity：调用（含暂时性失败的重试）在“到期前5秒”截止，之后 identity 必然拒绝
	cctx, cancel := context.WithTimeout(tctx, domain.IssuableUntil(expires).Sub(s.now()))
	defer cancel()
	if s.Sessions == nil {
		return login("identity_unavailable")
	}
	session, err := s.Sessions.IssueChannelSession(cctx, &identityapi.IssueChannelSessionRequest{CredentialID: credential, BindingID: identityapi.ChannelBindingID(b.id),
		BindingVersion: b.version, Subject: b.subject, MemberVersion: b.memberVersion, CredentialExpiresAt: expires})
	if err != nil {
		var safe *identity.Error
		switch {
		case errors.As(err, &safe) && safe.Code == "member_inactive":
			return login("member_inactive")
		case errors.As(err, &safe) && safe.Code == "member_rejoined":
			// 绑定建立后成员退出过（之后又重新加入）：这是退出前的旧绑定，立即以 member_left 失效（不等成员事件），须重新绑定
			s.retireStaleBinding(tctx, r, b)
			return login("binding_inactive")
		case errors.As(err, &safe) && safe.Status < 500:
			return login("session_rejected")
		default:
			return login("identity_unavailable")
		}
	}
	// 签发返回后再核对一次：其间绑定已失效时不交出会话（该会话由撤销作废）
	current, err := s.readBinding(tctx, b.id)
	if err != nil || current.state != domain.Bound {
		return login("binding_inactive")
	}
	return &api.SSOResult{Outcome: "session", Tenant: r.tenant, Subject: b.subject, Session: session}, nil
}

// retireStaleBinding 使退出前的旧绑定以 member_left 失效（只在它仍是同一成员版本的有效绑定时），提交后同步撤销。
// 失败只记录：identity 对它的签发与会话核验都已拒绝，成员事件送达时同样会使它失效。
func (s *Service) retireStaleBinding(ctx context.Context, r *corpRoute, b *binding) {
	var retired []*binding
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		retired, err = s.invalidate(ctx, tx, `id = $1 AND member_version = $2`, []any{b.id, b.memberVersion}, "member_left", "")
		return err
	})
	if err != nil {
		s.logger().WarnContext(ctx, "旧绑定失效未完成", "binding", b.id, "error", err.Error())
		return
	}
	for _, x := range retired {
		s.revokeAfterCommit(ctx, r.tenant, r.cell, x)
	}
}

// credentialID 生成签发凭据标识（24字节随机数的 base64url）。
func credentialID() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

var cardObject = regexp.MustCompile(`^(objective:obj|pursuit:pur|research_case:rsc)_[0-9A-HJKMNP-TV-Z]{26}$`)

// CreateCardLink 是 Messaging.CreateCardLink：以带租户的服务主体为收件人与对象生成卡片详情链接（72小时有效）。
func (s *Service) CreateCardLink(ctx context.Context, recipient, object string) (*api.CardLink, error) {
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Validate() != nil || rc.Tenant == "" || rc.Principal.Kind != "service" {
		return nil, failure(403, "forbidden")
	}
	if recipient == "" || !cardObject.MatchString(object) {
		return nil, failure(400, "invalid_request")
	}
	token, digest, err := newToken()
	if err != nil {
		return nil, err
	}
	link := &api.CardLink{ID: ids.New("lnk"), Token: token, Recipient: recipient, Object: object, ExpiresAt: s.businessNow().Add(domain.CardLinkTTL)}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO chn_card_links (id, tenant_id, token_digest, recipient, object, expires_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			link.ID, rc.Tenant, digest, recipient, object, link.ExpiresAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return link, nil
}

// OpenCardLink 以当前成员打开卡片详情链接（F7 第4–5步）：不切换账号；不存在、其他租户、对象不存在与无权限是同一个 404。
func (s *Service) OpenCardLink(ctx context.Context, req *chn.OpenCardLinkRequest) (*chn.CardTarget, error) {
	if req == nil || req.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, err := s.member(ctx)
	if err != nil {
		return nil, err
	}
	var recipient, object string
	var expires time.Time
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT recipient, object, expires_at FROM chn_card_links WHERE token_digest = $1`, tokenDigest(string(req.Token))).Scan(&recipient, &object, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(404, "content_unavailable")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if recipient != rc.Principal.ID {
		return nil, &api.Error{Status: 409, Code: "account_mismatch", Recipient: recipient}
	}
	if !s.now().Before(expires) {
		return nil, failure(410, "card_link_expired")
	}
	ok, err := s.allowed(ctx, rc, "can_view", object)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, failure(404, "content_unavailable")
	}
	return &chn.CardTarget{Object: object}, nil
}

// DeliveryTarget 是 Messaging.DeliveryTarget：渠道已启用且绑定为 bound 时投到渠道，否则改投站内（PRD 第1段 F4 第4步、第9节）。
func (s *Service) DeliveryTarget(ctx context.Context, subject string) (*api.DeliveryTarget, error) {
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Validate() != nil || rc.Tenant == "" || rc.Principal.Kind != "service" {
		return nil, failure(403, "forbidden")
	}
	out := &api.DeliveryTarget{Channel: "in_app", Reason: "channel_not_enabled"}
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var installation string
		err := tx.QueryRow(ctx, `SELECT id FROM chn_installations WHERE channel = $1 AND state = 'enabled'`, Channel).Scan(&installation)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var id string
		err = tx.QueryRow(ctx, `SELECT id FROM chn_bindings WHERE installation_id = $1 AND subject = $2 AND state = 'bound'`, installation, subject).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			out.Reason = "not_bound"
			return nil
		}
		if err != nil {
			return err
		}
		*out = api.DeliveryTarget{Channel: Channel, BindingID: id}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
