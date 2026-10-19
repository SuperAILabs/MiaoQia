package api

import (
	"context"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/binding"
)

// ChannelSessionsBinding 是渠道免登会话签发与撤销在绑定表中的名称。
const ChannelSessionsBinding = "identity.ChannelSessions"

// 免登会话的契约类型（contracts/apis/identity/openapi.yaml 1.4.0）。
type (
	IssueChannelSessionRequest   = identityapi.IssueChannelSessionRequest
	ChannelSession               = identityapi.ChannelSession
	RevokeChannelSessionsRequest = identityapi.RevokeChannelSessionsRequest
	ChannelSessionRevocation     = identityapi.ChannelSessionRevocation
	LiveChannelBindingList       = identityapi.LiveChannelBindingList
)

// ChannelSessions 是只供 channels 服务主体调用的免登会话接口（本地与远程绑定行为一致，identity.md 第5节）：
//   - IssueChannelSession 凭 channels 的签发凭据签发不带强认证的免登会话：锁定该绑定的撤销水位行，核验凭据未过期
//     （5秒偏差从严）、绑定版本高于水位、成员有效且绑定建立后没有退出过（MemberVersion 高于最近一次退出时的成员版本）后写入会话；同一凭据标识的重试返回首次结果；
//   - RevokeChannelSessions 在同一水位行上把水位抬高到给定版本并作废版本不高于它的会话；返回即已生效。按版本幂等；
//   - ListLiveChannelSessions 分页列出仍有有效会话的绑定，供 channels 对账（cursor 为空表示第一页）。
type ChannelSessions interface {
	IssueChannelSession(ctx context.Context, req *IssueChannelSessionRequest) (*ChannelSession, error)
	RevokeChannelSessions(ctx context.Context, req *RevokeChannelSessionsRequest) (*ChannelSessionRevocation, error)
	ListLiveChannelSessions(ctx context.Context, cursor string, limit int) (*LiveChannelBindingList, error)
}

func LocalChannelSessions(impl ChannelSessions) ChannelSessions { return localChannelSessions{impl} }

type localChannelSessions struct{ impl ChannelSessions }

func (l localChannelSessions) IssueChannelSession(ctx context.Context, req *IssueChannelSessionRequest) (*ChannelSession, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "IssueChannelSession", Idempotent: true},
		func(ctx context.Context) (*ChannelSession, error) { return l.impl.IssueChannelSession(ctx, req) })
}

func (l localChannelSessions) RevokeChannelSessions(ctx context.Context, req *RevokeChannelSessionsRequest) (*ChannelSessionRevocation, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "RevokeChannelSessions", Idempotent: true},
		func(ctx context.Context) (*ChannelSessionRevocation, error) {
			return l.impl.RevokeChannelSessions(ctx, req)
		})
}

func (l localChannelSessions) ListLiveChannelSessions(ctx context.Context, cursor string, limit int) (*LiveChannelBindingList, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ListLiveChannelSessions", Idempotent: true},
		func(ctx context.Context) (*LiveChannelBindingList, error) {
			return l.impl.ListLiveChannelSessions(ctx, cursor, limit)
		})
}
