package api

import (
	"context"
	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/binding"
)

const SessionBinding = "identity.SessionResolver"

type Session = identityapi.Session

// SessionResolver is the authoritative tenant/membership/cell check, shared by local and remote binding.
type SessionResolver interface {
	Resolve(context.Context) (*Session, error)
}

func LocalSession(impl SessionResolver) SessionResolver { return localSession{impl} }

type localSession struct{ impl SessionResolver }

func (s localSession) Resolve(ctx context.Context) (*Session, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ResolveSession", Idempotent: true}, s.impl.Resolve)
}

// Error is safe to expose at the transport boundary; raw infrastructure errors are not.
type Error struct {
	Status int
	Code   string
}

func (e *Error) Error() string { return e.Code }

// DirectoryBinding 是组织单元读取与资源参与关系同步在绑定表中的名称。
const DirectoryBinding = "identity.Directory"

// 组织单元与参与关系同步的契约类型（contracts/apis/identity/openapi.yaml）。
type (
	OrgUnit                 = identityapi.OrgUnit
	ParticipantRelation     = identityapi.ParticipantRelation
	SyncParticipantsRequest = identityapi.SyncParticipantsRequest
	SyncParticipantsResult  = identityapi.SyncParticipantsResult
)

// Directory 是业务子系统使用的目录接口（本地与远程绑定行为一致）：
//   - OrgUnit 返回上下文租户内的组织单元与单元链（root 或 unt_…）；不属于本租户时为 404 org_unit_not_found。只读、幂等；
//   - SyncResourceParticipants 由资源所属子系统以服务主体调用，写入资源上的参与关系（identity.md 第5节）。按参与版本幂等。
type Directory interface {
	OrgUnit(ctx context.Context, id string) (*OrgUnit, error)
	SyncResourceParticipants(ctx context.Context, req *SyncParticipantsRequest) (*SyncParticipantsResult, error)
}

func LocalDirectory(impl Directory) Directory { return localDirectory{impl} }

type localDirectory struct{ impl Directory }

func (l localDirectory) OrgUnit(ctx context.Context, id string) (*OrgUnit, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "GetOrgUnit", Idempotent: true},
		func(ctx context.Context) (*OrgUnit, error) { return l.impl.OrgUnit(ctx, id) })
}

func (l localDirectory) SyncResourceParticipants(ctx context.Context, req *SyncParticipantsRequest) (*SyncParticipantsResult, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "SyncResourceParticipants", Idempotent: true},
		func(ctx context.Context) (*SyncParticipantsResult, error) {
			return l.impl.SyncResourceParticipants(ctx, req)
		})
}

// ChannelSessionResolver 核验渠道免登会话（PRD 第1段 F6、F7）：会话未作废、未过期、绑定版本高于该绑定的撤销水位，
// 且会话的成员是令牌租户的有效成员。网关对 miaoqia_session_source=channel 的令牌每次请求调用它（取代 Resolve）。
// 会话标识来自令牌（进程内的 reqctx.Assurance），不跨进程传递，因此只由本地绑定提供；远程绑定没有它，网关失败关闭。
type ChannelSessionResolver interface {
	ResolveChannelSession(ctx context.Context, sessionID string) (*Session, error)
}

func (s localSession) ResolveChannelSession(ctx context.Context, sessionID string) (*Session, error) {
	impl, ok := s.impl.(ChannelSessionResolver)
	if !ok {
		return nil, &Error{Status: 503, Code: "identity_unavailable"}
	}
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ResolveChannelSession", Idempotent: true},
		func(ctx context.Context) (*Session, error) { return impl.ResolveChannelSession(ctx, sessionID) })
}
