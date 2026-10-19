package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// Directory 是 identity 目录：租户、成员、组织单元、角色绑定与业务资源登记以 SQL 为准，授权模型在 OpenFGA 中求值
// （FGA；Authorizer 只用于平台对象上的持久化关系，例如 operator）。
//
// IdP 与 Notifier 是出站端口（api.IdentityProvider、api.Notifier），缺省为未接入（api.NotWired）；Now 是业务时钟
// （强认证时效与邀请过期按它判断），缺省为系统时钟。记录时间（created_at 等）取数据库时间。
type Directory struct {
	DB         *db.DB
	Authorizer authz.Decider
	FGA        Store
	IdP        api.IdentityProvider
	Notifier   api.Notifier
	Now        func() time.Time
}

func failure(status int, code string) error { return &api.Error{Status: status, Code: code} }

func (d *Directory) tx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	if d == nil || d.DB == nil {
		return failure(503, "identity_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := d.DB.Tx(ctx, func(tx pgx.Tx) error { return fn(ctx, tx) })
	var safe *api.Error
	if err != nil && !errors.As(err, &safe) {
		return failure(503, "identity_unavailable")
	}
	return err
}
func actor(ctx context.Context) (reqctx.Context, error) {
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Validate() != nil || rc.Principal.Kind != "user" {
		return rc, failure(403, "invalid_tenant_context")
	}
	return rc, nil
}
func (d *Directory) platformAdmin(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rc, err := actor(ctx)
	if err != nil {
		return err
	}
	if rc.Tenant != "" {
		return failure(403, "forbidden")
	}
	if !reqctx.MFA(ctx) {
		return failure(403, "mfa_required")
	}
	if d == nil || d.Authorizer == nil {
		return failure(503, "identity_unavailable")
	}
	allowed, err := d.Authorizer.Check(ctx, authz.Request{Subject: authz.Subject(rc.Principal), Relation: "operator", Object: "platform:miaoqia"})
	if err != nil {
		return failure(503, "identity_unavailable")
	}
	if !allowed {
		return failure(403, "forbidden")
	}
	return nil
}
func tenant(ctx context.Context, tx pgx.Tx, id string, lock bool) (*identityapi.Tenant, error) {
	q := `SELECT id,name,cell,state,version FROM identity_tenants WHERE id=$1`
	if lock {
		q += " FOR UPDATE"
	}
	var value identityapi.Tenant
	var cell, state string
	err := tx.QueryRow(ctx, q, id).Scan(&value.ID, &value.Name, &cell, &state, &value.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, failure(403, "invalid_tenant_context")
	}
	if err != nil {
		return nil, err
	}
	value.Cell = identityapi.TenantCell(cell)
	value.State = identityapi.TenantState(state)
	return &value, nil
}
func session(ctx context.Context, tx pgx.Tx, tenantID, subject, cell string) (*api.Session, error) {
	return readMember(ctx, tx, tenantID, subject, cell, true)
}
func readMember(ctx context.Context, tx pgx.Tx, tenantID, subject, cell string, activeOnly bool) (*api.Session, error) {
	var s api.Session
	var route, state, tenantState string
	var roles []string
	err := tx.QueryRow(ctx, `SELECT t.id,t.cell,t.state,t.version,m.subject,m.state,m.roles,m.version
 FROM identity_tenants t JOIN identity_members m ON m.tenant_id=t.id WHERE t.id=$1 AND m.subject=$2`, tenantID, subject).Scan(&s.Tenant, &route, &tenantState, &s.TenantVersion, &s.Subject, &state, &roles, &s.MemberVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		if !activeOnly {
			return nil, failure(404, "member_not_found")
		}
		return nil, failure(403, "invalid_tenant_context")
	}
	if err != nil {
		return nil, err
	}
	if route != "cell-01" || cell != route || tenantState != "active" || (activeOnly && state != "active") {
		return nil, failure(403, "invalid_tenant_context")
	}
	s.Cell = identityapi.SessionCell(route)
	s.State = identityapi.SessionState(state)
	s.Roles = make([]identityapi.SessionRolesItem, 0, len(roles))
	for _, r := range roles {
		s.Roles = append(s.Roles, identityapi.SessionRolesItem(r))
	}
	units, err := unitRoles(ctx, tx, tenantID, subject)
	if err != nil {
		return nil, err
	}
	s.UnitRoles = toUnitRoles(units)
	return &s, nil
}
func (d *Directory) Resolve(ctx context.Context) (*api.Session, error) {
	rc, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Tenant == "" {
		return nil, failure(403, "invalid_tenant_context")
	}
	var s *api.Session
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		s, err = session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell)
		return err
	})
	return s, err
}

// Allow 判断成员对其租户（tenant 资源）的关系，按当前目录角色求值。
func (d *Directory) Allow(ctx context.Context, s *api.Session, relation string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	allowed, err := d.evaluate(ctx, s, relation, &target{})
	if err != nil {
		return false, failure(503, "identity_unavailable")
	}
	return allowed, nil
}
func emit(ctx context.Context, tx pgx.Tx, event, id string, data any) error {
	w := outbox.Writer{Subsystem: api.Name, Namespace: "identity", Zone: reqctx.ZonePlatform}
	if _, err := w.Append(ctx, tx, outbox.Event{Type: event, Subject: id, PartitionKey: id, Data: data}); err != nil {
		return err
	}
	return (audit.Recorder{Subsystem: api.Name}).Record(ctx, tx, audit.Entry{Action: event, Resource: id, Result: "succeeded", Basis: "identity.directory"})
}
func tenantEvent(t *identityapi.Tenant) map[string]any {
	return map[string]any{"tenant_id": t.ID, "state": string(t.State), "version": t.Version}
}
func memberEvent(s *api.Session) map[string]any {
	event := map[string]any{"tenant_id": s.Tenant, "subject": s.Subject, "state": string(s.State), "version": s.MemberVersion, "roles": s.Roles}
	if len(s.UnitRoles) > 0 {
		event["unit_roles"] = s.UnitRoles
	}
	return event
}

func (d *Directory) Create(ctx context.Context, req *identityapi.CreateTenantRequest, key string) (*identityapi.Tenant, error) {
	if err := req.Validate(); err != nil || strings.TrimSpace(req.Name) != req.Name {
		return nil, failure(400, "invalid_request")
	}
	if err := d.platformAdmin(ctx); err != nil {
		return nil, err
	}
	t := &identityapi.Tenant{ID: ids.New("tnt"), Name: req.Name, Cell: "cell-01", State: "active", Version: 1}
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return command(ctx, tx, key, "tenant.create", req, t, func() error {
			if _, err := tx.Exec(ctx, `INSERT INTO identity_tenants(id,name,cell,state,version) VALUES($1,$2,'cell-01','active',1)`, t.ID, t.Name); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO identity_members(tenant_id,subject,state,roles,version) VALUES($1,$2,'active',ARRAY['admin'],1)`, t.ID, req.AdminSubject); err != nil {
				return err
			}
			if err := emit(ctx, tx, "identity.tenant.created.v1", t.ID, tenantEvent(t)); err != nil {
				return err
			}
			return emit(ctx, tx, "identity.member.changed.v1", t.ID, map[string]any{"tenant_id": t.ID, "subject": req.AdminSubject, "version": 1, "state": "active", "roles": []string{"admin"}})
		})
	})
	return t, err
}
func (d *Directory) SetState(ctx context.Context, id string, req *identityapi.SetTenantStateRequest, key string) (*identityapi.Tenant, error) {
	if req.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	if err := d.platformAdmin(ctx); err != nil {
		return nil, err
	}
	t := new(identityapi.Tenant)
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return command(ctx, tx, key, "tenant.state/"+id, req, t, func() error {
			current, err := tenant(ctx, tx, id, true)
			if err != nil {
				return err
			}
			*t = *current
			if t.Version != req.ExpectedVersion {
				return failure(409, "version_conflict")
			}
			if string(t.State) == string(req.State) {
				return nil
			}
			t.State = identityapi.TenantState(req.State)
			t.Version++
			if _, err = tx.Exec(ctx, `UPDATE identity_tenants SET state=$2,version=$3 WHERE id=$1`, id, string(t.State), t.Version); err != nil {
				return err
			}
			event := "identity.tenant.activated.v1"
			if t.State == "suspended" {
				event = "identity.tenant.suspended.v1"
			}
			return emit(ctx, tx, event, id, tenantEvent(t))
		})
	})
	return t, err
}
func (d *Directory) Put(ctx context.Context, subject string, req *identityapi.PutMemberRequest, key string) (*api.Session, error) {
	if req.Validate() != nil || (req.State == "removed" && (len(req.Roles) != 0 || len(req.UnitRoles) != 0)) {
		return nil, failure(400, "invalid_request")
	}
	wantUnits, err := unitRoleMap(req.UnitRoles)
	if err != nil {
		return nil, err
	}
	rc, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Tenant == "" {
		return nil, failure(403, "invalid_tenant_context")
	}
	roles := make([]string, 0, len(req.Roles))
	for _, r := range req.Roles {
		roles = append(roles, string(r))
	}
	result := new(api.Session)
	removed := false
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		t, err := tenant(ctx, tx, rc.Tenant, true)
		if err != nil {
			return err
		}
		caller, err := session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell)
		if err != nil {
			return err
		}
		allowed, err := d.Allow(ctx, caller, "can_manage_members")
		if err != nil {
			return err
		}
		if !allowed {
			return failure(403, "forbidden")
		}
		// 角色变更是强认证动作（PRD 第1段 F3）：先授权、再核对强认证，均在返回已存回执之前
		if err := d.strongAuth(ctx, rc.Principal.ID, nil); err != nil {
			return err
		}
		return command(ctx, tx, key, "member.put/"+subject, req, result, func() error {
			removed, err = d.writeMember(ctx, tx, t, subject, string(req.State), roles, wantUnits, req.ExpectedVersion, result)
			return err
		})
	})
	if err == nil && removed {
		// 退出后的参与关系已因成员资格求交而失效；提交后尽力清理，失败时由重新加入前的清理兜底。
		_ = d.purgeAfterLeave(ctx, rc.Tenant, subject)
	}
	return result, err
}

// unitRoleMap 把单元角色转换为 单元 → 角色（排序）；同一单元出现两次为请求无效。
func unitRoleMap(in identityapi.UnitRoles) (map[string][]string, error) {
	out := map[string][]string{}
	for _, b := range in {
		if _, dup := out[string(b.OrgUnitID)]; dup {
			return nil, failure(400, "invalid_request")
		}
		roles := make([]string, 0, len(b.Roles))
		for _, r := range b.Roles {
			roles = append(roles, string(r))
		}
		slices.Sort(roles)
		out[string(b.OrgUnitID)] = roles
	}
	return out, nil
}

// writeMember 是成员写入的内部逻辑，成员命令与接受邀请共用：调用方已完成授权并持有租户行锁（tenant(..., true)）。
// 对成员行加排他锁并按 expected 做版本控制；加入或重新加入时先确认账号不是其他租户的有效成员（M2 一个账号只属于一个租户），
// 并清除旧参与关系；保护末位管理员；写入成员资格与单元角色并发布 identity.member.changed.v1。返回本次是否使成员退出。
func (d *Directory) writeMember(ctx context.Context, tx pgx.Tx, t *identityapi.Tenant, subject, state string, roles []string, wantUnits map[string][]string, expected int64, result *api.Session) (bool, error) {
	var version int64
	var oldState string
	var oldRoles []string
	// 成员行加排他锁：与退出后的清理（purgeAfterLeave）及参与关系同步（admit 的共享锁）按成员串行。
	err := tx.QueryRow(ctx, `SELECT state,roles,version FROM identity_members WHERE tenant_id=$1 AND subject=$2 FOR UPDATE`, t.ID, subject).Scan(&oldState, &oldRoles, &version)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if version != expected {
		return false, failure(409, "version_conflict")
	}
	roles = slices.Clone(roles)
	slices.Sort(roles)
	slices.Sort(oldRoles)
	oldUnits, err := unitRoles(ctx, tx, t.ID, subject)
	if err != nil {
		return false, err
	}
	*result = api.Session{Tenant: t.ID, Cell: "cell-01", Subject: subject, State: identityapi.SessionState(state), Roles: make([]identityapi.SessionRolesItem, 0, len(roles)), TenantVersion: t.Version, MemberVersion: version, UnitRoles: toUnitRoles(wantUnits)}
	for _, r := range roles {
		result.Roles = append(result.Roles, identityapi.SessionRolesItem(r))
	}
	if state == oldState && slices.Equal(oldRoles, roles) && sameUnitRoles(oldUnits, wantUnits) {
		return false, nil
	}
	for unit := range wantUnits {
		chain, err := unitChain(ctx, tx, t.ID, unit)
		if err != nil {
			return false, err
		}
		if chain == nil {
			return false, failure(422, "org_unit_not_found")
		}
	}
	if state == "active" && oldState != "active" {
		if err := oneTenantPerAccount(ctx, tx, t.ID, subject); err != nil {
			return false, err
		}
		// 加入或重新加入：先清除该主体此前留在本租户资源上的参与关系，避免旧参与关系随重新加入复活（identity.md 7.1）。
		if err := d.purgeParticipation(ctx, tx, t.ID, subject); err != nil {
			return false, err
		}
	}
	removed := oldState == "active" && state != "active"
	if oldState == "active" && slices.Contains(oldRoles, "admin") && (state != "active" || !slices.Contains(roles, "admin")) {
		var n int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM identity_members WHERE tenant_id=$1 AND state='active' AND roles @> ARRAY['admin']`, t.ID).Scan(&n); err != nil {
			return false, err
		}
		if n <= 1 {
			return false, failure(409, "last_admin")
		}
	}
	result.MemberVersion++
	if _, err = tx.Exec(ctx, `INSERT INTO identity_members(tenant_id,subject,state,roles,version) VALUES($1,$2,$3,$4,$5) ON CONFLICT(tenant_id,subject) DO UPDATE SET state=EXCLUDED.state,roles=EXCLUDED.roles,version=EXCLUDED.version`, t.ID, subject, state, roles, result.MemberVersion); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM identity_member_unit_roles WHERE tenant_id=$1 AND subject=$2`, t.ID, subject); err != nil {
		return false, err
	}
	for unit, bound := range wantUnits {
		if _, err = tx.Exec(ctx, `INSERT INTO identity_member_unit_roles(tenant_id,subject,org_unit_id,roles) VALUES($1,$2,$3,$4)`, t.ID, subject, unit, bound); err != nil {
			return false, err
		}
	}
	return removed, emit(ctx, tx, "identity.member.changed.v1", t.ID, memberEvent(result))
}

// oneTenantPerAccount 确认账号不是其他租户的有效成员（PRD 第1段第13节第2项）。按账号加事务级咨询锁，
// 不同租户对同一账号的并发加入（成员命令或接受邀请）串行，不会同时成功。
func oneTenantPerAccount(ctx context.Context, tx pgx.Tx, tenantID, subject string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "identity-account/"+subject); err != nil {
		return err
	}
	var other bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM identity_members WHERE subject=$1 AND tenant_id<>$2 AND state='active')`, subject, tenantID).Scan(&other); err != nil {
		return err
	}
	if other {
		return failure(409, "account_in_other_tenant")
	}
	return nil
}

func sameUnitRoles(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for unit, roles := range a {
		if !slices.Equal(roles, b[unit]) {
			return false
		}
	}
	return true
}

// GetTenant/GetMember expose current versions needed to recover from a command conflict.
func (d *Directory) GetTenant(ctx context.Context, id string) (*identityapi.Tenant, error) {
	if err := d.platformAdmin(ctx); err != nil {
		return nil, err
	}
	var value *identityapi.Tenant
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		value, err = tenant(ctx, tx, id, false)
		return err
	})
	return value, err
}

// GetMember 读取成员的当前状态与版本（成员管理：can_manage_members + 15分钟内的强认证）。
func (d *Directory) GetMember(ctx context.Context, subject string) (*api.Session, error) {
	var value *api.Session
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rc, err := d.admin(ctx, tx)
		if err != nil {
			return err
		}
		value, err = readMember(ctx, tx, rc.Tenant, subject, rc.Cell, false)
		return err
	})
	return value, err
}
