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

// Roles is the model adapter; lifecycle state and role bindings are authoritative in SQL.
type Roles interface {
	CheckRoles(context.Context, string, string, []string) (bool, error)
}
type Directory struct {
	DB         *db.DB
	Authorizer authz.Decider
	Roles      Roles
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
func (d *Directory) Allow(ctx context.Context, s *api.Session, relation string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if d == nil || d.Roles == nil {
		return false, failure(503, "identity_unavailable")
	}
	roles := make([]string, 0, len(s.Roles))
	for _, r := range s.Roles {
		roles = append(roles, string(r))
	}
	allowed, err := d.Roles.CheckRoles(ctx, "user:"+s.Subject, relation, roles)
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
	return map[string]any{"tenant_id": s.Tenant, "subject": s.Subject, "state": string(s.State), "version": s.MemberVersion, "roles": s.Roles}
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
	if req.Validate() != nil || (req.State == "removed" && len(req.Roles) != 0) {
		return nil, failure(400, "invalid_request")
	}
	rc, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if !reqctx.MFA(ctx) {
		return nil, failure(403, "mfa_required")
	}
	if rc.Tenant == "" {
		return nil, failure(403, "invalid_tenant_context")
	}
	result := new(api.Session)
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
		return command(ctx, tx, key, "member.put/"+subject, req, result, func() error {
			var version int64
			var oldState string
			var oldRoles []string
			err = tx.QueryRow(ctx, `SELECT state,roles,version FROM identity_members WHERE tenant_id=$1 AND subject=$2`, rc.Tenant, subject).Scan(&oldState, &oldRoles, &version)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if version != req.ExpectedVersion {
				return failure(409, "version_conflict")
			}
			roles := make([]string, 0, len(req.Roles))
			for _, r := range req.Roles {
				roles = append(roles, string(r))
			}
			slices.Sort(roles)
			slices.Sort(oldRoles)
			*result = api.Session{Tenant: t.ID, Cell: "cell-01", Subject: subject, State: identityapi.SessionState(req.State), Roles: make([]identityapi.SessionRolesItem, 0, len(roles)), TenantVersion: t.Version, MemberVersion: version}
			for _, r := range roles {
				result.Roles = append(result.Roles, identityapi.SessionRolesItem(r))
			}
			if string(req.State) == oldState && slices.Equal(oldRoles, roles) {
				return nil
			}
			if oldState == "active" && slices.Contains(oldRoles, "admin") && (req.State != "active" || !slices.Contains(roles, "admin")) {
				var n int
				if err = tx.QueryRow(ctx, `SELECT count(*) FROM identity_members WHERE tenant_id=$1 AND state='active' AND roles @> ARRAY['admin']`, rc.Tenant).Scan(&n); err != nil {
					return err
				}
				if n <= 1 {
					return failure(409, "last_admin")
				}
			}
			result.MemberVersion++
			if _, err = tx.Exec(ctx, `INSERT INTO identity_members(tenant_id,subject,state,roles,version) VALUES($1,$2,$3,$4,$5) ON CONFLICT(tenant_id,subject) DO UPDATE SET state=EXCLUDED.state,roles=EXCLUDED.roles,version=EXCLUDED.version`, rc.Tenant, subject, string(req.State), roles, result.MemberVersion); err != nil {
				return err
			}
			return emit(ctx, tx, "identity.member.changed.v1", rc.Tenant, memberEvent(result))
		})
	})
	return result, err
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
func (d *Directory) GetMember(ctx context.Context, subject string) (*api.Session, error) {
	rc, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if !reqctx.MFA(ctx) {
		return nil, failure(403, "mfa_required")
	}
	var value *api.Session
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
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
		value, err = readMember(ctx, tx, rc.Tenant, subject, rc.Cell, false)
		return err
	})
	return value, err
}
