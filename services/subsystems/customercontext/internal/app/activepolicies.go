package app

import (
	"context"
	"encoding/json"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/domain"
)

// 生效策略读取（GetActivePolicies，customer-context.md 第6节；PRD 第7段第5节生效边界）：detection 每批求值前以服务主体读取
// 各目标当前的策略代次与生效版本。激活与回滚在同一事务中写入生效版本并递增代次（switchActive），本读取读主库，
// 因此激活或回滚返回后开始的读取一律看到新代次；返回的文档与启用领域集合即该版本激活时固定的内容。

// ActivePolicy 是一个目标的生效策略（契约 ActivePolicy）。
type ActivePolicy struct {
	ObjectiveID     string          `json:"objective_id"`
	ObjectiveCode   string          `json:"objective_code"`
	ObjectiveState  string          `json:"objective_state"`
	OrgUnitID       string          `json:"org_unit_id"`
	PolicyID        string          `json:"policy_id"`
	Generation      int64           `json:"generation"`
	Version         int64           `json:"version"`
	TemplateID      string          `json:"template_id"`
	TemplateVersion string          `json:"template_version"`
	EnabledDomains  []string        `json:"enabled_domains"`
	Document        json.RawMessage `json:"document"`
	EffectiveAt     time.Time       `json:"effective_at"`
	// 目标当前版本的负责人与参与人（契约 0.6.0，C-08）：channels 据此确定 route 受众的即时通知候选收件人，发送前以收件人本人再检查访问
	Owner        string   `json:"owner,omitempty"`
	Participants []string `json:"participants"`
	// 目标当前的分派配置（契约 0.7.0，C-14）：detection 生成信号时以第一条命中的规则给出候选负责人，只从同一次读取的负责人与参与人中选
	Assignment *ActiveAssignment `json:"assignment,omitempty"`
}

// ActiveAssignment 是生效策略读取中的分派配置（契约 ActiveAssignment）：版本（从未配置为 0）与按顺序的规则。
type ActiveAssignment struct {
	Version int64                   `json:"version"`
	Rules   []domain.AssignmentRule `json:"rules"`
}

// ActivePolicyPage 是一页生效策略（契约 ActivePolicyList）。
type ActivePolicyPage struct {
	Policies   []ActivePolicy `json:"policies"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// MaxActivePolicies 是每页的条数上限。
const MaxActivePolicies = 200

var objectiveCursor = regexp.MustCompile(`^obj_[0-9A-HJKMNP-TV-Z]{26}$`)

// ActivePolicies 返回本租户有生效版本的目标（按目标标识升序分页）。只供有 can_read_context 的服务主体；用户主体 403。
func (s *Service) ActivePolicies(ctx context.Context, limit int, cursor string) (*ActivePolicyPage, error) {
	rc, err := tenantContext(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Principal.Kind != "service" {
		return nil, failure(403, "forbidden")
	}
	if _, err := s.requireReader(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxActivePolicies {
		limit = MaxActivePolicies
	}
	if cursor != "" && !objectiveCursor.MatchString(cursor) {
		return nil, failure(400, "invalid_cursor")
	}
	out := &ActivePolicyPage{Policies: []ActivePolicy{}}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT o.id, o.template_id, o.state, o.org_unit_id, p.policy_id, p.generation, p.active_version,
 v.template_id, v.template_version, v.enabled_domains, v.document, a.effective_at, o.owner, o.participants,
 COALESCE(r.version, 0), COALESCE(r.rules, '[]'::jsonb)
 FROM ctx_policies p
 JOIN ctx_objectives o ON o.id = p.objective_id
 JOIN ctx_policy_versions v ON v.objective_id = p.objective_id AND v.version = p.active_version
 JOIN ctx_policy_activations a ON a.objective_id = p.objective_id AND a.generation = p.generation
 LEFT JOIN ctx_assignment_rules r ON r.objective_id = p.objective_id
 WHERE p.active_version > 0 AND p.objective_id > $1
 ORDER BY p.objective_id LIMIT $2`, cursor, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a ActivePolicy
			var doc, rules []byte
			assignment := &ActiveAssignment{Rules: []domain.AssignmentRule{}}
			if err := rows.Scan(&a.ObjectiveID, &a.ObjectiveCode, &a.ObjectiveState, &a.OrgUnitID, &a.PolicyID, &a.Generation, &a.Version,
				&a.TemplateID, &a.TemplateVersion, &a.EnabledDomains, &doc, &a.EffectiveAt, &a.Owner, &a.Participants, &assignment.Version, &rules); err != nil {
				return err
			}
			if err := json.Unmarshal(rules, &assignment.Rules); err != nil {
				return err
			}
			a.Assignment = assignment
			a.Document, a.EffectiveAt = doc, a.EffectiveAt.UTC()
			if a.EnabledDomains == nil {
				a.EnabledDomains = []string{}
			}
			if a.Participants == nil {
				a.Participants = []string{}
			}
			out.Policies = append(out.Policies, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if len(out.Policies) > limit {
		out.Policies = out.Policies[:limit]
		out.NextCursor = out.Policies[limit-1].ObjectiveID
	}
	return out, nil
}
