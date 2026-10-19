package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	chn "github.com/superailabs/miaoqia/services/contracts/apis/channels"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/domain"
)

// 绑定（PRD 第1段 F5、F6）。绑定只在渠道内置浏览器中完成，同时需要“当前渠道身份”（渠道网页授权）与“妙洽登录”两项证明：
// 绑定链接记录发起它的渠道账号；首次以妙洽账号调用时请求固定到本次 Web 会话（令牌 sid）与成员；确认时三者与内容摘要须一致。
// 绑定的权威在本子系统：失效即时生效（revoking），之后不再签发免登凭据、不再投递；identity 确认撤销且最后一张凭据过期后才为 invalidated。

type binding struct {
	id, tenant, installation, channel, corpID, userID, displayName, subject, state, reason, note string
	version, confirmed                                                                           int64
	lastCredential                                                                               *time.Time
	boundAt                                                                                      time.Time
	revokingAt, invalidatedAt                                                                    *time.Time
	createdAt                                                                                    time.Time
}

const bindingColumns = `id, tenant_id, installation_id, channel, corp_id, channel_user_id, coalesce(display_name,''), subject, state, coalesce(reason,''), coalesce(note,''),
 version, revoke_confirmed_version, last_credential_expires_at, bound_at, revoking_at, invalidated_at, created_at`

func scanBinding(row pgx.Row) (*binding, error) {
	var b binding
	err := row.Scan(&b.id, &b.tenant, &b.installation, &b.channel, &b.corpID, &b.userID, &b.displayName, &b.subject, &b.state, &b.reason, &b.note,
		&b.version, &b.confirmed, &b.lastCredential, &b.boundAt, &b.revokingAt, &b.invalidatedAt, &b.createdAt)
	return &b, err
}

func (b *binding) out() *chn.Binding {
	o := &chn.Binding{ID: chn.BindingID(b.id), Channel: chn.ChannelKind(b.channel), InstallationID: chn.InstallationID(b.installation), Subject: b.subject,
		CorpID: b.corpID, ChannelUserID: b.userID, State: chn.BindingState(b.state), Version: b.version, BoundAt: b.boundAt.UTC()}
	if b.displayName != "" {
		o.DisplayName = chn.NewOptString(b.displayName)
	}
	if b.reason != "" {
		o.Reason = chn.NewOptBindingReason(chn.BindingReason(b.reason))
	}
	if b.note != "" {
		o.Note = chn.NewOptString(b.note)
	}
	if b.revokingAt != nil {
		o.RevokingAt = chn.NewOptDateTime(b.revokingAt.UTC())
	}
	if b.invalidatedAt != nil {
		o.InvalidatedAt = chn.NewOptDateTime(b.invalidatedAt.UTC())
	}
	return o
}

// invalidate 在事务中使符合条件的 bound 绑定进入失效中：版本加一、记录原因与失效时间，各发布一条 channels.binding.changed.v1。
// 返回失效的绑定（提交后由调用方同步撤销，失败由事件与定时重试送达）。
func (s *Service) invalidate(ctx context.Context, tx pgx.Tx, where string, args []any, reason, note string) ([]*binding, error) {
	n := len(args)
	args = append(args, reason, s.businessNow())
	q := `UPDATE chn_bindings SET state = 'revoking', version = version + 1, reason = $` + strconv.Itoa(n+1) + `, revoking_at = $` + strconv.Itoa(n+2) + `, updated_at = now()`
	if note != "" {
		args = append(args, note)
		q += `, note = $` + strconv.Itoa(n+3)
	}
	rows, err := tx.Query(ctx, q+` WHERE state = 'bound' AND `+where+` RETURNING `+bindingColumns, args...)
	if err != nil {
		return nil, err
	}
	var out []*binding
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, b := range out {
		if err := emitBinding(ctx, tx, b, reason); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// bindingRequest 是一次性绑定链接。
type bindingRequest struct {
	id, tenant, installation, channel, corpID, userID, displayName, state string
	pinnedSubject, pinnedSession, bindingID                               string
	result                                                                []byte
	expiresAt                                                             time.Time
}

func scanRequest(row pgx.Row) (*bindingRequest, error) {
	var r bindingRequest
	err := row.Scan(&r.id, &r.tenant, &r.installation, &r.channel, &r.corpID, &r.userID, &r.displayName, &r.state, &r.pinnedSubject, &r.pinnedSession, &r.bindingID, &r.result, &r.expiresAt)
	return &r, err
}

const requestColumns = `id, tenant_id, installation_id, channel, corp_id, channel_user_id, coalesce(display_name,''), state, coalesce(pinned_subject,''), coalesce(pinned_session,''), coalesce(binding_id,''), result, expires_at`

// linkCheck 是打开与确认绑定链接共同的前置核验结果。
type linkCheck struct {
	rc      reqctx.Context
	session string
	who     *api.ChannelIdentity
}

// checkLink 核验调用者与当前渠道身份（不访问链接）：有效成员、交互登录的 Web 会话（sid）、渠道网页授权取得的账号所在企业
// 是本租户已安装且未停用的企业。渠道网页授权不可用是 503（不可用不等于不一致）。
func (s *Service) checkLink(ctx context.Context, oauthCode string) (*linkCheck, error) {
	rc, err := s.member(ctx)
	if err != nil {
		return nil, err
	}
	a := reqctx.AssuranceOf(ctx)
	if a.Source != reqctx.SourceIdP || a.SessionID == "" {
		// 免登会话与没有 Web 会话标识的令牌不能用来绑定：绑定要求本人在内置浏览器中以妙洽账号登录
		return nil, failure(403, "interactive_login_required")
	}
	if oauthCode == "" {
		return nil, failure(400, "channel_browser_required")
	}
	who, err := s.provider().ResolveOAuthCode(ctx, oauthCode)
	if errors.Is(err, api.ErrInvalidCode) {
		return nil, failure(401, "channel_oauth_invalid")
	}
	if err != nil {
		return nil, failure(503, "channel_oauth_unavailable")
	}
	r, err := s.route(ctx, who.CorpID)
	if err != nil {
		return nil, err
	}
	if r == nil || r.tenant != rc.Tenant {
		return nil, failure(403, "channel_tenant_mismatch")
	}
	return &linkCheck{rc: rc, session: a.SessionID, who: who}, nil
}

// openRequest 在事务中锁定链接对应的绑定请求并按状态机核验；需要作废时写入作废并返回 voided 错误（由调用方在提交后返回）。
// completed 的请求原样返回，由调用方区分重试与重放。
func (s *Service) openRequest(ctx context.Context, tx pgx.Tx, token string, c *linkCheck) (*bindingRequest, error, error) {
	r, err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM chn_binding_requests WHERE token_digest = $1 FOR UPDATE`, tokenDigest(token)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, failure(404, "binding_link_not_found")
	}
	if err != nil {
		return nil, nil, err
	}
	void := func(reason string, cause error) (*bindingRequest, error, error) {
		if _, err := tx.Exec(ctx, `UPDATE chn_binding_requests SET state = 'voided', void_reason = $2, updated_at = now() WHERE id = $1`, r.id, reason); err != nil {
			return nil, nil, err
		}
		if err := record(ctx, tx, "channels.binding_request.voided", r.id+"/"+reason, "channels.binding"); err != nil {
			return nil, nil, err
		}
		return nil, cause, nil
	}
	switch r.state {
	case domain.RequestVoided:
		return nil, nil, failure(410, "binding_link_voided")
	case domain.RequestCompleted:
		return r, nil, nil
	}
	if !s.now().Before(r.expiresAt) {
		return void("expired", failure(410, "binding_link_expired"))
	}
	// 当前渠道账号必须与链接记录的渠道账号一致（链接被转发到他人的企业微信时在这里失败）
	if c.who.CorpID != r.corpID || c.who.UserID != r.userID {
		return void("channel_identity_mismatch", failure(403, "channel_identity_mismatch"))
	}
	if r.state == domain.RequestInitiated {
		if _, err := tx.Exec(ctx, `UPDATE chn_binding_requests SET state = 'pinned', pinned_subject = $2, pinned_session = $3, updated_at = now() WHERE id = $1`,
			r.id, c.rc.Principal.ID, c.session); err != nil {
			return nil, nil, err
		}
		r.state, r.pinnedSubject, r.pinnedSession = domain.RequestPinned, c.rc.Principal.ID, c.session
		return r, nil, nil
	}
	// 已固定：会话被替换、退出后以他人登录，确认均被拒绝，链接作废
	if r.pinnedSubject != c.rc.Principal.ID || r.pinnedSession != c.session {
		return void("session_mismatch", failure(409, "binding_session_mismatch"))
	}
	return r, nil, nil
}

func (r *bindingRequest) confirmation() string {
	return domain.Confirmation(r.id, r.channel, r.corpID, r.userID, r.tenant, r.pinnedSubject, r.pinnedSession)
}

// PrepareBinding 打开绑定链接：核验渠道身份与链接，首次调用时固定会话，返回确认页内容与内容摘要。
func (s *Service) PrepareBinding(ctx context.Context, req *chn.BindingLinkRequest) (*chn.BindingPreview, error) {
	if req == nil || req.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	c, err := s.checkLink(ctx, string(req.OAuthCode.Or("")))
	if err != nil {
		return nil, err
	}
	var out *chn.BindingPreview
	var deferred error
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		r, voided, err := s.openRequest(ctx, tx, string(req.LinkToken), c)
		if err != nil || voided != nil {
			deferred = voided
			return err
		}
		if r.state == domain.RequestCompleted {
			return failure(410, "binding_link_used")
		}
		var corpName string
		if err := tx.QueryRow(ctx, `SELECT coalesce(corp_name,'') FROM chn_installations WHERE id = $1`, r.installation).Scan(&corpName); err != nil {
			return err
		}
		out = &chn.BindingPreview{Channel: chn.ChannelKind(r.channel), CorpID: r.corpID, ChannelUserID: r.userID, Subject: r.pinnedSubject,
			ExpiresAt: r.expiresAt.UTC(), Confirmation: chn.Confirmation(r.confirmation())}
		if corpName != "" {
			out.CorpName = chn.NewOptString(corpName)
		}
		if r.displayName != "" {
			out.DisplayName = chn.NewOptString(r.displayName)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if deferred != nil {
		return nil, deferred
	}
	return out, nil
}

// ConfirmBinding 确认绑定（BindAccount）。见 contracts/apis/channels/openapi.yaml confirmBinding。
func (s *Service) ConfirmBinding(ctx context.Context, req *chn.ConfirmBindingRequest) (*chn.BindingConfirmation, error) {
	if req == nil || req.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	c, err := s.checkLink(ctx, string(req.OAuthCode.Or("")))
	if err != nil {
		return nil, err
	}
	var out *chn.BindingConfirmation
	var deferred error
	var created *binding
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		r, voided, err := s.openRequest(ctx, tx, string(req.LinkToken), c)
		if err != nil || voided != nil {
			deferred = voided
			return err
		}
		if r.state == domain.RequestCompleted {
			// 同一请求、同一固定会话与成员、同一渠道身份、内容未变的重试（例如首次成功但响应丢失）：只读取并返回原结果，
			// 不新建绑定、不再通知；绑定已不再有效、或任何一项不同都按重放拒绝
			same := r.pinnedSubject == c.rc.Principal.ID && r.pinnedSession == c.session && c.who.CorpID == r.corpID && c.who.UserID == r.userID &&
				string(req.Confirmation) == r.confirmation()
			if !same {
				return failure(410, "binding_link_used")
			}
			var state string
			if err := tx.QueryRow(ctx, `SELECT state FROM chn_bindings WHERE id = $1`, r.bindingID).Scan(&state); err != nil {
				return err
			}
			if state != domain.Bound {
				return failure(410, "binding_link_used")
			}
			out = &chn.BindingConfirmation{}
			if err := json.Unmarshal(r.result, out); err != nil {
				return err
			}
			out.Outcome = chn.BindingConfirmationOutcomeAlreadyBound
			return nil
		}
		if string(req.Confirmation) != r.confirmation() {
			return failure(409, "binding_request_changed")
		}
		var installationState string
		if err := tx.QueryRow(ctx, `SELECT state FROM chn_installations WHERE id = $1 FOR SHARE`, r.installation).Scan(&installationState); err != nil {
			return err
		}
		if installationState != "pending_verification" && installationState != "enabled" {
			return failure(409, "channel_not_enabled")
		}
		// 绑定规则：一个渠道账号只绑定一个成员，一个成员在同一渠道只绑定一个账号（唯一索引兜底并发）
		var accountTaken, memberTaken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chn_bindings WHERE channel = $1 AND corp_id = $2 AND channel_user_id = $3 AND state = 'bound'),
 EXISTS (SELECT 1 FROM chn_bindings WHERE channel = $1 AND subject = $4 AND state = 'bound')`, r.channel, r.corpID, r.userID, r.pinnedSubject).Scan(&accountTaken, &memberTaken); err != nil {
			return err
		}
		if accountTaken {
			return failure(409, "channel_account_already_bound")
		}
		if memberTaken {
			return failure(409, "member_already_bound")
		}
		b := &binding{id: ids.New("chb"), tenant: r.tenant, installation: r.installation, channel: r.channel, corpID: r.corpID, userID: r.userID,
			displayName: r.displayName, subject: r.pinnedSubject, state: domain.Bound, version: 1, boundAt: s.businessNow()}
		if _, err := tx.Exec(ctx, `INSERT INTO chn_bindings (id, tenant_id, installation_id, channel, corp_id, channel_user_id, display_name, subject, state, version, bound_at)
 VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, 'bound', 1, $9)`, b.id, b.tenant, b.installation, b.channel, b.corpID, b.userID, b.displayName, b.subject, b.boundAt); err != nil {
			if name, ok := uniqueViolation(err); ok {
				if name == "chn_bindings_member" {
					return failure(409, "member_already_bound")
				}
				return failure(409, "channel_account_already_bound")
			}
			return err
		}
		out = &chn.BindingConfirmation{Binding: *b.out(), Outcome: chn.BindingConfirmationOutcomeBound}
		result, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE chn_binding_requests SET state = 'completed', binding_id = $2, result = $3, updated_at = now() WHERE id = $1`, r.id, b.id, result); err != nil {
			return err
		}
		created = b
		return emitBinding(ctx, tx, b, "channels.binding")
	})
	if err != nil {
		return nil, err
	}
	if deferred != nil {
		return nil, deferred
	}
	if created != nil {
		// 提交后在渠道内通知“绑定成功”；发送失败不回滚绑定
		if err := s.provider().Send(ctx, created.corpID, created.userID, api.Message{Kind: "binding_succeeded", Text: "绑定成功"}); err != nil {
			s.logger().WarnContext(ctx, "绑定成功通知发送失败", "binding", created.id, "error", err.Error())
		}
	}
	return out, nil
}

// bindingCursor 是绑定列表的翻页位置：上一页最后一条的 (created_at, id)，以及签发它的租户、可见范围与 state 过滤的摘要。
type bindingCursor struct {
	CreatedAt int64  `json:"t"`
	ID        string `json:"i"`
	Filter    string `json:"f"`
}

func bindingFilter(tenant, scope, state string) string {
	sum := sha256.Sum256([]byte(tenant + "\x00" + scope + "\x00" + state))
	return hex.EncodeToString(sum[:8])
}

const maxListedBindings = 500

// ListBindings 分页列出绑定：管理员看到本租户全部，其他成员只看到本人的（均不要求强认证）。
func (s *Service) ListBindings(ctx context.Context, params chn.ListBindingsParams) (*chn.BindingList, error) {
	if l, ok := params.Limit.Get(); ok && (l < 1 || l > maxListedBindings) {
		return nil, failure(400, "invalid_request")
	}
	if st, ok := params.State.Get(); ok && st.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, err := s.member(ctx)
	if err != nil {
		return nil, err
	}
	admin, err := s.allowed(ctx, rc, "can_manage_members", "tenant:"+rc.Tenant)
	if err != nil {
		return nil, err
	}
	scope := "*"
	if !admin {
		scope = rc.Principal.ID
	}
	state := string(params.State.Or(""))
	filter := bindingFilter(rc.Tenant, scope, state)
	limit := params.Limit.Or(maxListedBindings)
	var after *bindingCursor
	if c, ok := params.Cursor.Get(); ok {
		raw, err := base64.RawURLEncoding.DecodeString(string(c))
		var cur bindingCursor
		if err != nil || json.Unmarshal(raw, &cur) != nil || chn.BindingID(cur.ID).Validate() != nil || cur.Filter != filter {
			return nil, failure(400, "invalid_cursor")
		}
		after = &cur
	}
	out := &chn.BindingList{Bindings: []chn.Binding{}}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := `SELECT ` + bindingColumns + ` FROM chn_bindings WHERE true`
		var args []any
		if scope != "*" {
			args = append(args, scope)
			q += ` AND subject = $` + strconv.Itoa(len(args))
		}
		if state != "" {
			args = append(args, state)
			q += ` AND state = $` + strconv.Itoa(len(args))
		}
		if after != nil {
			args = append(args, time.UnixMicro(after.CreatedAt).UTC(), after.ID)
			q += ` AND (created_at, id) < ($` + strconv.Itoa(len(args)-1) + `, $` + strconv.Itoa(len(args)) + `)`
		}
		rows, err := tx.Query(ctx, q+` ORDER BY created_at DESC, id DESC LIMIT `+strconv.Itoa(limit+1), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		var list []*binding
		for rows.Next() {
			b, err := scanBinding(rows)
			if err != nil {
				return err
			}
			list = append(list, b)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(list) > limit {
			list, out.Truncated = list[:limit], true
			last := list[limit-1]
			raw, _ := json.Marshal(bindingCursor{CreatedAt: last.createdAt.UnixMicro(), ID: last.id, Filter: filter})
			out.NextCursor = chn.NewOptBindingCursor(chn.BindingCursor(base64.RawURLEncoding.EncodeToString(raw)))
		}
		for _, b := range list {
			out.Bindings = append(out.Bindings, *b.out())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UnbindAccount 解绑：绑定的成员本人（不要求强认证）或管理员强制解绑（强认证并记录原因）。
func (s *Service) UnbindAccount(ctx context.Context, id string, req *chn.UnbindRequest, key string) (*chn.Binding, error) {
	if req == nil || req.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, err := s.member(ctx)
	if err != nil {
		return nil, err
	}
	var owner string
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT subject FROM chn_bindings WHERE id = $1`, id).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(404, "binding_not_found")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	reason, note := "member_unbind", ""
	if owner != rc.Principal.ID {
		// 强制解绑：非管理员看到的与不存在相同，不泄露他人绑定
		if err := s.requireAdmin(ctx); err != nil {
			var safe *api.Error
			if errors.As(err, &safe) && safe.Code == "forbidden" {
				return nil, failure(404, "binding_not_found")
			}
			return nil, err
		}
		reason = "admin_unbind"
		note = req.Reason.Or("")
		if note == "" {
			return nil, failure(400, "reason_required")
		}
	}
	out := &chn.Binding{}
	var revoked []*binding
	var fresh bool
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		fresh, err = command(ctx, tx, key, "binding.unbind/"+id, req, out, func() error {
			b, err := scanBinding(tx.QueryRow(ctx, `SELECT `+bindingColumns+` FROM chn_bindings WHERE id = $1 FOR UPDATE`, id))
			if errors.Is(err, pgx.ErrNoRows) {
				return failure(404, "binding_not_found")
			}
			if err != nil {
				return err
			}
			if b.subject != owner {
				return failure(409, "version_conflict")
			}
			if b.version != req.ExpectedVersion {
				return failure(409, "version_conflict")
			}
			if b.state != domain.Bound {
				return failure(409, "binding_not_bound")
			}
			revoked, err = s.invalidate(ctx, tx, `id = $1`, []any{id}, reason, note)
			if err != nil {
				return err
			}
			if len(revoked) != 1 {
				return failure(409, "binding_not_bound")
			}
			*out = *revoked[0].out()
			return nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if fresh {
		s.revokeAfterCommit(ctx, rc.Tenant, rc.Cell, revoked[0])
		if current, err := s.readBinding(ctx, id); err == nil {
			return current.out(), nil
		}
	}
	return out, nil
}

// readBinding 读取本租户的绑定（不加锁）。
func (s *Service) readBinding(ctx context.Context, id string) (*binding, error) {
	var b *binding
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = scanBinding(tx.QueryRow(ctx, `SELECT `+bindingColumns+` FROM chn_bindings WHERE id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(404, "binding_not_found")
		}
		return err
	})
	return b, err
}
