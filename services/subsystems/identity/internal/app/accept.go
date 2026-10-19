package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// link 是接受请求的链接解析结果。
type link struct {
	invitation, tenant, cell string
	generation               int64
}

// resolveLink 按令牌摘要找到邀请与链接代次；不存在的令牌返回 404 invitation_not_found。
func (d *Directory) resolveLink(ctx context.Context, token string) (*link, error) {
	var l link
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT l.invitation_id, l.generation, i.tenant_id, t.cell FROM identity_invitation_links l
 JOIN identity_invitations i ON i.id=l.invitation_id JOIN identity_tenants t ON t.id=i.tenant_id WHERE l.digest=$1`, linkDigest(token)).
			Scan(&l.invitation, &l.generation, &l.tenant, &l.cell)
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(404, "invitation_not_found")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// AcceptInvitation 是受邀人接受邀请（PRD 第1段 F2 第4–6步）。独立授权：调用者只需是已认证的用户主体，
// 授权来自邀请链接本身与 IdP 中账号的已验证邮箱，不经成员管理接口。
//
// 顺序：解析链接 → 同一账号以同一链接重复接受时直接返回首次结果（不需要 IdP）→ 读取 IdP 账号（不可用时 503）
// → 到期的邀请先置为已过期 → 在一个事务中锁定租户行与邀请行，核验状态、链接代次、过期、邮箱、强认证（含租户管理员角色时）、
// 账号不属于其他租户也不是本租户的有效成员，按邀请限定的角色与单元角色经 writeMember 写入成员资格，把邀请置为已接受，
// 登记待写入 IdP 的租户归属，发布 identity.member.changed.v1 与 identity.invitation.changed.v1 各一次
// → 提交后写入 IdP 的租户与 cell 归属（失败时 access 为 provisioning，重试见 RetryTenantBindings）。
func (d *Directory) AcceptInvitation(ctx context.Context, req *identityapi.AcceptInvitationRequest) (*identityapi.InvitationAcceptance, error) {
	if req == nil || req.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Validate() != nil || rc.Principal.Kind != "user" {
		return nil, failure(403, "invalid_tenant_context")
	}
	subject := rc.Principal.ID
	l, err := d.resolveLink(ctx, req.Token)
	if err != nil {
		return nil, err
	}
	// 写入以邀请所在租户为上下文（事件与审计归属该租户），主体仍是受邀人本人。
	tctx := reqctx.With(ctx, reqctx.Context{Tenant: l.tenant, Cell: l.cell, Principal: rc.Principal, Purpose: rc.Purpose})
	if out, err := d.replayAccept(tctx, l, subject); out != nil || err != nil {
		return out, err
	}
	account, err := d.idp().Account(ctx, subject)
	if errors.Is(err, api.ErrAccountNotFound) {
		return nil, failure(403, "email_unverified")
	}
	if err != nil {
		return nil, failure(503, "identity_unavailable")
	}
	if err := d.expireBefore(tctx, l.tenant, l.invitation, ""); err != nil {
		return nil, err
	}
	out := &identityapi.InvitationAcceptance{}
	replayed := false
	err = d.tx(tctx, func(ctx context.Context, tx pgx.Tx) error {
		t, err := tenant(ctx, tx, l.tenant, true)
		if err != nil {
			return err
		}
		if t.State != "active" {
			return failure(409, "tenant_inactive")
		}
		inv, err := readInvitation(ctx, tx, l.tenant, l.invitation, true)
		if err != nil {
			return err
		}
		switch {
		case inv.state == "accepted":
			// 并发的重复接受：后到的一方在行锁后看到首次结果
			if inv.acceptedBy != nil && *inv.acceptedBy == subject && l.generation == inv.generation {
				replayed = true
				return json.Unmarshal(inv.acceptResult, out)
			}
			return failure(409, "invitation_already_accepted")
		case inv.state == "revoked":
			return failure(410, "invitation_revoked")
		case l.generation != inv.generation:
			return failure(410, "invitation_link_superseded")
		case inv.state == "expired" || inv.due(d.now()):
			return failure(410, "invitation_expired")
		}
		if !account.EmailVerified {
			return failure(403, "email_unverified")
		}
		if strings.ToLower(account.Email) != inv.email {
			return failure(403, "invitation_email_mismatch")
		}
		if slices.Contains(inv.roles, "admin") {
			// 受邀管理员须已登记 TOTP 并刚完成强认证，两者是独立条件：令牌里新鲜的 amr=mfa 只证明当时用过第二因素，
			// 不能覆盖 IdP 本次明确的“未登记”（例如认证后删除了 TOTP）。account 已在事务前读取，IdP 不可用时已 503 失败关闭。
			if !account.TOTPEnrolled {
				return failure(403, "totp_enrollment_required")
			}
			if err := d.strongAuth(ctx, subject, account); err != nil {
				return err
			}
		}
		var state string
		var version int64
		err = tx.QueryRow(ctx, `SELECT state, version FROM identity_members WHERE tenant_id=$1 AND subject=$2`, l.tenant, subject).Scan(&state, &version)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if state == "active" {
			return failure(409, "already_member")
		}
		member := new(api.Session)
		if _, err := d.writeMember(ctx, tx, t, subject, "active", inv.roles, inv.units, version, member); err != nil {
			return err
		}
		now := d.businessNow()
		*out = identityapi.InvitationAcceptance{InvitationID: identityapi.InvitationID(inv.id), Tenant: inv.tenant, Subject: subject,
			Roles: invitationRoles(inv.roles), UnitRoles: toUnitRoles(inv.units), MemberVersion: member.MemberVersion, AcceptedAt: now.UTC(),
			Outcome: identityapi.InvitationAcceptanceOutcomeAccepted, Access: identityapi.InvitationAcceptanceAccessProvisioning}
		stored, err := json.Marshal(out)
		if err != nil {
			return err
		}
		inv.state, inv.version = "accepted", inv.version+1
		if _, err := tx.Exec(ctx, `UPDATE identity_invitations SET state='accepted', version=$2, accepted_by=$3, accepted_at=$4, accept_result=$5, updated_at=now() WHERE id=$1`,
			inv.id, inv.version, subject, now, stored); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identity_tenant_bindings(invitation_id, tenant_id, subject, cell, state) VALUES($1,$2,$3,$4,'pending')`,
			inv.id, inv.tenant, subject, string(t.Cell)); err != nil {
			return err
		}
		return emitInvitation(ctx, tx, inv, "accepted", now, subject, "invitation_link")
	})
	if err != nil {
		return nil, err
	}
	if replayed {
		out.Outcome = identityapi.InvitationAcceptanceOutcomeAlreadyAccepted
	}
	out.Access = d.bindTenant(tctx, l.invitation)
	return out, nil
}

// replayAccept 处理同一账号以同一链接的重复接受（例如首次成功但响应丢失）：返回首次结果，outcome 为 already_accepted，
// 不写入、不发布；仍未写入 IdP 租户归属时顺带重试。其他情况返回 nil, nil，由正常流程处理。
func (d *Directory) replayAccept(ctx context.Context, l *link, subject string) (*identityapi.InvitationAcceptance, error) {
	var stored []byte
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT accept_result FROM identity_invitations WHERE id=$1 AND state='accepted' AND accepted_by=$2 AND link_generation=$3`,
			l.invitation, subject, l.generation).Scan(&stored)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil || stored == nil {
		return nil, err
	}
	out := &identityapi.InvitationAcceptance{}
	if err := json.Unmarshal(stored, out); err != nil {
		return nil, err
	}
	out.Outcome = identityapi.InvitationAcceptanceOutcomeAlreadyAccepted
	out.Access = d.bindTenant(ctx, l.invitation)
	return out, nil
}

// bindTenant 把已接受邀请的租户与 cell 归属写入 IdP，返回 ready（已写入）或 provisioning（未写入，可重试）。
//
// 在一个事务中锁定归属行并持有账号咨询锁（与 oneTenantPerAccount 相同）直到写入 IdP 并记录结果：
//   - 写入前核对接受人仍是该租户的有效成员，否则取消，不写入已退出成员的归属；
//   - 账号加入其他租户须先取得同一把锁，因此旧租户的迟到写入不会在新租户的归属之后落到 IdP，
//     最后写入 IdP 的总是账号当前所在的租户；
//   - 只有 pending 的行会写入，done 直接返回 ready，并发重试串行。
//
// 成员资格的权威始终是 identity 目录：核对之后成员退出的，网关的目录核验仍拒绝其请求。
func (d *Directory) bindTenant(ctx context.Context, invitationID string) identityapi.InvitationAcceptanceAccess {
	access := identityapi.InvitationAcceptanceAccessProvisioning
	_ = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var subject, tenantID, cell, state string
		if err := tx.QueryRow(ctx, `SELECT subject, tenant_id, cell, state FROM identity_tenant_bindings WHERE invitation_id=$1 FOR UPDATE`, invitationID).
			Scan(&subject, &tenantID, &cell, &state); err != nil {
			return err
		}
		if state == "done" {
			access = identityapi.InvitationAcceptanceAccessReady
			return nil
		}
		if state != "pending" {
			return nil
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "identity-account/"+subject); err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM identity_members WHERE tenant_id=$1 AND subject=$2 AND state='active')`, tenantID, subject).Scan(&active); err != nil {
			return err
		}
		if !active {
			_, err := tx.Exec(ctx, `UPDATE identity_tenant_bindings SET state='cancelled', updated_at=now() WHERE invitation_id=$1`, invitationID)
			return err
		}
		bctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		bindErr := d.idp().BindTenant(bctx, subject, tenantID, cell)
		cancel()
		next := "done"
		if bindErr != nil {
			next = "pending"
		}
		if _, err := tx.Exec(ctx, `UPDATE identity_tenant_bindings SET state=$2, attempts=attempts+1, updated_at=now() WHERE invitation_id=$1`, invitationID, next); err != nil {
			return err
		}
		if bindErr == nil {
			access = identityapi.InvitationAcceptanceAccessReady
		}
		return nil
	})
	return access
}

// RetryTenantBindings 重试尚未写入 IdP 的租户归属（供定时任务调用），返回本次写入成功的数量。
func (d *Directory) RetryTenantBindings(ctx context.Context, limit int) (int, error) {
	var pending []string
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT invitation_id FROM identity_tenant_bindings WHERE state='pending' ORDER BY updated_at LIMIT $1`, limit)
		if err != nil {
			return err
		}
		pending, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range pending {
		if d.bindTenant(ctx, id) == identityapi.InvitationAcceptanceAccessReady {
			n++
		}
	}
	return n, nil
}
