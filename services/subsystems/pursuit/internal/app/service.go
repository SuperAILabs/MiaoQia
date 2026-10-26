// Package app 是 pursuit 的用例编排：交易型跟进的创建与关联、改派、任务与提醒、关闭与结果、授权同步（PRD 第6段；pursuit.md）。
package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	customercontext "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	detection "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// 事件类型（contracts/events/pursuit/events.asyncapi.yaml）。
const (
	PursuitCreated  = "pursuit.pursuit.created.v1"
	SignalLinked    = "pursuit.pursuit.signal_linked.v1"
	SignalUnlinked  = "pursuit.pursuit.signal_unlinked.v1"
	PursuitVoided   = "pursuit.pursuit.voided.v1"
	TeamChanged     = "pursuit.team.changed.v1"
	PursuitAssigned = "pursuit.pursuit.assigned.v1"
	TaskDue         = "pursuit.task.due.v1"
	OutcomeRecorded = "pursuit.outcome.recorded.v1"
	PursuitClosed   = "pursuit.pursuit.closed.v1"
)

// 授权关系。单元关系由 identity 在 org_unit 上判断（model.fga 的 can_view_unit_pursuits / can_edit_unit_pursuits）；跟进上的
// can_view / can_edit 只对 pursuit 本地期望关系中的负责人与协作者再经 identity 判断（参与关系经授权同步写入）。
const (
	relUnitView   = "can_view_unit_pursuits"
	relUnitEdit   = "can_edit_unit_pursuits"
	relView       = "can_view"
	relEdit       = "can_edit"
	relAssign     = "can_assign"       // customer-context 目标关系（org_unit#can_assign_unit_objectives）
	relViewSignal = "can_view_signals" // customer-context 目标关系：被分派人须能查看该目标的信号
)

// 时间常量（PRD 第6段第13节第3、11项，F12 第2、6条）。
const (
	VoidWindow     = 10 * time.Minute
	ReopenWeb      = 10 * time.Second
	ReopenChannel  = 10 * time.Minute
	SyncFailedWait = 24 * time.Hour
)

// MinSigningKey 是游标签名密钥的最短长度（字节）。
const MinSigningKey = 32

// Service 是 pursuit 的用例。Authz 是 identity 的授权决策点，Directory 是参与关系同步，Members 是 identity 的会话核验
// （以被分派人本人判断其成员状态），Scope 是 customer-context 的目标访问与目标所在单元，Signals 是 detection 的信号卡片
// （从信号创建时核对信号；未加载 detection 的角色为空，从信号创建失败关闭为 503）。Key 是游标签名密钥，Outcomes 是应用领域包的
// 结果定义。
type Service struct {
	DB        *db.DB
	Authz     authz.Decider
	Directory identity.Directory
	Members   identity.SessionResolver
	Scope     customercontext.ObjectiveScope
	Signals   detection.Briefing
	Key       []byte
	Outcomes  map[string]*OutcomeDef
	Now       func() time.Time
	Log       *slog.Logger
}

var (
	writer   = outbox.Writer{Subsystem: api.Name, Namespace: "pursuit", Zone: reqctx.ZoneTenant}
	recorder = audit.Recorder{Subsystem: api.Name}
)

func failure(status int, code string) *api.Error { return &api.Error{Status: status, Code: code} }

func unavailable() *api.Error { return failure(503, "pursuit_unavailable") }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// businessNow 是写入的业务时间，取整到毫秒（与数据库精度一致）。
func (s *Service) businessNow() time.Time { return s.now().Truncate(time.Millisecond) }

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// tx 在请求上下文的租户事务中执行；基础设施错误不外泄（503 pursuit_unavailable）。
func (s *Service) tx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	if s == nil || s.DB == nil {
		return unavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := s.DB.Tx(ctx, func(tx pgx.Tx) error { return fn(ctx, tx) })
	var safe *api.Error
	if err == nil || errors.As(err, &safe) || errors.Is(err, reqctx.ErrMissing) {
		return err
	}
	s.logger().WarnContext(ctx, "pursuit 数据库事务失败", "error", err.Error())
	return unavailable()
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

// serviceContext 要求带租户的服务主体（事件消费、定时任务）。
func serviceContext(ctx context.Context) (reqctx.Context, error) {
	rc, ok := reqctx.From(ctx)
	if !ok {
		return rc, reqctx.ErrMissing
	}
	if rc.Validate() != nil || rc.Tenant == "" || rc.Principal.Kind != "service" {
		return rc, failure(403, "forbidden")
	}
	return rc, nil
}

// asService 是本子系统服务主体在给定租户内的请求上下文（授权同步、提醒与成员变化处理）。
func asService(ctx context.Context, tenant, cell string) context.Context {
	rc, _ := reqctx.From(ctx)
	return reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: cell, Principal: reqctx.Principal{ID: "svc:" + api.Name, Kind: "service"}, Purpose: rc.Purpose})
}

// asMember 是以成员本人（用户主体）在给定租户内判断的请求上下文：被分派人的成员状态与目标访问、提醒收件人的更新权限
// 按其本人的当前权限判断，不以操作者的权限代替。
func asMember(ctx context.Context, tenant, cell, subject string) context.Context {
	rc, _ := reqctx.From(ctx)
	return reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: cell, Principal: reqctx.Principal{ID: subject, Kind: "user"}, Purpose: rc.Purpose})
}

type internalKey struct{}

// Internal 标记来自内部端口（mTLS）或本地绑定的调用：channel 为 wecom 的请求只在这样的调用中接受。
func Internal(ctx context.Context) context.Context {
	return context.WithValue(ctx, internalKey{}, true)
}

func isInternal(ctx context.Context) bool { v, _ := ctx.Value(internalKey{}).(bool); return v }

// check 以给定上下文的主体经 identity 判断一条关系；判断失败为 503（失败关闭，不当作无权限）。
func (s *Service) check(ctx context.Context, rc reqctx.Context, relation, object string) (bool, error) {
	if s.Authz == nil {
		return false, failure(503, "identity_unavailable")
	}
	ok, err := s.Authz.Check(ctx, authz.Request{Subject: authz.Subject(rc.Principal), Relation: relation, Object: object})
	if err != nil {
		return false, failure(503, "identity_unavailable")
	}
	return ok, nil
}

// scopeOf 经 customer-context 按其当前口径判断调用者在目标上的关系；不可见为 404 objective_not_found，判断失败为 503。
func (s *Service) scopeOf(ctx context.Context, objective string, relations ...string) (*customercontext.Scope, error) {
	if s.Scope == nil {
		return nil, failure(503, "customer_context_unavailable")
	}
	sc, err := s.Scope.Scope(ctx, objective, relations)
	if err != nil {
		var safe *customercontext.Error
		switch {
		case errors.As(err, &safe) && safe.Status == 404:
			return nil, failure(404, "objective_not_found")
		case errors.As(err, &safe) && safe.Status == 403 && safe.Code == "invalid_tenant_context":
			return nil, failure(403, "invalid_tenant_context")
		case errors.As(err, &safe) && safe.Status < 500 && safe.Status != 403:
			return nil, failure(safe.Status, safe.Code)
		}
		return nil, failure(503, "customer_context_unavailable")
	}
	return sc, nil
}

// memberAccess 判断成员（被分派人、负责人、协作者）此刻是否仍可承担跟进：是租户有效成员（identity 以其本人核验会话：停用或退出为
// 403），且能查看该目标的信号（customer-context 以其本人判断）。明确否定返回 false；判断失败返回 503，不当作无权限也不当作有权限。
func (s *Service) memberAccess(ctx context.Context, tenant, cell, subject, objective string) (bool, error) {
	if s.Members == nil {
		return false, failure(503, "identity_unavailable")
	}
	mctx := asMember(ctx, tenant, cell, subject)
	if _, err := s.Members.Resolve(mctx); err != nil {
		var safe *identity.Error
		if errors.As(err, &safe) && safe.Status == 403 {
			return false, nil
		}
		return false, failure(503, "identity_unavailable")
	}
	sc, err := s.scopeOf(mctx, objective, relViewSignal)
	if err != nil {
		var safe *api.Error
		if errors.As(err, &safe) && (safe.Status == 404 || safe.Status == 403) {
			return false, nil
		}
		return false, err
	}
	return sc.Has(relViewSignal), nil
}

// Rights 是调用者对一个跟进的当前权限（每次读取与命令都实时判断，不以创建或上次读取时为准）。
type Rights struct {
	View, Edit, UnitEdit bool
	Involved             bool
}

// unitRights 缓存同一请求内同一单元的单元关系。
type unitRights map[string][2]bool

// rights 判断调用者对跟进的权限（info.description 的授权口径）：单元关系，或 pursuit 本地期望关系中的负责人（且负责人有效）/协作者
// 且 identity 在跟进资源上判断通过。改派后原负责人立即不再是本地负责人；新负责人在授权同步完成前 identity 判断不通过（失败关闭）。
func (s *Service) rights(ctx context.Context, rc reqctx.Context, p *Pursuit, cache unitRights) (Rights, error) {
	var r Rights
	unit, ok := cache[p.OrgUnitID]
	if !ok {
		view, err := s.check(ctx, rc, relUnitView, "org_unit:"+p.OrgUnitID)
		if err != nil {
			return r, err
		}
		edit, err := s.check(ctx, rc, relUnitEdit, "org_unit:"+p.OrgUnitID)
		if err != nil {
			return r, err
		}
		unit = [2]bool{view, edit}
		if cache != nil {
			cache[p.OrgUnitID] = unit
		}
	}
	r.View, r.Edit, r.UnitEdit = unit[0], unit[1], unit[1]
	r.Involved = p.involves(rc.Principal.ID)
	if r.Involved && (!r.View || !r.Edit) {
		if !r.View {
			ok, err := s.check(ctx, rc, relView, "pursuit:"+p.ID)
			if err != nil {
				return r, err
			}
			r.View = ok
		}
		if !r.Edit {
			ok, err := s.check(ctx, rc, relEdit, "pursuit:"+p.ID)
			if err != nil {
				return r, err
			}
			r.Edit = ok && r.View
		}
	}
	return r, nil
}

func fingerprint(parts ...any) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// 签名用途（域分隔）。
const (
	purposeListCursor  = "pursuit/list-cursor/v1"
	purposeTasksCursor = "pursuit/my-tasks-cursor/v1"
)

func (s *Service) mac(purpose string, payload []byte) []byte {
	h := hmac.New(sha256.New, s.Key)
	h.Write([]byte(purpose))
	h.Write([]byte{0})
	h.Write(payload)
	return h.Sum(nil)
}

// seal 签发客户端持有的翻页位置：整个载荷连同 HMAC-SHA256 签名一起编码；密钥不可用时 503（失败关闭）。
func (s *Service) seal(purpose string, v any) (string, error) {
	if len(s.Key) < MinSigningKey {
		return "", failure(503, "signing_key_unavailable")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(append(raw, s.mac(purpose, raw)...)), nil
}

// unseal 先校验签名再解析；签名不符、格式错误时返回 false（在任何读取之前判断）。
func (s *Service) unseal(purpose, value string, v any) bool {
	if len(s.Key) < MinSigningKey || len(value) > 1024 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) <= sha256.Size {
		return false
	}
	payload, sum := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	if !hmac.Equal(sum, s.mac(purpose, payload)) {
		return false
	}
	return json.Unmarshal(payload, v) == nil
}

func dedupe(list []string, without string) []string {
	out := []string{}
	for _, v := range list {
		if v != without && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}
