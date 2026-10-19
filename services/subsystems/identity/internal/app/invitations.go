package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// 邀请（PRD 第1段 F2；identity.md 第5节与“邀请状态机”）。
//
//	待接受 ──接受──► 已接受
//	   ├──撤回──► 已撤回
//	   ├──重发──► 待接受（新链接，旧链接失效）
//	   └──过期──► 已过期 ──重发──► 待接受
//
// 状态变化都在邀请行锁下执行并使版本加一，每次发布一条 identity.invitation.changed.v1。过期按业务时钟（Directory.Now）
// 判断：到期的待接受邀请在被读取或操作前先在独立事务中置为已过期（列表、创建、重发、撤回、接受前，以及
// ExpireDueInvitations）；命令事务中再次发现到期时按已过期处理，不会接受或撤回到期的邀请。
const (
	// InvitationTTL 是邀请链接的有效期（自创建或最近一次重发）。
	InvitationTTL = 7 * 24 * time.Hour
	// MaxPendingInvitations 是每个租户未过期的待接受邀请上限，在写入前核对。
	MaxPendingInvitations = 1000
	maxListedInvitations  = 500
)

type invitation struct {
	tenant, id, email, name string
	roles                   []string
	units                   map[string][]string
	state                   string
	version, generation     int64
	inviter                 string
	expiresAt               time.Time
	delivery                string
	acceptedBy              *string
	acceptedAt              *time.Time
	acceptResult            []byte
	createdAt               time.Time
}

const invitationColumns = `id,tenant_id,email,name,roles,state,version,inviter,link_generation,expires_at,delivery,accepted_by,accepted_at,accept_result,created_at`

func scanInvitation(row pgx.Row) (*invitation, error) {
	var i invitation
	err := row.Scan(&i.id, &i.tenant, &i.email, &i.name, &i.roles, &i.state, &i.version, &i.inviter, &i.generation, &i.expiresAt, &i.delivery, &i.acceptedBy, &i.acceptedAt, &i.acceptResult, &i.createdAt)
	if err != nil {
		return nil, err
	}
	slices.Sort(i.roles)
	return &i, nil
}

// readInvitation 读取本租户的邀请（含单元角色）。其他租户的与不存在的邀请返回同一 404 invitation_not_found。
func readInvitation(ctx context.Context, tx pgx.Tx, tenantID, id string, lock bool) (*invitation, error) {
	q := `SELECT ` + invitationColumns + ` FROM identity_invitations WHERE tenant_id=$1 AND id=$2`
	if lock {
		q += " FOR UPDATE"
	}
	inv, err := scanInvitation(tx.QueryRow(ctx, q, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, failure(404, "invitation_not_found")
	}
	if err != nil {
		return nil, err
	}
	return inv, inv.loadUnits(ctx, tx)
}

func (i *invitation) loadUnits(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT org_unit_id, roles FROM identity_invitation_unit_roles WHERE invitation_id=$1`, i.id)
	if err != nil {
		return err
	}
	defer rows.Close()
	i.units = map[string][]string{}
	for rows.Next() {
		var unit string
		var roles []string
		if err := rows.Scan(&unit, &roles); err != nil {
			return err
		}
		slices.Sort(roles)
		i.units[unit] = roles
	}
	return rows.Err()
}

// due 判断待接受的邀请在 now 时是否已到过期时间。
func (i *invitation) due(now time.Time) bool { return i.state == "pending" && !now.Before(i.expiresAt) }

func invitationRoles(roles []string) identityapi.InvitationRoles {
	out := make(identityapi.InvitationRoles, 0, len(roles))
	for _, r := range roles {
		out = append(out, identityapi.InvitationRolesItem(r))
	}
	return out
}

func (i *invitation) out() *identityapi.Invitation {
	o := &identityapi.Invitation{ID: identityapi.InvitationID(i.id), Email: i.email, Name: i.name, Roles: invitationRoles(i.roles),
		UnitRoles: toUnitRoles(i.units), State: identityapi.InvitationState(i.state), Version: i.version, Inviter: i.inviter,
		ExpiresAt: i.expiresAt.UTC(), Delivery: identityapi.InvitationDelivery(i.delivery), CreatedAt: i.createdAt.UTC()}
	if i.acceptedBy != nil {
		o.AcceptedBy = identityapi.NewOptString(*i.acceptedBy)
	}
	if i.acceptedAt != nil {
		o.AcceptedAt = identityapi.NewOptDateTime(i.acceptedAt.UTC())
	}
	return o
}

// emitInvitation 在事务中发布 identity.invitation.changed.v1 并写审计。载荷不含邮箱与链接令牌。
// effective 是变化在业务上生效的时间：过期为过期时间，其余为命令执行时间。
func emitInvitation(ctx context.Context, tx pgx.Tx, i *invitation, change string, effective time.Time, subject, basis string) error {
	data := map[string]any{"tenant_id": i.tenant, "invitation_id": i.id, "change": change, "state": i.state, "version": i.version,
		"link_generation": i.generation, "expires_at": i.expiresAt.UTC().Format(time.RFC3339Nano),
		"effective_at": effective.UTC().Format(time.RFC3339Nano), "roles": i.roles}
	if len(i.units) > 0 {
		data["unit_roles"] = toUnitRoles(i.units)
	}
	if subject != "" {
		data["subject"] = subject
	}
	w := outbox.Writer{Subsystem: api.Name, Namespace: "identity", Zone: reqctx.ZonePlatform}
	if _, err := w.Append(ctx, tx, outbox.Event{Type: "identity.invitation.changed.v1", Subject: i.id, PartitionKey: i.tenant, Data: data}); err != nil {
		return err
	}
	return (audit.Recorder{Subsystem: api.Name}).Record(ctx, tx, audit.Entry{Action: "identity.invitation." + change, Resource: i.id, Result: "succeeded", Basis: basis})
}

// expireLocked 把到期的待接受邀请置为已过期（调用方持有邀请行锁）。
func expireLocked(ctx context.Context, tx pgx.Tx, i *invitation) error {
	i.state, i.version = "expired", i.version+1
	if _, err := tx.Exec(ctx, `UPDATE identity_invitations SET state='expired', version=$2, updated_at=now() WHERE id=$1`, i.id, i.version); err != nil {
		return err
	}
	return emitInvitation(ctx, tx, i, "expired", i.expiresAt, "", "invitation_expiry")
}

// expireDue 在事务中把本租户（可限定邀请或邮箱）到期的待接受邀请置为已过期，返回处理数量。
func (d *Directory) expireDue(ctx context.Context, tx pgx.Tx, tenantID, id, email string) (int, error) {
	q := `SELECT ` + invitationColumns + ` FROM identity_invitations WHERE tenant_id=$1 AND state='pending' AND expires_at <= $2`
	args := []any{tenantID, d.now()}
	if id != "" {
		q += ` AND id=$3`
		args = append(args, id)
	} else if email != "" {
		q += ` AND email=$3`
		args = append(args, email)
	}
	rows, err := tx.Query(ctx, q+` ORDER BY expires_at LIMIT 500 FOR UPDATE`, args...)
	if err != nil {
		return 0, err
	}
	var due []*invitation
	for rows.Next() {
		inv, err := scanInvitation(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, inv)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, inv := range due {
		if err := inv.loadUnits(ctx, tx); err != nil {
			return 0, err
		}
		if err := expireLocked(ctx, tx, inv); err != nil {
			return 0, err
		}
	}
	return len(due), nil
}

// expireBefore 在独立事务中先处理到期的邀请，使随后的命令按已过期状态判断并返回原因（命令失败回滚时过期仍保留）。
func (d *Directory) expireBefore(ctx context.Context, tenantID, id, email string) error {
	return d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := d.expireDue(ctx, tx, tenantID, id, email)
		return err
	})
}

// ExpireDueInvitations 把全部租户中到期的待接受邀请置为已过期（供定时任务调用，以服务主体执行），返回处理数量。
func (d *Directory) ExpireDueInvitations(ctx context.Context, limit int) (int, error) {
	type item struct{ tenant, cell string }
	var tenants []item
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT i.tenant_id, t.cell FROM identity_invitations i JOIN identity_tenants t ON t.id=i.tenant_id
 WHERE i.state='pending' AND i.expires_at <= $1 LIMIT $2`, d.now(), limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.tenant, &it.cell); err != nil {
				rows.Close()
				return err
			}
			tenants = append(tenants, it)
		}
		rows.Close()
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	total := 0
	for _, it := range tenants {
		sctx := reqctx.With(ctx, reqctx.Context{Tenant: it.tenant, Cell: it.cell, Principal: reqctx.Principal{ID: "svc:" + api.Name, Kind: "service"}})
		err := d.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
			n, err := d.expireDue(ctx, tx, it.tenant, "", "")
			total += n
			return err
		})
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// newLinkToken 生成邀请链接令牌（32 字节随机数的 base64url，43 位）与其摘要。
func newLinkToken() (token, digest string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, linkDigest(token), nil
}

func linkDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func uniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// lockInvitations 使本租户的邀请创建与重发串行（同一邮箱一个待接受邀请、待接受上限）。
func lockInvitations(ctx context.Context, tx pgx.Tx, tenantID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "identity-invitations/"+tenantID)
	return err
}

// pendingCapacity 在写入前核对本租户未过期的待接受邀请未达上限。
func (d *Directory) pendingCapacity(ctx context.Context, tx pgx.Tx, tenantID string) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity_invitations WHERE tenant_id=$1 AND state='pending' AND expires_at > $2`, tenantID, d.now()).Scan(&n); err != nil {
		return err
	}
	if n >= MaxPendingInvitations {
		return failure(409, "invitation_limit_reached")
	}
	return nil
}

// issueLink 为邀请写入新链接（代次 i.generation）并返回令牌原文。
func issueLink(ctx context.Context, tx pgx.Tx, i *invitation) (string, error) {
	token, digest, err := newLinkToken()
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO identity_invitation_links(digest, invitation_id, generation) VALUES($1,$2,$3)`, digest, i.id, i.generation)
	return token, err
}

// CreateInvitation 创建邀请（can_manage_members + 15分钟内的强认证）。
func (d *Directory) CreateInvitation(ctx context.Context, req *identityapi.CreateInvitationRequest, key string) (*identityapi.InvitationIssued, error) {
	if req.Validate() != nil || strings.TrimSpace(req.Name) != req.Name {
		return nil, failure(400, "invalid_request")
	}
	units, err := unitRoleMap(req.UnitRoles)
	if err != nil {
		return nil, err
	}
	if len(req.Roles) == 0 && len(units) == 0 {
		return nil, failure(400, "invalid_request")
	}
	roles := make([]string, 0, len(req.Roles))
	for _, r := range req.Roles {
		roles = append(roles, string(r))
	}
	slices.Sort(roles)
	email := strings.ToLower(req.Email)
	rc, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Tenant == "" {
		return nil, failure(403, "invalid_tenant_context")
	}
	out := &identityapi.InvitationIssued{}
	var issued *invitation
	var token, tenantName string
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := d.admin(ctx, tx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := d.expireBefore(ctx, rc.Tenant, "", email); err != nil {
		return nil, err
	}
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := d.admin(ctx, tx); err != nil {
			return err
		}
		t, err := tenant(ctx, tx, rc.Tenant, false)
		if err != nil {
			return err
		}
		tenantName = t.Name
		return command(ctx, tx, key, "invitation.create", req, out, func() error {
			if err := lockInvitations(ctx, tx, rc.Tenant); err != nil {
				return err
			}
			var existingID string
			err := tx.QueryRow(ctx, `SELECT id FROM identity_invitations WHERE tenant_id=$1 AND email=$2 AND state='pending'`, rc.Tenant, email).Scan(&existingID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err == nil {
				existing, err := readInvitation(ctx, tx, rc.Tenant, existingID, true)
				if err != nil {
					return err
				}
				if !existing.due(d.now()) {
					*out = identityapi.InvitationIssued{Invitation: *existing.out(), Outcome: identityapi.InvitationIssuedOutcomeExisting}
					return nil
				}
				if err := expireLocked(ctx, tx, existing); err != nil {
					return err
				}
			}
			for unit := range units {
				chain, err := unitChain(ctx, tx, rc.Tenant, unit)
				if err != nil {
					return err
				}
				if chain == nil {
					return failure(422, "org_unit_not_found")
				}
			}
			if err := d.pendingCapacity(ctx, tx, rc.Tenant); err != nil {
				return err
			}
			now := d.businessNow()
			inv := &invitation{tenant: rc.Tenant, id: ids.New("inv"), email: email, name: req.Name, roles: roles, units: units, state: "pending",
				version: 1, generation: 1, inviter: rc.Principal.ID, expiresAt: now.Add(InvitationTTL), delivery: "pending"}
			if err := tx.QueryRow(ctx, `INSERT INTO identity_invitations(id,tenant_id,email,name,roles,state,version,inviter,link_generation,expires_at,delivery)
 VALUES($1,$2,$3,$4,$5,'pending',1,$6,1,$7,'pending') RETURNING created_at`, inv.id, inv.tenant, inv.email, inv.name, inv.roles, inv.inviter, inv.expiresAt).Scan(&inv.createdAt); err != nil {
				if uniqueViolation(err) {
					return failure(409, "invitation_duplicate")
				}
				return err
			}
			for unit, bound := range units {
				if _, err := tx.Exec(ctx, `INSERT INTO identity_invitation_unit_roles(tenant_id,invitation_id,org_unit_id,roles) VALUES($1,$2,$3,$4)`, inv.tenant, inv.id, unit, bound); err != nil {
					return err
				}
			}
			if token, err = issueLink(ctx, tx, inv); err != nil {
				return err
			}
			if err := emitInvitation(ctx, tx, inv, "created", now, "", "can_manage_members"); err != nil {
				return err
			}
			*out = identityapi.InvitationIssued{Invitation: *inv.out(), Outcome: identityapi.InvitationIssuedOutcomeCreated}
			issued = inv
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if issued != nil {
		out.Invitation.Delivery = d.deliver(ctx, issued, token, tenantName)
		out.LinkToken = identityapi.NewOptString(token)
	}
	return out, nil
}

// deliver 在提交后发送邀请邮件，并按链接代次记录投递结果：只更新仍是同一代次的待接受邀请，重发后旧代次的结果不会覆盖新链接。
// 结果未能记录时返回 pending（未知），不声称已发送或失败。
func (d *Directory) deliver(ctx context.Context, i *invitation, token, tenantName string) identityapi.InvitationDelivery {
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	state := "sent"
	if err := d.notifier().SendInvitation(sctx, api.InvitationMessage{InvitationID: i.id, Tenant: i.tenant, TenantName: tenantName, Email: i.email,
		Name: i.name, Token: token, Generation: i.generation, ExpiresAt: i.expiresAt}); err != nil {
		state = "failed"
	}
	recorded := false
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE identity_invitations SET delivery=$3, updated_at=now() WHERE id=$1 AND link_generation=$2 AND state='pending'`, i.id, i.generation, state)
		recorded = err == nil && tag.RowsAffected() == 1
		return err
	})
	if err != nil || !recorded {
		return identityapi.InvitationDeliveryPending
	}
	return identityapi.InvitationDelivery(state)
}

// ResendInvitation 重发邀请：待接受或已过期的邀请生成新链接与新的有效期，旧链接失效（can_manage_members + 强认证）。
func (d *Directory) ResendInvitation(ctx context.Context, id string, req *identityapi.InvitationVersionRequest, key string) (*identityapi.InvitationIssued, error) {
	if req.Validate() != nil || identityapi.InvitationID(id).Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Tenant == "" {
		return nil, failure(403, "invalid_tenant_context")
	}
	if err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := d.admin(ctx, tx); return err }); err != nil {
		return nil, err
	}
	if err := d.expireBefore(ctx, rc.Tenant, id, ""); err != nil {
		return nil, err
	}
	out := &identityapi.InvitationIssued{}
	var issued *invitation
	var token, tenantName string
	request := struct {
		ID  string                                `json:"id"`
		Req *identityapi.InvitationVersionRequest `json:"request"`
	}{id, req}
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := d.admin(ctx, tx); err != nil {
			return err
		}
		t, err := tenant(ctx, tx, rc.Tenant, false)
		if err != nil {
			return err
		}
		tenantName = t.Name
		return command(ctx, tx, key, "invitation.resend", request, out, func() error {
			if err := lockInvitations(ctx, tx, rc.Tenant); err != nil {
				return err
			}
			inv, err := readInvitation(ctx, tx, rc.Tenant, id, true)
			if err != nil {
				return err
			}
			if inv.state != "pending" && inv.state != "expired" {
				return failure(409, "invitation_not_pending")
			}
			if inv.version != req.ExpectedVersion {
				return failure(409, "version_conflict")
			}
			now := d.businessNow()
			if inv.due(d.now()) {
				if err := expireLocked(ctx, tx, inv); err != nil {
					return err
				}
			}
			if inv.state == "expired" {
				// 已过期的邀请重新进入待接受：同一邮箱不能已有另一个待接受邀请，且不超过待接受上限
				if _, err := d.expireDue(ctx, tx, rc.Tenant, "", inv.email); err != nil {
					return err
				}
				var other bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM identity_invitations WHERE tenant_id=$1 AND email=$2 AND state='pending' AND id<>$3)`, rc.Tenant, inv.email, inv.id).Scan(&other); err != nil {
					return err
				}
				if other {
					return failure(409, "invitation_duplicate")
				}
				if err := d.pendingCapacity(ctx, tx, rc.Tenant); err != nil {
					return err
				}
			}
			inv.state, inv.version, inv.generation = "pending", inv.version+1, inv.generation+1
			inv.inviter, inv.expiresAt, inv.delivery = rc.Principal.ID, now.Add(InvitationTTL), "pending"
			if _, err := tx.Exec(ctx, `UPDATE identity_invitations SET state='pending', version=$2, link_generation=$3, inviter=$4, expires_at=$5, delivery='pending', updated_at=now() WHERE id=$1`,
				inv.id, inv.version, inv.generation, inv.inviter, inv.expiresAt); err != nil {
				if uniqueViolation(err) {
					return failure(409, "invitation_duplicate")
				}
				return err
			}
			if token, err = issueLink(ctx, tx, inv); err != nil {
				return err
			}
			if err := emitInvitation(ctx, tx, inv, "resent", now, "", "can_manage_members"); err != nil {
				return err
			}
			*out = identityapi.InvitationIssued{Invitation: *inv.out(), Outcome: identityapi.InvitationIssuedOutcomeResent}
			issued = inv
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if issued != nil {
		out.Invitation.Delivery = d.deliver(ctx, issued, token, tenantName)
		out.LinkToken = identityapi.NewOptString(token)
	}
	return out, nil
}

// RevokeInvitation 撤回待接受的邀请（can_manage_members + 强认证）。与接受并发时在邀请行锁下只有一个生效。
func (d *Directory) RevokeInvitation(ctx context.Context, id string, req *identityapi.InvitationVersionRequest, key string) (*identityapi.Invitation, error) {
	if req.Validate() != nil || identityapi.InvitationID(id).Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Tenant == "" {
		return nil, failure(403, "invalid_tenant_context")
	}
	if err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := d.admin(ctx, tx); return err }); err != nil {
		return nil, err
	}
	if err := d.expireBefore(ctx, rc.Tenant, id, ""); err != nil {
		return nil, err
	}
	out := &identityapi.Invitation{}
	request := struct {
		ID  string                                `json:"id"`
		Req *identityapi.InvitationVersionRequest `json:"request"`
	}{id, req}
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := d.admin(ctx, tx); err != nil {
			return err
		}
		return command(ctx, tx, key, "invitation.revoke", request, out, func() error {
			inv, err := readInvitation(ctx, tx, rc.Tenant, id, true)
			if err != nil {
				return err
			}
			if inv.state != "pending" || inv.due(d.now()) {
				return failure(409, "invitation_not_pending")
			}
			if inv.version != req.ExpectedVersion {
				return failure(409, "version_conflict")
			}
			inv.state, inv.version = "revoked", inv.version+1
			if _, err := tx.Exec(ctx, `UPDATE identity_invitations SET state='revoked', version=$2, updated_at=now() WHERE id=$1`, inv.id, inv.version); err != nil {
				return err
			}
			if err := emitInvitation(ctx, tx, inv, "revoked", d.businessNow(), "", "can_manage_members"); err != nil {
				return err
			}
			*out = *inv.out()
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListInvitations 列出本租户邀请（can_manage_members + 强认证），先把到期的待接受邀请置为已过期。
func (d *Directory) ListInvitations(ctx context.Context, state identityapi.OptInvitationState) (*identityapi.InvitationList, error) {
	rc, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Tenant == "" {
		return nil, failure(403, "invalid_tenant_context")
	}
	out := &identityapi.InvitationList{Invitations: []identityapi.Invitation{}}
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := d.admin(ctx, tx); err != nil {
			return err
		}
		if _, err := d.expireDue(ctx, tx, rc.Tenant, "", ""); err != nil {
			return err
		}
		q := `SELECT ` + invitationColumns + ` FROM identity_invitations WHERE tenant_id=$1`
		args := []any{rc.Tenant}
		if s, ok := state.Get(); ok {
			q += ` AND state=$2`
			args = append(args, string(s))
		}
		rows, err := tx.Query(ctx, q+` ORDER BY created_at DESC, id DESC LIMIT `+strconv.Itoa(maxListedInvitations+1), args...)
		if err != nil {
			return err
		}
		var list []*invitation
		for rows.Next() {
			inv, err := scanInvitation(rows)
			if err != nil {
				rows.Close()
				return err
			}
			list = append(list, inv)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(list) > maxListedInvitations {
			list, out.Truncated = list[:maxListedInvitations], true
		}
		for _, inv := range list {
			if err := inv.loadUnits(ctx, tx); err != nil {
				return err
			}
			out.Invitations = append(out.Invitations, *inv.out())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
