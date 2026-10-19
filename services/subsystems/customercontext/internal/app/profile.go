package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/domain"
)

// 事件类型（contracts/events/customer_context/events.asyncapi.yaml）。
const (
	ProfileUpdated   = "customer_context.profile.updated.v1"
	ObjectiveChanged = "customer_context.objective.changed.v1"
)

// state 是租户的上下文行；Version 为 0 表示从未保存档案。
type state struct {
	Context, Profile int64
	RefreshAfter     *domain.Date
}

func readState(ctx context.Context, tx pgx.Tx, tenant string, lock bool) (state, error) {
	q := `SELECT context_version, profile_version, to_char(refresh_after, 'YYYY-MM-DD') FROM ctx_contexts WHERE tenant_id = $1`
	if lock {
		q += " FOR UPDATE"
	}
	var st state
	var refresh *string
	err := tx.QueryRow(ctx, q, tenant).Scan(&st.Context, &st.Profile, &refresh)
	if errors.Is(err, pgx.ErrNoRows) {
		return state{}, nil
	}
	if refresh != nil {
		d := domain.Date(*refresh)
		st.RefreshAfter = &d
	}
	return st, err
}

func readProfile(ctx context.Context, tx pgx.Tx, tenant string, version int64) (domain.Profile, error) {
	if version == 0 {
		return domain.Empty(), nil
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT content FROM ctx_profile_versions WHERE tenant_id = $1 AND version = $2`, tenant, version).Scan(&raw); err != nil {
		return domain.Profile{}, err
	}
	var p domain.Profile
	return p, json.Unmarshal(raw, &p)
}

// ProfileResult 是档案的一个版本。
type ProfileResult struct {
	Version, ContextVersion int64
	Content                 domain.Profile
}

// GetProfile 读取当前档案（can_view_profile）。
func (s *Service) GetProfile(ctx context.Context) (*ProfileResult, error) {
	rc, err := s.require(ctx, "can_view_profile")
	if err != nil {
		return nil, err
	}
	out := &ProfileResult{}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		st, err := readState(ctx, tx, rc.Tenant, false)
		if err != nil {
			return err
		}
		out.Version = st.Profile
		if st.Profile > 0 {
			if err := tx.QueryRow(ctx, `SELECT context_version FROM ctx_profile_versions WHERE tenant_id = $1 AND version = $2`, rc.Tenant, st.Profile).Scan(&out.ContextVersion); err != nil {
				return err
			}
		}
		out.Content, err = readProfile(ctx, tx, rc.Tenant, st.Profile)
		return err
	})
	return out, err
}

// UpdateProfile 保存档案（can_edit_profile）：校验、乐观并发、形成档案版本与上下文版本、写入根单元快照、事件与审计，同一事务。
// 档案是客户事实，不走审批（PRD 第2段第13节第1项）；保存返回后开始的求值读取到的上下文版本不低于本次版本。
func (s *Service) UpdateProfile(ctx context.Context, expected int64, content domain.Profile, key string) (*ProfileResult, error) {
	rc, err := s.require(ctx, "can_edit_profile")
	if err != nil {
		return nil, err
	}
	if err := content.Validate(s.Catalog); err != nil {
		var bad *domain.Invalid
		if errors.As(err, &bad) {
			return nil, failureDetail(422, bad.Code, bad.Detail)
		}
		return nil, err
	}
	today := s.today()
	out := &ProfileResult{}
	request := struct {
		Expected int64          `json:"expected_version"`
		Content  domain.Profile `json:"content"`
	}{expected, content}
	var warn *overLimit
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return command(ctx, tx, key, "profile.update", request, out, func() error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "ctx-context/"+rc.Tenant); err != nil {
				return err
			}
			st, err := readState(ctx, tx, rc.Tenant, true)
			if err != nil {
				return err
			}
			if st.Profile != expected {
				return failureDetail(409, "version_conflict", "当前档案版本 "+strconv.FormatInt(st.Profile, 10))
			}
			previous, err := readProfile(ctx, tx, rc.Tenant, st.Profile)
			if err != nil {
				return err
			}
			next := state{Context: st.Context + 1, Profile: st.Profile + 1}
			if err := commit(ctx, tx, rc, st, next, content, today); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO ctx_profile_versions (tenant_id, version, context_version, content, created_by) VALUES ($1, $2, $3, $4, $5)`,
				rc.Tenant, next.Profile, next.Context, content, rc.Principal.ID); err != nil {
				return err
			}
			if warn, err = writeSnapshot(ctx, tx, rc.Tenant, next, content, today); err != nil {
				return err
			}
			*out = ProfileResult{Version: next.Profile, ContextVersion: next.Context, Content: content}
			return emit(ctx, tx, ProfileUpdated, "profile", map[string]any{"context_version": next.Context, "profile_version": next.Profile,
				"cause": "profile_saved", "changed": domain.Changed(previous, content)},
				"customer_context.profile.update", fmt.Sprintf("profile/v%d", next.Profile), "can_edit_profile")
		})
	})
	if err != nil {
		return nil, err
	}
	s.report(ctx, warn)
	return out, nil
}

// commit 写入租户上下文行的新版本。档案版本行引用上下文行，首次保存时先插入上下文行。
func commit(ctx context.Context, tx pgx.Tx, rc reqctx.Context, current, next state, content domain.Profile, today domain.Date) error {
	var refresh *string
	if d := domain.RefreshAfter(content, today); d != "" {
		v := string(d)
		refresh = &v
	}
	if current.Context == 0 {
		_, err := tx.Exec(ctx, `INSERT INTO ctx_contexts (tenant_id, context_version, profile_version, refresh_after) VALUES ($1, $2, $3, $4::text::date)`,
			rc.Tenant, next.Context, next.Profile, refresh)
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE ctx_contexts SET context_version = $2, profile_version = $3, refresh_after = $4::text::date, updated_at = now() WHERE tenant_id = $1`,
		rc.Tenant, next.Context, next.Profile, refresh)
	return err
}

// writeSnapshot 物化根单元在新上下文版本的快照。identity 登记组织单元前只有根单元，单元链只含根单元。
// 快照超出规模上界（我方主体超过 10 个）时省略主体、依赖条件为未知，并返回需告警的情况；
// 快照按（租户、单元、上下文版本）只写一次，告警因此每个上下文版本至多一次，读取快照不重复告警。
func writeSnapshot(ctx context.Context, tx pgx.Tx, tenant string, st state, content domain.Profile, today domain.Date) (*overLimit, error) {
	view, warn := domain.Compose(content, today)
	_, err := tx.Exec(ctx, `INSERT INTO ctx_snapshots (tenant_id, org_unit_id, context_version, profile_version, unit_chain, as_of, policy_view)
 VALUES ($1, $2, $3, $4, $5, $6::text::date, $7)`, tenant, api.RootUnit, st.Context, st.Profile, []string{api.RootUnit}, string(today), view)
	if err != nil || !warn {
		return nil, err
	}
	return &overLimit{Tenant: tenant, Unit: api.RootUnit, ContextVersion: st.Context, Entities: len(content.Entities)}, nil
}

// overLimit 是快照超出规模上界的告警（上下文快照契约 policy_view：超出上界不截断而省略，并告警）。
type overLimit struct {
	Tenant, Unit   string
	ContextVersion int64
	Entities       int
}

// SnapshotOverLimit 是告警日志的消息，运维按此消息与字段建立告警规则。
const SnapshotOverLimit = "上下文快照超出规模上界，已省略我方主体，依赖主体的条件为未知"

// report 在事务提交后记录告警（回滚的写入不告警）。
func (s *Service) report(ctx context.Context, w *overLimit) {
	if w == nil {
		return
	}
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	log.WarnContext(ctx, SnapshotOverLimit, "subsystem", api.Name, "tenant", w.Tenant, "org_unit", w.Unit,
		"context_version", w.ContextVersion, "entities", w.Entities, "limit", domain.MaxSnapshotEntities)
}

// ContextVersion 返回租户当前上下文版本（读主库）。快照中有资质的状态会因日期变化时（refresh_after 已到），
// 先在同一事务中形成新的上下文版本与快照、发布事件，再返回新版本：快照保持不可变，返回后开始的求值不会把已过期的资质当作有效。
func (s *Service) ContextVersion(ctx context.Context) (*api.Version, error) {
	rc, err := s.requireReader(ctx)
	if err != nil {
		return nil, err
	}
	var out api.Version
	var warn *overLimit
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		st, w, err := s.current(ctx, tx, rc)
		out, warn = api.Version{ContextVersion: st.Context, ProfileVersion: st.Profile}, w
		return err
	})
	if err != nil {
		return nil, err
	}
	s.report(ctx, warn)
	return &out, nil
}

// current 读取当前状态，必要时按日期刷新快照；刷新形成的快照超出规模上界时返回需在提交后告警的情况。
func (s *Service) current(ctx context.Context, tx pgx.Tx, rc reqctx.Context) (state, *overLimit, error) {
	st, err := readState(ctx, tx, rc.Tenant, false)
	today := s.today()
	if err != nil || st.RefreshAfter == nil || *st.RefreshAfter > today {
		return st, nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "ctx-context/"+rc.Tenant); err != nil {
		return st, nil, err
	}
	if st, err = readState(ctx, tx, rc.Tenant, true); err != nil || st.RefreshAfter == nil || *st.RefreshAfter > today {
		return st, nil, err
	}
	content, err := readProfile(ctx, tx, rc.Tenant, st.Profile)
	if err != nil {
		return st, nil, err
	}
	previous, err := readView(ctx, tx, rc.Tenant, api.RootUnit, st.Context)
	if err != nil {
		return st, nil, err
	}
	view, _ := domain.Compose(content, today)
	if domain.SameView(previous, view) {
		// 状态没有变化（例如同一代码另有有效记录）：只推进下一次检查日期，不形成新版本。
		return st, nil, commit(ctx, tx, rc, st, st, content, today)
	}
	next := state{Context: st.Context + 1, Profile: st.Profile}
	if err := commit(ctx, tx, rc, st, next, content, today); err != nil {
		return st, nil, err
	}
	warn, err := writeSnapshot(ctx, tx, rc.Tenant, next, content, today)
	if err != nil {
		return st, nil, err
	}
	return next, warn, emit(ctx, tx, ProfileUpdated, "profile", map[string]any{"context_version": next.Context, "profile_version": next.Profile,
		"cause": "qualification_validity", "changed": []string{"qualifications"}},
		"customer_context.context.refresh", fmt.Sprintf("context/v%d", next.Context), "qualification_validity")
}

func readView(ctx context.Context, tx pgx.Tx, tenant, unit string, version int64) (domain.View, error) {
	var raw []byte
	var view domain.View
	if err := tx.QueryRow(ctx, `SELECT policy_view FROM ctx_snapshots WHERE tenant_id = $1 AND org_unit_id = $2 AND context_version = $3`,
		tenant, unit, version).Scan(&raw); err != nil {
		return view, err
	}
	return view, json.Unmarshal(raw, &view)
}

// SnapshotResult 是一个快照。
type SnapshotResult struct {
	Tenant, Unit string
	Version      int64
	Chain        []string
	View         domain.View
}

// ContextSnapshot 返回（租户、组织单元、上下文版本）的快照；version 为 0 时取当前版本（必要时先按日期刷新）。
// 负版本是无效请求，与 HTTP 入口按契约（context_version minimum 1）在授权前拒绝一致。
func (s *Service) ContextSnapshot(ctx context.Context, unit string, version int64) (*SnapshotResult, error) {
	if version < 0 {
		return nil, failure(400, "invalid_request")
	}
	rc, err := s.requireReader(ctx)
	if err != nil {
		return nil, err
	}
	if unit != api.RootUnit {
		return nil, failure(404, "org_unit_not_found")
	}
	out := &SnapshotResult{Tenant: rc.Tenant, Unit: unit}
	var warn *overLimit
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var st state
		var err error
		if version == 0 {
			st, warn, err = s.current(ctx, tx, rc)
		} else {
			st, err = readState(ctx, tx, rc.Tenant, false)
		}
		if err != nil {
			return err
		}
		if st.Context == 0 {
			return failure(404, "context_not_initialized")
		}
		out.Version = version
		if version == 0 {
			out.Version = st.Context
		}
		if out.Version > st.Context {
			return failure(404, "context_version_not_found")
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT unit_chain, policy_view FROM ctx_snapshots WHERE tenant_id = $1 AND org_unit_id = $2 AND context_version = $3`,
			rc.Tenant, unit, out.Version).Scan(&out.Chain, &raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &out.View)
	})
	if err != nil {
		return nil, err
	}
	s.report(ctx, warn)
	return out, nil
}
