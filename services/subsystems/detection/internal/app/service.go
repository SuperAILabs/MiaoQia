// Package app 是 detection 的用例编排：生效边界上的匹配、信号收件箱、处理与反馈、批量与撤销。
package app

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	customercontext "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// 事件类型。
const (
	SignalDetected = "detection.signal.detected.v1"
	SignalTriaged  = "detection.signal.triaged.v1"
)

// 授权关系（customer-context 目标的关系，经 ObjectiveAccess 按目标所有者的当前口径判断）。
const (
	relViewSignals = "can_view_signals"
	relFeedback    = "can_feedback"
)

// BatchHook 在批量作业执行每一项之前调用（测试用屏障与故障注入）：返回错误时执行者立即停止且不释放租约，模拟进程中断。
type BatchHook func(ctx context.Context, batchID string, position int) error

// Service 是 detection 的用例。Access、Context 与 Policies 是 customer-context 的目标访问判断、上下文读取与生效策略读取（经绑定）；
// Facts 与 Candidates 是匹配端口（api.NotWired 为未接入）；Packs 是应用领域包内容；Cell 是本进程所在 cell。
type Service struct {
	DB         *db.DB
	Access     customercontext.ObjectiveAccess
	Context    customercontext.ContextReader
	Policies   customercontext.ActivePolicies
	Facts      api.Facts
	Candidates api.Candidates
	Packs      *Packs
	Cell       string
	Now        func() time.Time
	Log        *slog.Logger
	LeaseTTL   time.Duration
	Hook       BatchHook

	compiled  sync.Map // 编译后的策略：键为（目标、版本、文档摘要），内容不可变
	snapshots sync.Map // 上下文快照：键为（租户、组织单元、上下文版本），内容不可变
}

var (
	writer   = outbox.Writer{Subsystem: api.Name, Namespace: "detection", Zone: reqctx.ZoneTenant}
	recorder = audit.Recorder{Subsystem: api.Name}
)

func failure(status int, code string) error { return &api.Error{Status: status, Code: code} }

func (s *Service) now() time.Time {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return now().UTC()
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) leaseTTL() time.Duration {
	if s.LeaseTTL > 0 {
		return s.LeaseTTL
	}
	return 2 * time.Minute
}

// tx 在租户事务中执行；基础设施错误不外泄（503 detection_unavailable）。
func (s *Service) tx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	if s == nil || s.DB == nil {
		return failure(503, "detection_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := s.DB.Tx(ctx, func(tx pgx.Tx) error { return fn(ctx, tx) })
	var safe *api.Error
	if err == nil || errors.As(err, &safe) {
		return err
	}
	s.logger().WarnContext(ctx, "detection 数据库事务失败", "error", err)
	return failure(503, "detection_unavailable")
}

// userContext 返回用户主体的租户请求上下文；服务主体与缺少租户的上下文被拒绝。
func userContext(ctx context.Context) (reqctx.Context, error) {
	rc, ok := reqctx.From(ctx)
	if !ok {
		return rc, reqctx.ErrMissing
	}
	if rc.Validate() != nil || rc.Tenant == "" {
		return rc, failure(403, "invalid_tenant_context")
	}
	if rc.Principal.Kind != "user" {
		return rc, failure(403, "forbidden")
	}
	return rc, nil
}

// access 经 customer-context（目标的所有者）按其当前口径判断用户主体对目标的关系：移除参与人在 customer-context 提交后立即撤权，
// 决策者与研究分析者经单元关系，不需要是参与人。返回被允许的关系；不能查看与不存在时 visible 为假。判断失败按失败关闭处理
// （503，不当作无权限）。
func (s *Service) access(ctx context.Context, objective string, relations ...string) (visible bool, granted []string, err error) {
	if s.Access == nil {
		return false, nil, failure(503, "customer_context_unavailable")
	}
	granted, err = s.Access.Access(ctx, objective, relations)
	if err != nil {
		var safe *customercontext.Error
		switch {
		case errors.As(err, &safe) && safe.Status == 404:
			return false, nil, nil
		case errors.As(err, &safe) && safe.Status == 503 && safe.Code != "":
			return false, nil, failure(503, safe.Code)
		case errors.As(err, &safe) && safe.Status < 500:
			return false, nil, failure(safe.Status, safe.Code)
		}
		return false, nil, failure(503, "customer_context_unavailable")
	}
	return true, granted, nil
}

func has(granted []string, relation string) bool { return slices.Contains(granted, relation) }

func transient(err error) bool {
	var safe *api.Error
	return !errors.As(err, &safe) || safe.Status >= 500
}
