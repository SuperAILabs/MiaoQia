package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// 渠道免登会话（PRD 第1段 F6、F7，第13节第1、3项）。channels 是绑定的权威：它核对绑定有效后签发一张30秒的签发凭据，
// identity 凭它签发不带强认证的免登会话。每个绑定一行撤销水位（identity_channel_revocations）：签发与撤销都在事务中锁定该行，
// 签发核验凭据未过期且绑定版本高于水位后写入会话，撤销抬高水位并作废版本不高于水位的会话，因此不存在“签发读到旧水位、
// 撤销随后完成、签发最后写入”的交错。网关对免登令牌每次请求经 ResolveChannelSession 核验会话、水位与成员状态。
const (
	// ChannelSessionTTL 是免登会话的绝对时长（不随使用延长）。
	ChannelSessionTTL = 12 * time.Hour
	// CredentialLifetime 是 channels 签发凭据的有效期；CredentialSkew 是双方按从严方向计入的时钟偏差：
	// identity 只接受早于“到期前5秒”到达的凭据，channels 在“到期后5秒”才认为最后一张凭据已过期。
	CredentialLifetime = 30 * time.Second
	CredentialSkew     = 5 * time.Second
	maxLiveBindings    = 500
)

// channelService 要求调用方是带有效租户上下文、有 can_issue_channel_sessions 服务授权的服务主体（只授予 channels）。
func (d *Directory) channelService(ctx context.Context) (reqctx.Context, error) {
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Validate() != nil || rc.Principal.Kind != "service" || rc.Tenant == "" {
		return rc, failure(403, "forbidden")
	}
	permitted, err := d.CheckService(ctx, "can_issue_channel_sessions", "tenant:"+rc.Tenant)
	if err != nil {
		return rc, err
	}
	if !permitted {
		return rc, failure(403, "forbidden")
	}
	return rc, nil
}

// lockWatermark 锁定绑定的撤销水位行（不存在时以水位0建立）并返回当前水位。绑定标识全局唯一；属于其他租户时 409 binding_conflict。
func lockWatermark(ctx context.Context, tx pgx.Tx, tenantID, bindingID string) (int64, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO identity_channel_revocations(binding_id,tenant_id,watermark) VALUES($1,$2,0) ON CONFLICT (binding_id) DO NOTHING`, bindingID, tenantID); err != nil {
		return 0, err
	}
	var owner string
	var watermark int64
	if err := tx.QueryRow(ctx, `SELECT tenant_id, watermark FROM identity_channel_revocations WHERE binding_id=$1 FOR UPDATE`, bindingID).Scan(&owner, &watermark); err != nil {
		return 0, err
	}
	if owner != tenantID {
		return 0, failure(409, "binding_conflict")
	}
	return watermark, nil
}

// leftVersion 是成员最近一次退出时的成员版本（从未退出或不存在时为 0）。绑定建立时的成员版本不高于它的绑定与会话
// 来自退出之前，重新加入不恢复它们（PRD 第1段 F6）。
func leftVersion(ctx context.Context, tx pgx.Tx, tenantID, subject string) (int64, error) {
	var left int64
	err := tx.QueryRow(ctx, `SELECT left_version FROM identity_members WHERE tenant_id=$1 AND subject=$2`, tenantID, subject).Scan(&left)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return left, err
}

// MemberLifecycle 是 api.ChannelSessions：成员当前状态、成员版本与最近一次退出时的成员版本（只读，不加锁）。
// 不是本租户成员时 state 为 absent。只接受 service:channels。
func (d *Directory) MemberLifecycle(ctx context.Context, subject string) (*identityapi.ChannelMemberLifecycle, error) {
	out := &identityapi.ChannelMemberLifecycle{Subject: subject, State: identityapi.ChannelMemberLifecycleStateAbsent}
	if out.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, err := d.channelService(ctx)
	if err != nil {
		return nil, err
	}
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var state string
		err := tx.QueryRow(ctx, `SELECT state, version, left_version FROM identity_members WHERE tenant_id=$1 AND subject=$2`, rc.Tenant, subject).
			Scan(&state, &out.MemberVersion, &out.LeftVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out.State = identityapi.ChannelMemberLifecycleState(state)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type storedSession struct {
	id, tenant, binding, subject string
	version, memberVersion       int64
	issuedAt, expiresAt          time.Time
	revoked                      bool
}

func (s *storedSession) out(outcome identityapi.ChannelSessionOutcome) *identityapi.ChannelSession {
	return &identityapi.ChannelSession{ID: identityapi.ChannelSessionID(s.id), BindingID: identityapi.ChannelBindingID(s.binding), BindingVersion: s.version,
		Subject: s.subject, IssuedAt: s.issuedAt.UTC(), ExpiresAt: s.expiresAt.UTC(), Outcome: outcome}
}

// IssueChannelSession 是 api.ChannelSessions：凭 channels 的签发凭据签发免登会话（identity.md 第5节）。
func (d *Directory) IssueChannelSession(ctx context.Context, req *identityapi.IssueChannelSessionRequest) (*identityapi.ChannelSession, error) {
	if req == nil || req.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, err := d.channelService(ctx)
	if err != nil {
		return nil, err
	}
	now := d.now()
	// 有效期长于30秒（加偏差）的凭据不是 channels 按规则签发的，拒绝
	if req.CredentialExpiresAt.After(now.Add(CredentialLifetime + CredentialSkew)) {
		return nil, failure(400, "invalid_credential")
	}
	var out *identityapi.ChannelSession
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		watermark, err := lockWatermark(ctx, tx, rc.Tenant, string(req.BindingID))
		if err != nil {
			return err
		}
		// 同一凭据的重试（例如 channels 侧超时后重试）：返回首次结果；该会话已被撤销时不返回已作废的会话
		left, err := leftVersion(ctx, tx, rc.Tenant, req.Subject)
		if err != nil {
			return err
		}
		var s storedSession
		err = tx.QueryRow(ctx, `SELECT id, tenant_id, binding_id, subject, binding_version, member_version, issued_at, expires_at, revoked_at IS NOT NULL
 FROM identity_channel_sessions WHERE credential_id=$1`, req.CredentialID).Scan(&s.id, &s.tenant, &s.binding, &s.subject, &s.version, &s.memberVersion, &s.issuedAt, &s.expiresAt, &s.revoked)
		if err == nil {
			if s.tenant != rc.Tenant || s.binding != string(req.BindingID) || s.version != req.BindingVersion || s.subject != req.Subject || s.memberVersion != req.MemberVersion {
				return failure(409, "credential_conflict")
			}
			if s.revoked || s.version <= watermark {
				return failure(409, "binding_revoked")
			}
			if s.memberVersion <= left {
				return failure(409, "member_rejoined")
			}
			out = s.out(identityapi.ChannelSessionOutcomeReplayed)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// 凭据到期按 identity 时钟从严判断：须早于“到期前5秒”。迟到的签发请求不会产生会话。
		if !d.now().Add(CredentialSkew).Before(req.CredentialExpiresAt) {
			return failure(410, "credential_expired")
		}
		if req.BindingVersion <= watermark {
			return failure(409, "binding_revoked")
		}
		member, err := session(ctx, tx, rc.Tenant, req.Subject, rc.Cell)
		if err != nil {
			var safe *api.Error
			if errors.As(err, &safe) {
				return failure(403, "member_inactive")
			}
			return err
		}
		// 成员退出是生命周期边界：绑定建立后成员退出过（即使已重新加入），这条绑定不能再签发会话，须重新绑定
		if req.MemberVersion <= left {
			return failure(409, "member_rejoined")
		}
		if req.MemberVersion > member.MemberVersion {
			return failure(400, "invalid_credential")
		}
		issued := d.businessNow()
		s = storedSession{id: ids.New("chs"), tenant: rc.Tenant, binding: string(req.BindingID), subject: req.Subject, version: req.BindingVersion,
			memberVersion: req.MemberVersion, issuedAt: issued, expiresAt: issued.Add(ChannelSessionTTL)}
		if _, err := tx.Exec(ctx, `INSERT INTO identity_channel_sessions(id,tenant_id,binding_id,binding_version,subject,member_version,credential_id,issued_at,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, s.id, s.tenant, s.binding, s.version, s.subject, s.memberVersion, req.CredentialID, s.issuedAt, s.expiresAt); err != nil {
			if uniqueViolation(err) {
				return failure(409, "credential_conflict")
			}
			return err
		}
		out = s.out(identityapi.ChannelSessionOutcomeIssued)
		return (audit.Recorder{Subsystem: api.Name}).Record(ctx, tx, audit.Entry{Action: "identity.channel_session.issued",
			Resource: fmt.Sprintf("%s/%s/v%d", s.id, s.binding, s.version), Result: "succeeded", Basis: "can_issue_channel_sessions"})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// revoke 在 tx 中把绑定的撤销水位抬高到 version（不降低），作废版本不高于水位的会话，返回撤销结果。
func revoke(ctx context.Context, tx pgx.Tx, tenantID, bindingID string, version int64, basis string) (*identityapi.ChannelSessionRevocation, error) {
	watermark, err := lockWatermark(ctx, tx, tenantID, bindingID)
	if err != nil {
		return nil, err
	}
	out := &identityapi.ChannelSessionRevocation{BindingID: identityapi.ChannelBindingID(bindingID), Outcome: identityapi.ChannelSessionRevocationOutcomeAlreadyApplied}
	if version > watermark {
		watermark = version
		out.Outcome = identityapi.ChannelSessionRevocationOutcomeApplied
		if _, err := tx.Exec(ctx, `UPDATE identity_channel_revocations SET watermark=$2, updated_at=now() WHERE binding_id=$1`, bindingID, watermark); err != nil {
			return nil, err
		}
	}
	out.Watermark = watermark
	tag, err := tx.Exec(ctx, `UPDATE identity_channel_sessions SET revoked_at=now() WHERE binding_id=$1 AND binding_version<=$2 AND revoked_at IS NULL`, bindingID, watermark)
	if err != nil {
		return nil, err
	}
	out.RevokedSessions = int(tag.RowsAffected())
	if out.Outcome == identityapi.ChannelSessionRevocationOutcomeApplied || out.RevokedSessions > 0 {
		if err := (audit.Recorder{Subsystem: api.Name}).Record(ctx, tx, audit.Entry{Action: "identity.channel_session.revoked",
			Resource: fmt.Sprintf("%s/v%d", bindingID, watermark), Result: "succeeded", Basis: basis}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RevokeChannelSessions 是 api.ChannelSessions：channels 同步撤销某绑定版本及以下的免登会话。
func (d *Directory) RevokeChannelSessions(ctx context.Context, req *identityapi.RevokeChannelSessionsRequest) (*identityapi.ChannelSessionRevocation, error) {
	if req == nil || req.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, err := d.channelService(ctx)
	if err != nil {
		return nil, err
	}
	var out *identityapi.ChannelSessionRevocation
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, err = revoke(ctx, tx, rc.Tenant, string(req.BindingID), req.BindingVersion, "can_issue_channel_sessions")
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ChannelBindingChanged 是 channels.binding.changed.v1 的消费处理（经发件箱，channels 同步撤销失败时的重试路径）：
// revoking 与 invalidated 把该绑定的水位抬高到事件版本并作废会话。水位只升不降，重复与迟到的旧事件不改变结果。
func (d *Directory) ChannelBindingChanged(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var data struct {
		BindingID string `json:"binding_id"`
		Version   int64  `json:"version"`
		State     string `json:"state"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return err
	}
	if data.State == "bound" {
		return nil
	}
	if env.Tenant == "" {
		return fmt.Errorf("identity：渠道绑定事件 %s 缺少租户", env.ID)
	}
	_, err := revoke(ctx, tx, env.Tenant, data.BindingID, data.Version, "channels.binding.changed.v1")
	return err
}

// ResolveChannelSession 是 api.ChannelSessionResolver：核验令牌中的免登会话（每次请求）。会话不存在、不属于令牌的租户与主体、
// 已作废、绑定版本不高于水位、来自成员最近一次退出之前或已过期时 401；成员已停用或退出时与普通会话相同（403 invalid_tenant_context）。
func (d *Directory) ResolveChannelSession(ctx context.Context, sessionID string) (*api.Session, error) {
	rc, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Tenant == "" || identityapi.ChannelSessionID(sessionID).Validate() != nil {
		return nil, failure(401, "channel_session_invalid")
	}
	var out *api.Session
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var tenantID, subject string
		var version, watermark, memberVersion, left int64
		var expires time.Time
		var revoked bool
		err := tx.QueryRow(ctx, `SELECT s.tenant_id, s.subject, s.binding_version, r.watermark, s.member_version, coalesce(m.left_version, 0), s.expires_at, s.revoked_at IS NOT NULL
 FROM identity_channel_sessions s JOIN identity_channel_revocations r ON r.binding_id=s.binding_id
 LEFT JOIN identity_members m ON m.tenant_id=s.tenant_id AND m.subject=s.subject WHERE s.id=$1`, sessionID).
			Scan(&tenantID, &subject, &version, &watermark, &memberVersion, &left, &expires, &revoked)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (tenantID != rc.Tenant || subject != rc.Principal.ID)) {
			return failure(401, "channel_session_invalid")
		}
		if err != nil {
			return err
		}
		// 已作废、绑定已失效，或会话来自成员退出之前（重新加入不恢复旧会话）
		if revoked || version <= watermark || memberVersion <= left {
			return failure(401, "channel_session_revoked")
		}
		if !d.now().Before(expires) {
			return failure(401, "channel_session_expired")
		}
		out, err = session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell)
		return err
	})
	return out, err
}

type liveCursor struct {
	Binding string `json:"b"`
	Filter  string `json:"f"`
}

func liveFilter(tenantID string) string {
	sum := sha256.Sum256([]byte("channel-sessions\x00" + tenantID))
	return hex.EncodeToString(sum[:8])
}

// ListLiveChannelSessions 是 api.ChannelSessions：按绑定标识升序分页列出仍有未作废、未过期会话的绑定（对账）。
func (d *Directory) ListLiveChannelSessions(ctx context.Context, cursor string, limit int) (*identityapi.LiveChannelBindingList, error) {
	if limit == 0 {
		limit = maxLiveBindings
	}
	if limit < 1 || limit > maxLiveBindings {
		return nil, failure(400, "invalid_request")
	}
	rc, err := d.channelService(ctx)
	if err != nil {
		return nil, err
	}
	after := ""
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		var c liveCursor
		if err != nil || json.Unmarshal(raw, &c) != nil || identityapi.ChannelBindingID(c.Binding).Validate() != nil || c.Filter != liveFilter(rc.Tenant) {
			return nil, failure(400, "invalid_cursor")
		}
		after = c.Binding
	}
	out := &identityapi.LiveChannelBindingList{Bindings: []identityapi.LiveChannelBindingListBindingsItem{}}
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT binding_id, max(binding_version), min(subject) FROM identity_channel_sessions
 WHERE tenant_id=$1 AND revoked_at IS NULL AND expires_at > $2 AND binding_id > $3 GROUP BY binding_id ORDER BY binding_id LIMIT $4`,
			rc.Tenant, d.now(), after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item identityapi.LiveChannelBindingListBindingsItem
			var binding string
			if err := rows.Scan(&binding, &item.BindingVersion, &item.Subject); err != nil {
				return err
			}
			item.BindingID = identityapi.ChannelBindingID(binding)
			out.Bindings = append(out.Bindings, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if len(out.Bindings) > limit {
		out.Bindings, out.Truncated = out.Bindings[:limit], true
		raw, _ := json.Marshal(liveCursor{Binding: string(out.Bindings[limit-1].BindingID), Filter: liveFilter(rc.Tenant)})
		out.NextCursor = identityapi.NewOptChannelSessionCursor(identityapi.ChannelSessionCursor(base64.RawURLEncoding.EncodeToString(raw)))
	}
	return out, nil
}
