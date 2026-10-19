// Package app 是 channels 的用例编排：授权、事务、幂等、发件箱与审计（PRD 第1段 F4–F7）。
package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// Channel 是首批唯一的渠道（企业微信）。飞书、钉钉随各自适配器追加。
const Channel = "wecom"

// Service 是 channels 的用例。Authz 是 identity 的授权决策点，Guard 是 identity 的强认证门禁（本地绑定；取不到时管理动作
// 失败关闭），Sessions 是 identity 的免登会话签发与撤销，Members 是 identity 的会话核验（取当前成员版本），WeCom 是企业微信渠道适配器（缺省未接入）。Now 是业务时钟。
type Service struct {
	DB       *db.DB
	Authz    authz.Decider
	Guard    identity.Guard
	Sessions identity.ChannelSessions
	Members  identity.SessionResolver
	WeCom    api.Provider
	Now      func() time.Time
	Log      *slog.Logger
}

func failure(status int, code string) error { return &api.Error{Status: status, Code: code} }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// businessNow 是写入的业务时间，取整到秒（接口的 date-time 以秒表示）。
func (s *Service) businessNow() time.Time { return s.now().UTC().Truncate(time.Second) }

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) provider() api.Provider {
	if s.WeCom == nil {
		return api.NotWired{}
	}
	return s.WeCom
}

// tx 在事务中执行：kernel/db 按请求上下文设置 app.tenant_id，行级安全据此过滤。基础设施错误不外泄。
func (s *Service) tx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	if s == nil || s.DB == nil {
		return failure(503, "channels_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := s.DB.Tx(ctx, func(tx pgx.Tx) error { return fn(ctx, tx) })
	var safe *api.Error
	if err != nil && !errors.As(err, &safe) {
		if errors.Is(err, reqctx.ErrMissing) {
			return err
		}
		s.logger().WarnContext(ctx, "channels 事务失败", "error", err.Error())
		return failure(503, "channels_unavailable")
	}
	return err
}

// serviceContext 是以 channels 服务主体在给定租户内执行的请求上下文（调用 identity、回调与事件处理）。
func serviceContext(ctx context.Context, tenant, cell string) context.Context {
	rc, _ := reqctx.From(ctx)
	return reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: cell, Principal: reqctx.Principal{ID: "svc:" + api.Name, Kind: "service"}, Purpose: rc.Purpose})
}

// member 要求调用者是带租户的用户，且是该租户的有效成员（identity Check can_view_profile：对全体有效成员开放）。
func (s *Service) member(ctx context.Context) (reqctx.Context, error) {
	rc, ok := reqctx.From(ctx)
	if !ok {
		return rc, reqctx.ErrMissing
	}
	if rc.Validate() != nil || rc.Tenant == "" || rc.Principal.Kind != "user" {
		return rc, failure(403, "invalid_tenant_context")
	}
	ok, err := s.allowed(ctx, rc, "can_view_profile", "tenant:"+rc.Tenant)
	if err != nil {
		return rc, err
	}
	if !ok {
		return rc, failure(403, "forbidden")
	}
	return rc, nil
}

func (s *Service) allowed(ctx context.Context, rc reqctx.Context, relation, object string) (bool, error) {
	if s.Authz == nil {
		return false, failure(503, "identity_unavailable")
	}
	ok, err := s.Authz.Check(ctx, authz.Request{Subject: authz.Subject(rc.Principal), Relation: relation, Object: object})
	if err != nil {
		return false, failure(503, "identity_unavailable")
	}
	return ok, nil
}

// requireAdmin 经 identity.Guard 要求管理员 + 15分钟内的强认证（渠道安装与停用、强制解绑）。门禁的稳定错误原样返回。
func (s *Service) requireAdmin(ctx context.Context) error {
	if s.Guard == nil {
		return failure(503, "identity_unavailable")
	}
	return identityError(s.Guard.RequireAdmin(ctx))
}

// identityError 把 identity 的错误转换为本子系统的错误：稳定的 4xx 原样带出，其余为 503 identity_unavailable（失败关闭）。
func identityError(err error) error {
	if err == nil {
		return nil
	}
	var safe *identity.Error
	if errors.As(err, &safe) && safe.Status < 500 {
		return failure(safe.Status, safe.Code)
	}
	var own *api.Error
	if errors.As(err, &own) {
		return own
	}
	return failure(503, "identity_unavailable")
}

var commandKey = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// command 在业务写入、发件箱与审计的同一事务中登记命令结果：同一主体、租户与键重放返回首次结果（fresh 为假），
// 键相同而操作或请求不同返回 409 idempotency_conflict。按（租户、主体、键）加事务级咨询锁串行化同键并发。
func command(ctx context.Context, tx pgx.Tx, key, operation string, request, response any, apply func() error) (fresh bool, err error) {
	if !commandKey.MatchString(key) {
		return false, failure(400, "invalid_idempotency_key")
	}
	rc, _ := reqctx.From(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "chn-command/"+rc.Tenant+"/"+rc.Principal.ID+"/"+key); err != nil {
		return false, err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(digest[:])
	var previousOperation, previousHash string
	var previous []byte
	err = tx.QueryRow(ctx, `SELECT operation, fingerprint, response FROM chn_commands WHERE tenant_id = $1 AND actor = $2 AND request_key = $3`,
		rc.Tenant, rc.Principal.ID, key).Scan(&previousOperation, &previousHash, &previous)
	if err == nil {
		if previousOperation != operation || previousHash != fingerprint {
			return false, failure(409, "idempotency_conflict")
		}
		return false, json.Unmarshal(previous, response)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if err := apply(); err != nil {
		return false, err
	}
	encoded, err = json.Marshal(response)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO chn_commands (tenant_id, actor, request_key, operation, fingerprint, response) VALUES ($1, $2, $3, $4, $5, $6)`,
		rc.Tenant, rc.Principal.ID, key, operation, fingerprint, encoded)
	return true, err
}

var (
	writer   = outbox.Writer{Subsystem: api.Name, Namespace: "channels", Zone: reqctx.ZoneTenant}
	recorder = audit.Recorder{Subsystem: api.Name}
)

// record 在事务中写审计条目。
func record(ctx context.Context, tx pgx.Tx, action, resource, basis string) error {
	return recorder.Record(ctx, tx, audit.Entry{Action: action, Resource: resource, Result: "succeeded", Basis: basis})
}

// BindingChanged 是绑定变化事件的类型名。
const BindingChanged = "channels.binding.changed.v1"

// emitBinding 在事务中发布 channels.binding.changed.v1 并写审计。
func emitBinding(ctx context.Context, tx pgx.Tx, b *binding, basis string) error {
	data := map[string]any{"binding_id": b.id, "channel": b.channel, "subject": b.subject, "version": b.version, "state": b.state}
	if b.reason != "" {
		data["reason"] = b.reason
	}
	if _, err := writer.Append(ctx, tx, outbox.Event{Type: BindingChanged, Subject: b.id, PartitionKey: b.id, Data: data}); err != nil {
		return err
	}
	return record(ctx, tx, "channels.binding."+b.state, b.id, basis)
}

// newToken 生成一次性令牌（32字节随机数的 base64url，43位）与其 SHA-256 摘要；只保存摘要。
func newToken() (token, digest string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, tokenDigest(token), nil
}

func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func uniqueViolation(err error) (string, bool) {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return pg.ConstraintName, true
	}
	return "", false
}
