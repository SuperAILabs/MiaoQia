// Package app 是 customer-context 的用例编排：授权、事务、幂等、发件箱与审计。
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/domain"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// Service 是 customer-context 的用例。Authz 是 identity 的授权决策点，Directory 是 identity 的组织单元读取与
// 参与关系同步（均经绑定）。
type Service struct {
	DB        *db.DB
	Authz     authz.Decider
	Directory identity.Directory
	Catalog   *domain.Catalog
	Now       func() time.Time // 缺省 time.Now；测试注入以验证有效期跨日
	Log       *slog.Logger     // 运行告警（快照超出规模上界）；缺省 slog.Default()
}

func failure(status int, code string) error { return &api.Error{Status: status, Code: code} }

func failureDetail(status int, code, detail string) error {
	return &api.Error{Status: status, Code: code, Detail: detail}
}

func (s *Service) today() domain.Date {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return domain.Today(now())
}

// tx 在租户事务中执行：kernel/db 按请求上下文设置 app.tenant_id，行级安全据此过滤。基础设施错误不外泄。
func (s *Service) tx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	if s == nil || s.DB == nil {
		return failure(503, "customer_context_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := s.DB.Tx(ctx, func(tx pgx.Tx) error { return fn(ctx, tx) })
	var safe *api.Error
	var bad *domain.Invalid
	switch {
	case err == nil, errors.As(err, &safe):
		return err
	case errors.As(err, &bad):
		return failureDetail(422, bad.Code, bad.Detail)
	}
	return failure(503, "customer_context_unavailable")
}

// tenantContext 要求请求上下文带租户（档案、目标与快照均为租户私有数据）。
func tenantContext(ctx context.Context) (reqctx.Context, error) {
	rc, ok := reqctx.From(ctx)
	if !ok {
		return rc, reqctx.ErrMissing
	}
	if rc.Validate() != nil || rc.Tenant == "" {
		return rc, failure(403, "invalid_tenant_context")
	}
	return rc, nil
}

// allowed 经 identity 判断调用主体对本租户是否具有关系（资源为 tenant 的动作，identity.md 7.2）。
// 决策失败按失败关闭处理。
func (s *Service) allowed(ctx context.Context, rc reqctx.Context, relation string) (bool, error) {
	return s.allowedOn(ctx, rc, relation, "tenant:"+rc.Tenant)
}

// allowedOn 经 identity 判断调用主体对资源（tenant、org_unit 或 objective）是否具有关系。
func (s *Service) allowedOn(ctx context.Context, rc reqctx.Context, relation, object string) (bool, error) {
	if s.Authz == nil {
		return false, failure(503, "identity_unavailable")
	}
	ok, err := s.Authz.Check(ctx, authz.Request{Subject: authz.Subject(rc.Principal), Relation: relation, Object: object})
	if err != nil {
		return false, failure(503, "identity_unavailable")
	}
	return ok, nil
}

// require 要求用户主体具有关系之一。
func (s *Service) require(ctx context.Context, relations ...string) (reqctx.Context, error) {
	rc, err := tenantContext(ctx)
	if err != nil {
		return rc, err
	}
	if rc.Principal.Kind != "user" {
		return rc, failure(403, "forbidden")
	}
	for _, relation := range relations {
		ok, err := s.allowed(ctx, rc, relation)
		if err != nil || ok {
			return rc, err
		}
	}
	return rc, failure(403, "forbidden")
}

// 目标的授权（identity.md 7.2，model.fga 的 objective 与 org_unit 类型）由 identity 在目标资源与目标单元上判断：
// 启用为目标单元的 can_create_objective；查看、调整与参与人管理为目标的 can_view / can_edit / can_manage_participants
// （含仅凭参与关系的查看，参与关系经 SyncResourceParticipants 同步）；列表按单元的 can_list_objectives 过滤。
//
// 快照与上下文版本的服务读取方（customer-context.md 第6节）经 identity 判断 tenant 上的 can_read_context，
// 读取方清单由 identity 的服务授权维护，本子系统不保存名单。
func (s *Service) requireReader(ctx context.Context) (reqctx.Context, error) {
	rc, err := tenantContext(ctx)
	if err != nil {
		return rc, err
	}
	if rc.Principal.Kind == "service" {
		ok, err := s.allowed(ctx, rc, "can_read_context")
		if err != nil {
			return rc, err
		}
		if !ok {
			return rc, failure(403, "forbidden")
		}
		return rc, nil
	}
	return s.require(ctx, "can_view_profile")
}

// unitChain 经 identity 核对单元属于本租户并返回单元链（从单元到根单元）。
func (s *Service) unitChain(ctx context.Context, unit string) ([]string, error) {
	if s.Directory == nil {
		return nil, failure(503, "identity_unavailable")
	}
	u, err := s.Directory.OrgUnit(ctx, unit)
	if err != nil {
		var safe *identity.Error
		if errors.As(err, &safe) && safe.Status < 500 {
			if safe.Code == "org_unit_not_found" || safe.Code == "invalid_request" {
				return nil, failure(404, "org_unit_not_found")
			}
			return nil, failure(safe.Status, safe.Code)
		}
		return nil, failure(503, "identity_unavailable")
	}
	chain := make([]string, 0, len(u.Chain))
	for _, c := range u.Chain {
		chain = append(chain, string(c))
	}
	return chain, nil
}

var commandKey = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// command 在业务写入、发件箱与审计的同一事务中登记命令结果：同一主体、租户与键重放返回首次结果，
// 键相同而操作或请求不同返回 409 idempotency_conflict。按（租户、主体、键）加事务级咨询锁串行化同键并发。
func command(ctx context.Context, tx pgx.Tx, key, operation string, request, response any, apply func() error) error {
	if !commandKey.MatchString(key) {
		return failure(400, "invalid_idempotency_key")
	}
	rc, _ := reqctx.From(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "ctx-command/"+rc.Tenant+"/"+rc.Principal.ID+"/"+key); err != nil {
		return err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(digest[:])
	var previousOperation, previousHash string
	var previous []byte
	err = tx.QueryRow(ctx, `SELECT operation, fingerprint, response FROM ctx_commands WHERE tenant_id = $1 AND actor = $2 AND request_key = $3`,
		rc.Tenant, rc.Principal.ID, key).Scan(&previousOperation, &previousHash, &previous)
	if err == nil {
		if previousOperation != operation || previousHash != fingerprint {
			return failure(409, "idempotency_conflict")
		}
		return json.Unmarshal(previous, response)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err := apply(); err != nil {
		return err
	}
	encoded, err = json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO ctx_commands (tenant_id, actor, request_key, operation, fingerprint, response) VALUES ($1, $2, $3, $4, $5, $6)`,
		rc.Tenant, rc.Principal.ID, key, operation, fingerprint, encoded)
	return err
}

var (
	writer   = outbox.Writer{Subsystem: api.Name, Namespace: "customer_context", Zone: reqctx.ZoneTenant}
	recorder = audit.Recorder{Subsystem: api.Name}
)

// emit 在事务中追加租户域事件与审计条目。审计只记引用：档案的前后值是两个不可变的档案版本，目标的前后值见目标版本的事件链。
func emit(ctx context.Context, tx pgx.Tx, event, subject string, data any, action, resource, basis string) error {
	if _, err := writer.Append(ctx, tx, outbox.Event{Type: event, Subject: subject, PartitionKey: subject, Data: data}); err != nil {
		return err
	}
	return recorder.Record(ctx, tx, audit.Entry{Action: action, Resource: resource, Result: "succeeded", Basis: basis})
}
