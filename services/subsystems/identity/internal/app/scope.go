package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// Store 是授权模型的求值与关系存储（infra.OpenFGA）。
type Store interface {
	Evaluate(ctx context.Context, subject, relation, object string, tuples [][3]string) (bool, error)
	ReadObject(ctx context.Context, object string) ([][3]string, error)
	ReadUser(ctx context.Context, user, objectType string) ([][3]string, error)
	Write(ctx context.Context, adds, deletes [][3]string) error
}

// RootUnit 是每个租户根组织单元的保留标识。
const RootUnit = "root"

// MaxUnitDepth 是单元链（含根单元）的最大层数：限制每次决策的上下文关系数量。
const MaxUnitDepth = 8

// platformObject 是服务授权所在的平台对象（infra.PlatformObject）。
const platformObject = "platform:miaoqia"

// resourceRelations 是可登记业务资源的类型与其参与关系（model.fga；SyncResourceParticipants 只接受这些关系）。
var resourceRelations = map[string][]string{
	"objective":     {"owner", "participant"},
	"pursuit":       {"owner", "collaborator"},
	"research_case": {"owner", "shared_with"},
}

// scope 是一次授权决策的上下文关系。目录中的租户与单元挂在本次决策独有的不可预测对象上，
// 持久化在 OpenFGA 中的旧关系（若有）不会参与求值；业务资源使用真实对象，以便合入其上持久化的参与关系。
type scope struct {
	nonce  string
	tuples [][3]string
}

func newScope() (*scope, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &scope{nonce: hex.EncodeToString(b)}, nil
}

func (s *scope) tenant() string { return "tenant:directory-" + s.nonce }

func (s *scope) unit(id string) string { return "org_unit:directory-" + s.nonce + "." + id }

// chain 写入单元层级：根单元的上级是租户，其他单元的上级是链中的下一个单元。chain 从单元到根单元。
func (s *scope) chain(chain []string) {
	for i, u := range chain {
		parent := s.tenant()
		if i+1 < len(chain) {
			parent = s.unit(chain[i+1])
		}
		s.tuples = append(s.tuples, [3]string{parent, "parent", s.unit(u)})
	}
}

// member 写入成员资格、租户上的角色与链上单元的角色（链外单元的角色与本次决策无关，不写入）。
func (s *scope) member(subject string, roles []string, unitRoles map[string][]string, chain []string) {
	s.tuples = append(s.tuples, [3]string{subject, "member", s.tenant()})
	for _, r := range roles {
		s.tuples = append(s.tuples, [3]string{subject, r, s.tenant()})
	}
	for _, u := range chain {
		for _, r := range unitRoles[u] {
			s.tuples = append(s.tuples, [3]string{subject, r, s.unit(u)})
		}
	}
}

// unitChain 返回租户内单元到根单元的链；单元不存在时返回 nil。
func unitChain(ctx context.Context, tx pgx.Tx, tenantID, id string) ([]string, error) {
	rows, err := tx.Query(ctx, `WITH RECURSIVE c(id, parent_id, depth) AS (
 SELECT id, parent_id, 1 FROM identity_org_units WHERE tenant_id = $1 AND id = $2
 UNION ALL SELECT u.id, u.parent_id, c.depth + 1 FROM identity_org_units u JOIN c ON u.tenant_id = $1 AND u.id = c.parent_id WHERE c.depth < 32)
SELECT id FROM c ORDER BY depth`, tenantID, id)
	if err != nil {
		return nil, err
	}
	chain, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(chain) == 0 {
		return nil, err
	}
	if chain[len(chain)-1] != RootUnit {
		return nil, errors.New("组织单元链未到达根单元")
	}
	return chain, nil
}

// unitRoles 读出成员绑定在单元上的角色。
func unitRoles(ctx context.Context, tx pgx.Tx, tenantID, subject string) (map[string][]string, error) {
	rows, err := tx.Query(ctx, `SELECT org_unit_id, roles FROM identity_member_unit_roles WHERE tenant_id = $1 AND subject = $2 ORDER BY org_unit_id`, tenantID, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var unit string
		var roles []string
		if err := rows.Scan(&unit, &roles); err != nil {
			return nil, err
		}
		slices.Sort(roles)
		out[unit] = roles
	}
	return out, rows.Err()
}

func sessionRoles(s *api.Session) []string {
	roles := make([]string, 0, len(s.Roles))
	for _, r := range s.Roles {
		roles = append(roles, string(r))
	}
	return roles
}

func sessionUnitRoles(s *api.Session) map[string][]string {
	out := map[string][]string{}
	for _, b := range s.UnitRoles {
		for _, r := range b.Roles {
			out[string(b.OrgUnitID)] = append(out[string(b.OrgUnitID)], string(r))
		}
	}
	return out
}

func toUnitRoles(m map[string][]string) identityapi.UnitRoles {
	if len(m) == 0 {
		return nil
	}
	units := make([]string, 0, len(m))
	for u := range m {
		units = append(units, u)
	}
	slices.Sort(units)
	out := make(identityapi.UnitRoles, 0, len(units))
	for _, u := range units {
		item := identityapi.UnitRolesItem{OrgUnitID: identityapi.OrgUnitID(u)}
		for _, r := range m[u] {
			item.Roles = append(item.Roles, identityapi.UnitRolesItemRolesItem(r))
		}
		out = append(out, item)
	}
	return out
}

// target 是一次决策的资源：在租户内解析出的单元链与求值对象。
type target struct {
	chain    []string // 资源所在单元到根单元；tenant 资源为空
	resource string   // 业务资源（真实对象）；tenant 与 org_unit 为空
}

// resolveTarget 在上下文租户内解析资源。不属于本租户、未登记或类型不受支持的资源返回 nil（决策为拒绝）。
func resolveTarget(ctx context.Context, tx pgx.Tx, tenantID, object string) (*target, error) {
	kind, id, ok := strings.Cut(object, ":")
	if !ok {
		return nil, nil
	}
	switch {
	case kind == "tenant":
		if id != tenantID {
			return nil, nil
		}
		return &target{}, nil
	case kind == "org_unit":
		chain, err := unitChain(ctx, tx, tenantID, id)
		if chain == nil || err != nil {
			return nil, err
		}
		return &target{chain: chain}, nil
	case resourceRelations[kind] != nil:
		var unit string
		err := tx.QueryRow(ctx, `SELECT org_unit_id FROM identity_resources WHERE object = $1 AND tenant_id = $2 AND applied_version > 0`, object, tenantID).Scan(&unit)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		chain, err := unitChain(ctx, tx, tenantID, unit)
		if chain == nil || err != nil {
			return nil, err
		}
		return &target{chain: chain, resource: object}, nil
	}
	return nil, nil
}

// evaluate 以当前目录状态求值成员对资源的关系。
func (d *Directory) evaluate(ctx context.Context, s *api.Session, relation string, t *target) (bool, error) {
	sc, err := newScope()
	if err != nil {
		return false, err
	}
	subject := "user:" + s.Subject
	sc.member(subject, sessionRoles(s), sessionUnitRoles(s), t.chain)
	object := sc.tenant()
	if len(t.chain) > 0 {
		sc.chain(t.chain)
		object = sc.unit(t.chain[0])
	}
	if t.resource != "" {
		sc.tuples = append(sc.tuples, [3]string{sc.unit(t.chain[0]), "unit", t.resource})
		object = t.resource
	}
	return d.store().Evaluate(ctx, subject, relation, object, sc.tuples)
}

func (d *Directory) store() Store {
	if d == nil || d.FGA == nil {
		return unavailable{}
	}
	return d.FGA
}

type unavailable struct{}

func (unavailable) Evaluate(context.Context, string, string, string, [][3]string) (bool, error) {
	return false, failure(503, "identity_unavailable")
}
func (unavailable) ReadObject(context.Context, string) ([][3]string, error) {
	return nil, failure(503, "identity_unavailable")
}
func (unavailable) ReadUser(context.Context, string, string) ([][3]string, error) {
	return nil, failure(503, "identity_unavailable")
}
func (unavailable) Write(context.Context, [][3]string, [][3]string) error {
	return failure(503, "identity_unavailable")
}

// decisionError 把求值错误转换为可暴露的错误：请求无效保持 ErrInvalid，其余为 503。
func decisionError(err error) error {
	var safe *api.Error
	if errors.Is(err, authz.ErrInvalid) || errors.As(err, &safe) {
		return err
	}
	return failure(503, "identity_unavailable")
}

// CheckMember 是带租户上下文的用户授权决策：当前成员资格、租户与单元角色、单元层级、资源所在单元来自目录，
// 资源上的参与关系来自 OpenFGA。资源不属于本租户或未登记时为拒绝。
func (d *Directory) CheckMember(ctx context.Context, relation, object string) (bool, error) {
	rc, err := actor(ctx)
	if err != nil {
		return false, err
	}
	if rc.Tenant == "" {
		return false, failure(403, "invalid_tenant_context")
	}
	var s *api.Session
	var t *target
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if s, err = session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell); err != nil {
			return err
		}
		t, err = resolveTarget(ctx, tx, rc.Tenant, object)
		return err
	})
	if err != nil || t == nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	allowed, err := d.evaluate(ctx, s, relation, t)
	if err != nil {
		return false, decisionError(err)
	}
	return allowed, nil
}

// serviceTenant 核对服务主体的租户上下文：租户存在、处于启用状态、cell 与目录一致。服务主体不是租户成员。
func serviceTenant(ctx context.Context, tx pgx.Tx, rc reqctx.Context) error {
	t, err := tenant(ctx, tx, rc.Tenant, false)
	if err != nil {
		return err
	}
	if t.State != "active" || string(t.Cell) != rc.Cell {
		return failure(403, "invalid_tenant_context")
	}
	return nil
}

// CheckService 是带租户上下文的服务主体授权决策：只判断上下文租户上的服务动作（model.fga 的 can_read_context 等），
// 依据 identity 服务授权清单写入的平台关系。
func (d *Directory) CheckService(ctx context.Context, relation, object string) (bool, error) {
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Validate() != nil || rc.Principal.Kind != "service" || rc.Tenant == "" {
		return false, failure(403, "invalid_tenant_context")
	}
	if object != "tenant:"+rc.Tenant {
		return false, nil
	}
	if err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error { return serviceTenant(ctx, tx, rc) }); err != nil {
		return false, err
	}
	sc, err := newScope()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	allowed, err := d.store().Evaluate(ctx, authz.Subject(rc.Principal), relation, sc.tenant(), [][3]string{{platformObject, "platform", sc.tenant()}})
	if err != nil {
		return false, decisionError(err)
	}
	return allowed, nil
}
