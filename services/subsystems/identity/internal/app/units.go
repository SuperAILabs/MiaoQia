package app

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// 组织单元（identity.md 7.1）：每个租户一个根单元 root（随租户创建），其下可建多级子单元。单元层级在授权决策时
// 从本库读出并作为上下文关系求值，移动单元立即改变此后决策中继承的角色范围。

type unitRow struct {
	id, name string
	parent   *string
	version  int64
}

func (u unitRow) out(chain []string) *identityapi.OrgUnit {
	o := &identityapi.OrgUnit{ID: identityapi.OrgUnitID(u.id), Name: u.name, Version: u.version}
	if u.parent != nil {
		o.ParentID = identityapi.NewOptOrgUnitID(identityapi.OrgUnitID(*u.parent))
	}
	for _, c := range chain {
		o.Chain = append(o.Chain, identityapi.OrgUnitID(c))
	}
	return o
}

func readUnit(ctx context.Context, tx pgx.Tx, tenantID, id string, lock bool) (*unitRow, error) {
	q := `SELECT id, name, parent_id, version FROM identity_org_units WHERE tenant_id=$1 AND id=$2`
	if lock {
		q += " FOR UPDATE"
	}
	var u unitRow
	err := tx.QueryRow(ctx, q, tenantID, id).Scan(&u.id, &u.name, &u.parent, &u.version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, failure(404, "org_unit_not_found")
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// caller 核对带租户上下文的调用者：用户须为当前有效成员；服务主体须带有效租户上下文（租户启用、cell 一致）。
func caller(ctx context.Context, tx pgx.Tx) (reqctx.Context, error) {
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Validate() != nil || rc.Tenant == "" {
		return rc, failure(403, "invalid_tenant_context")
	}
	switch rc.Principal.Kind {
	case "user":
		_, err := session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell)
		return rc, err
	case "service":
		return rc, serviceTenant(ctx, tx, rc)
	}
	return rc, failure(403, "invalid_tenant_context")
}

// OrgUnit 返回上下文租户内的单元及其单元链（成员或带租户上下文的服务主体）。
func (d *Directory) OrgUnit(ctx context.Context, id string) (*identityapi.OrgUnit, error) {
	if identityapi.OrgUnitID(id).Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	var out *identityapi.OrgUnit
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rc, err := caller(ctx, tx)
		if err != nil {
			return err
		}
		u, err := readUnit(ctx, tx, rc.Tenant, id, false)
		if err != nil {
			return err
		}
		chain, err := unitChain(ctx, tx, rc.Tenant, id)
		if err != nil {
			return err
		}
		out = u.out(chain)
		return nil
	})
	return out, err
}

// ListOrgUnits 列出本租户全部单元（当前有效成员）。
func (d *Directory) ListOrgUnits(ctx context.Context) (*identityapi.OrgUnitList, error) {
	out := &identityapi.OrgUnitList{}
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rc, err := actor(ctx)
		if err != nil {
			return err
		}
		if _, err := session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, name, parent_id, version FROM identity_org_units WHERE tenant_id=$1`, rc.Tenant)
		if err != nil {
			return err
		}
		units := map[string]unitRow{}
		for rows.Next() {
			var u unitRow
			if err := rows.Scan(&u.id, &u.name, &u.parent, &u.version); err != nil {
				rows.Close()
				return err
			}
			units[u.id] = u
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		chainOf := func(id string) []string {
			var chain []string
			for cur := id; len(chain) <= MaxUnitDepth*4; {
				chain = append(chain, cur)
				p := units[cur].parent
				if p == nil {
					break
				}
				cur = *p
			}
			return chain
		}
		for id, u := range units {
			out.Units = append(out.Units, *u.out(chainOf(id)))
		}
		// 按从根单元开始的路径排序，上级在下级之前
		path := func(u identityapi.OrgUnit) string {
			parts := make([]string, len(u.Chain))
			for i, c := range u.Chain {
				parts[len(u.Chain)-1-i] = string(c)
			}
			return strings.Join(parts, "/")
		}
		slices.SortFunc(out.Units, func(a, b identityapi.OrgUnit) int { return strings.Compare(path(a), path(b)) })
		return nil
	})
	return out, err
}

// manager 核对单元与成员管理的调用者：用户、MFA、当前成员且有 can_manage_members（与成员命令相同）。
func (d *Directory) manager(ctx context.Context, tx pgx.Tx) (reqctx.Context, error) {
	rc, err := actor(ctx)
	if err != nil {
		return rc, err
	}
	if !reqctx.MFA(ctx) {
		return rc, failure(403, "mfa_required")
	}
	if rc.Tenant == "" {
		return rc, failure(403, "invalid_tenant_context")
	}
	if _, err := tenant(ctx, tx, rc.Tenant, false); err != nil {
		return rc, err
	}
	s, err := session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell)
	if err != nil {
		return rc, err
	}
	allowed, err := d.Allow(ctx, s, "can_manage_members")
	if err != nil {
		return rc, err
	}
	if !allowed {
		return rc, failure(403, "forbidden")
	}
	// 单元结构变更在租户内串行，并发移动不会形成环
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "identity-units/"+rc.Tenant)
	return rc, err
}

func unitName(name string) (string, error) {
	if strings.TrimSpace(name) != name || name == "" {
		return "", failure(400, "invalid_request")
	}
	return name, nil
}

// CreateOrgUnit 在上级单元下创建单元（can_manage_members + MFA）。
func (d *Directory) CreateOrgUnit(ctx context.Context, req *identityapi.CreateOrgUnitRequest, key string) (*identityapi.OrgUnit, error) {
	if req.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	if _, err := unitName(req.Name); err != nil {
		return nil, err
	}
	out := &identityapi.OrgUnit{}
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rc, err := d.manager(ctx, tx)
		if err != nil {
			return err
		}
		return command(ctx, tx, key, "org_unit.create", req, out, func() error {
			chain, err := unitChain(ctx, tx, rc.Tenant, string(req.ParentID))
			if err != nil {
				return err
			}
			if chain == nil {
				return failure(404, "org_unit_not_found")
			}
			if len(chain)+1 > MaxUnitDepth {
				return failure(422, "org_unit_too_deep")
			}
			parent := string(req.ParentID)
			u := unitRow{id: ids.New("unt"), name: req.Name, parent: &parent, version: 1}
			if _, err := tx.Exec(ctx, `INSERT INTO identity_org_units(tenant_id,id,parent_id,name,version) VALUES($1,$2,$3,$4,1)`, rc.Tenant, u.id, parent, u.name); err != nil {
				return err
			}
			*out = *u.out(append([]string{u.id}, chain...))
			return (audit.Recorder{Subsystem: api.Name}).Record(ctx, tx, audit.Entry{Action: "identity.org_unit.create", Resource: u.id, Result: "succeeded", Basis: "can_manage_members"})
		})
	})
	return out, err
}

// UpdateOrgUnit 重命名或移动单元（can_manage_members + MFA，expected_version）。根单元只能重命名；
// 不能移到自身或下级之下；移动后整棵子树的单元链不超过 MaxUnitDepth。
func (d *Directory) UpdateOrgUnit(ctx context.Context, id string, req *identityapi.UpdateOrgUnitRequest, key string) (*identityapi.OrgUnit, error) {
	if req.Validate() != nil || identityapi.OrgUnitID(id).Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	if _, err := unitName(req.Name); err != nil {
		return nil, err
	}
	parent, hasParent := req.ParentID.Get()
	if (id == RootUnit) == hasParent {
		if id == RootUnit {
			return nil, failure(422, "root_unit_fixed")
		}
		return nil, failure(400, "invalid_request")
	}
	out := &identityapi.OrgUnit{}
	request := struct {
		ID  string                            `json:"id"`
		Req *identityapi.UpdateOrgUnitRequest `json:"request"`
	}{id, req}
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rc, err := d.manager(ctx, tx)
		if err != nil {
			return err
		}
		return command(ctx, tx, key, "org_unit.update", request, out, func() error {
			u, err := readUnit(ctx, tx, rc.Tenant, id, true)
			if err != nil {
				return err
			}
			if u.version != req.ExpectedVersion {
				return failure(409, "version_conflict")
			}
			moved := hasParent && (u.parent == nil || *u.parent != string(parent))
			if moved {
				chain, err := unitChain(ctx, tx, rc.Tenant, string(parent))
				if err != nil {
					return err
				}
				if chain == nil {
					return failure(404, "org_unit_not_found")
				}
				if slices.Contains(chain, id) {
					return failure(409, "org_unit_cycle")
				}
				var height int
				if err := tx.QueryRow(ctx, `WITH RECURSIVE s(id, depth) AS (
 SELECT id, 1 FROM identity_org_units WHERE tenant_id=$1 AND id=$2
 UNION ALL SELECT u.id, s.depth+1 FROM identity_org_units u JOIN s ON u.tenant_id=$1 AND u.parent_id=s.id WHERE s.depth < 32)
SELECT max(depth) FROM s`, rc.Tenant, id).Scan(&height); err != nil {
					return err
				}
				if len(chain)+height > MaxUnitDepth {
					return failure(422, "org_unit_too_deep")
				}
				p := string(parent)
				u.parent = &p
			}
			if !moved && u.name == req.Name {
				chain, err := unitChain(ctx, tx, rc.Tenant, id)
				*out = *u.out(chain)
				return err
			}
			u.name, u.version = req.Name, u.version+1
			if _, err := tx.Exec(ctx, `UPDATE identity_org_units SET name=$3, parent_id=$4, version=$5 WHERE tenant_id=$1 AND id=$2`, rc.Tenant, id, u.name, u.parent, u.version); err != nil {
				return err
			}
			chain, err := unitChain(ctx, tx, rc.Tenant, id)
			if err != nil {
				return err
			}
			*out = *u.out(chain)
			action := "identity.org_unit.rename"
			if moved {
				action = "identity.org_unit.move"
			}
			return (audit.Recorder{Subsystem: api.Name}).Record(ctx, tx, audit.Entry{Action: action, Resource: id, Result: "succeeded", Basis: "can_manage_members"})
		})
	})
	return out, err
}
