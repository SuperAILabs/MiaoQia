package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/domain"
)

// Objective 是一个租户目标（TenantObjective）。首版策略草案随第7段的策略草案实现创建（实现中发现的设计缺口）。
type Objective struct {
	ID                    string                  `json:"id"`
	TemplateID            string                  `json:"template_id"`
	TemplateVersion       string                  `json:"template_version"`
	PolicyTemplateID      string                  `json:"policy_template_id"`
	PolicyTemplateVersion string                  `json:"policy_template_version"`
	OrgUnitID             string                  `json:"org_unit_id"`
	State                 string                  `json:"state"`
	Owner                 string                  `json:"owner"`
	Participants          []string                `json:"participants"`
	Params                map[string]domain.Param `json:"params"`
	Version               int64                   `json:"version"`
}

// ObjectiveInput 是启用或调整目标的输入；Owner 为空时启用取当前用户。
type ObjectiveInput struct {
	TemplateID, OrgUnitID string
	Owner                 string
	Participants          []string
	Params                map[string]domain.Param
}

var subjectPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,127}$`)

// Templates 列出目标目录（can_view_profile），按当前档案的价值链角色判断可启用。
func (s *Service) Templates(ctx context.Context) ([]string, []domain.TemplateView, error) {
	rc, err := s.require(ctx, "can_view_profile")
	if err != nil {
		return nil, nil, err
	}
	var profile domain.Profile
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		st, err := readState(ctx, tx, rc.Tenant, false)
		if err != nil {
			return err
		}
		profile, err = readProfile(ctx, tx, rc.Tenant, st.Profile)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return profile.Roles, domain.Templates(s.Catalog, profile.Roles), nil
}

const objectiveColumns = `id, template_id, template_version, policy_template_id, policy_template_version, org_unit_id, state, owner, participants, params, version`

func scanObjective(row pgx.Row) (*Objective, error) {
	var o Objective
	var params []byte
	if err := row.Scan(&o.ID, &o.TemplateID, &o.TemplateVersion, &o.PolicyTemplateID, &o.PolicyTemplateVersion, &o.OrgUnitID, &o.State,
		&o.Owner, &o.Participants, &params, &o.Version); err != nil {
		return nil, err
	}
	return &o, json.Unmarshal(params, &o.Params)
}

func checkPeople(owner string, participants []string) error {
	if !subjectPattern.MatchString(owner) {
		return failure(400, "invalid_request")
	}
	seen := map[string]bool{}
	for _, p := range participants {
		if !subjectPattern.MatchString(p) || seen[p] {
			return failure(400, "invalid_request")
		}
		seen[p] = true
	}
	return nil
}

// EnableObjective 启用目标（目标单元上的 can_create_objective）：目标须在目录中可启用，参数按模板与当前档案补全校验；
// 创建状态为未激活的目标，激活前不产生信号。同一单元同一模板只启用一次（含已停用的目标，重新启用经 SetObjectiveState）。
func (s *Service) EnableObjective(ctx context.Context, in ObjectiveInput, key string) (*Objective, error) {
	rc, err := s.require(ctx, canCreateObjective...)
	if err != nil {
		return nil, err
	}
	if in.OrgUnitID != api.RootUnit {
		return nil, failure(404, "org_unit_not_found")
	}
	if in.Owner == "" {
		in.Owner = rc.Principal.ID
	}
	if in.Participants == nil {
		in.Participants = []string{}
	}
	if err := checkPeople(in.Owner, in.Participants); err != nil {
		return nil, err
	}
	out := &Objective{}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return command(ctx, tx, key, "objective.enable", in, out, func() error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "ctx-objective/"+rc.Tenant+"/"+in.OrgUnitID+"/"+in.TemplateID); err != nil {
				return err
			}
			var existing string
			err := tx.QueryRow(ctx, `SELECT id FROM ctx_objectives WHERE tenant_id = $1 AND org_unit_id = $2 AND template_id = $3`, rc.Tenant, in.OrgUnitID, in.TemplateID).Scan(&existing)
			if err == nil {
				return failureDetail(409, "objective_exists", existing)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			st, err := readState(ctx, tx, rc.Tenant, false)
			if err != nil {
				return err
			}
			profile, err := readProfile(ctx, tx, rc.Tenant, st.Profile)
			if err != nil {
				return err
			}
			view, ok := s.template(profile, in.TemplateID)
			if !ok {
				return failure(404, "objective_template_not_found")
			}
			if !view.Enableable {
				return failureDetail(422, "objective_not_enableable", view.UnavailableReason)
			}
			params, err := domain.ResolveParams(s.Catalog, *view.Policy, in.Params, profile, nil)
			if err != nil {
				return err
			}
			*out = Objective{ID: ids.New("obj"), TemplateID: view.Objective.ID, TemplateVersion: view.Objective.Version,
				PolicyTemplateID: view.Policy.ID, PolicyTemplateVersion: view.Policy.Version, OrgUnitID: in.OrgUnitID,
				State: domain.StateInactive, Owner: in.Owner, Participants: in.Participants, Params: params, Version: 1}
			if _, err := tx.Exec(ctx, `INSERT INTO ctx_objectives (`+objectiveColumns+`, tenant_id, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
				out.ID, out.TemplateID, out.TemplateVersion, out.PolicyTemplateID, out.PolicyTemplateVersion, out.OrgUnitID, out.State, out.Owner,
				out.Participants, out.Params, out.Version, rc.Tenant, rc.Principal.ID); err != nil {
				return err
			}
			return emitObjective(ctx, tx, out, "enabled", "can_create_objective")
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) template(profile domain.Profile, id string) (domain.TemplateView, bool) {
	for _, v := range domain.Templates(s.Catalog, profile.Roles) {
		if v.Objective.ID == id {
			return v, true
		}
	}
	return domain.TemplateView{}, false
}

func emitObjective(ctx context.Context, tx pgx.Tx, o *Objective, change, basis string) error {
	return emit(ctx, tx, ObjectiveChanged, o.ID, map[string]any{"objective_id": o.ID, "version": o.Version, "template_id": o.TemplateID,
		"template_version": o.TemplateVersion, "org_unit_id": o.OrgUnitID, "state": o.State, "change": change},
		"customer_context.objective."+change, fmt.Sprintf("%s/v%d", o.ID, o.Version), basis)
}

// lockObjective 在事务中锁定目标并核对版本；不存在返回 404。
func lockObjective(ctx context.Context, tx pgx.Tx, id string, expected int64) (*Objective, error) {
	o, err := scanObjective(tx.QueryRow(ctx, `SELECT `+objectiveColumns+` FROM ctx_objectives WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, failure(404, "objective_not_found")
	}
	if err != nil {
		return nil, err
	}
	if o.Version != expected {
		return nil, failureDetail(409, "version_conflict", fmt.Sprintf("当前目标版本 %d", o.Version))
	}
	return o, nil
}

// ConfigureObjective 调整负责人、参与人与参数（can_manage_participants、can_edit，二者都是目标单元的决策者）。
// 参数是下一份策略草案的输入，不改变已生效的策略版本；未修改的本目标值不按当前档案重新校验。
// 目标所用的策略模板已有新版本时，参数调整须经第7段的模板升级，本接口只接受负责人与参与人的调整。
func (s *Service) ConfigureObjective(ctx context.Context, id string, expected int64, in ObjectiveInput, key string) (*Objective, error) {
	rc, err := s.require(ctx, canEditObjective...)
	if err != nil {
		return nil, s.hideObjective(ctx, rc, err)
	}
	if in.Participants == nil {
		in.Participants = []string{}
	}
	if err := checkPeople(in.Owner, in.Participants); err != nil {
		return nil, err
	}
	out := &Objective{}
	request := struct {
		ID       string         `json:"id"`
		Expected int64          `json:"expected_version"`
		Input    ObjectiveInput `json:"input"`
	}{id, expected, in}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return command(ctx, tx, key, "objective.configure", request, out, func() error {
			o, err := lockObjective(ctx, tx, id, expected)
			if err != nil {
				return err
			}
			st, err := readState(ctx, tx, rc.Tenant, false)
			if err != nil {
				return err
			}
			profile, err := readProfile(ctx, tx, rc.Tenant, st.Profile)
			if err != nil {
				return err
			}
			params := o.Params
			tpl, ok := s.Catalog.PolicyTemplates[o.PolicyTemplateID]
			if !ok || tpl.Version != o.PolicyTemplateVersion {
				if in.Params != nil && !sameParams(in.Params, o.Params) {
					return failure(409, "template_version_changed")
				}
			} else if params, err = domain.ResolveParams(s.Catalog, tpl, in.Params, profile, o.Params); err != nil {
				return err
			}
			if o.Owner == in.Owner && slices.Equal(o.Participants, in.Participants) && sameParams(params, o.Params) {
				*out = *o
				return nil
			}
			o.Owner, o.Participants, o.Params, o.Version = in.Owner, in.Participants, params, o.Version+1
			if _, err := tx.Exec(ctx, `UPDATE ctx_objectives SET owner = $2, participants = $3, params = $4, version = $5, updated_at = now() WHERE id = $1`,
				o.ID, o.Owner, o.Participants, o.Params, o.Version); err != nil {
				return err
			}
			*out = *o
			return emitObjective(ctx, tx, o, "configured", "can_edit")
		})
	})
	if err != nil {
		return nil, s.hideObjective(ctx, rc, err)
	}
	return out, nil
}

func sameParams(a, b map[string]domain.Param) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// SetObjectiveState 停用或重新启用目标（can_edit）。生效只由策略激活设置（第7段）。
func (s *Service) SetObjectiveState(ctx context.Context, id string, expected int64, target, key string) (*Objective, error) {
	rc, err := s.require(ctx, canEditObjective...)
	if err != nil {
		return nil, s.hideObjective(ctx, rc, err)
	}
	if target != domain.StateInactive && target != domain.StateDisabled {
		return nil, failure(400, "invalid_request")
	}
	out := &Objective{}
	request := struct {
		ID       string `json:"id"`
		Expected int64  `json:"expected_version"`
		State    string `json:"state"`
	}{id, expected, target}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return command(ctx, tx, key, "objective.state", request, out, func() error {
			o, err := lockObjective(ctx, tx, id, expected)
			if err != nil {
				return err
			}
			change, err := domain.Transition(o.State, target)
			if err != nil || change == "" {
				*out = *o
				return err
			}
			o.State, o.Version = target, o.Version+1
			if _, err := tx.Exec(ctx, `UPDATE ctx_objectives SET state = $2, version = $3, updated_at = now() WHERE id = $1`, o.ID, o.State, o.Version); err != nil {
				return err
			}
			*out = *o
			return emitObjective(ctx, tx, o, change, "can_edit")
		})
	})
	if err != nil {
		return nil, s.hideObjective(ctx, rc, err)
	}
	return out, nil
}

// hideObjective 让无权查看目标的调用者得到与“不存在”相同的结果，不暴露目标是否存在（PRD 第1段 F5）。
func (s *Service) hideObjective(ctx context.Context, rc reqctx.Context, err error) error {
	var safe *api.Error
	if !errors.As(err, &safe) || safe.Code != "forbidden" || rc.Tenant == "" {
		return err
	}
	if viewer, verr := s.canView(ctx, rc); verr == nil && !viewer {
		return failure(404, "objective_not_found")
	}
	return err
}

func (s *Service) canView(ctx context.Context, rc reqctx.Context) (bool, error) {
	if rc.Principal.Kind != "user" {
		return false, nil
	}
	for _, relation := range canViewObjective {
		ok, err := s.allowed(ctx, rc, relation)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// GetObjective 读取目标（can_view）；无权限与不存在同样返回 404。
func (s *Service) GetObjective(ctx context.Context, id string) (*Objective, error) {
	rc, err := tenantContext(ctx)
	if err != nil {
		return nil, err
	}
	viewer, err := s.canView(ctx, rc)
	if err != nil {
		return nil, err
	}
	if !viewer {
		return nil, failure(404, "objective_not_found")
	}
	var out *Objective
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out, err = scanObjective(tx.QueryRow(ctx, `SELECT `+objectiveColumns+` FROM ctx_objectives WHERE id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(404, "objective_not_found")
		}
		return err
	})
	return out, err
}

// ListObjectives 列出调用者可查看的目标；没有单元级查看关系的成员得到空列表。
func (s *Service) ListObjectives(ctx context.Context) ([]Objective, error) {
	rc, err := tenantContext(ctx)
	if err != nil {
		return nil, err
	}
	viewer, err := s.canView(ctx, rc)
	if err != nil || !viewer {
		return []Objective{}, err
	}
	out := []Objective{}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+objectiveColumns+` FROM ctx_objectives ORDER BY template_id, org_unit_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o, err := scanObjective(rows)
			if err != nil {
				return err
			}
			out = append(out, *o)
		}
		return rows.Err()
	})
	return out, err
}
