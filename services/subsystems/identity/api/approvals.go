package api

import (
	"context"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/binding"
)

// ApprovalsBinding 是审批请求的提交与失效在绑定表中的名称。
const ApprovalsBinding = "identity.Approvals"

// 审批的契约类型（contracts/apis/identity/openapi.yaml 1.5.0）。
type (
	ApprovalRequest        = identityapi.ApprovalRequest
	ApprovalBinding        = identityapi.ApprovalBinding
	RequestApprovalRequest = identityapi.RequestApprovalRequest
	CloseApprovalRequest   = identityapi.CloseApprovalRequest
)

// Approvals 是审批类型的所属子系统使用的审批接口（identity.md 第5节“审批”）：
//   - RequestApproval 以请求上下文的用户为申请人提交审批请求：申请关系、15分钟内的强认证、同一资源无进行中的请求；
//     按 (申请人, 租户, key) 幂等。认证保证只在网关进程内传递，需要强认证的类型只能经本地绑定提交（远程绑定一律 403 mfa_required）；
//   - CloseApproval 由所属子系统的服务主体调用，使绑定已不成立的待审批请求失效；已决定或已关闭的请求原样返回，
//     所属子系统据返回的状态对账（例如请求其实已批准）。
type Approvals interface {
	RequestApproval(ctx context.Context, req *RequestApprovalRequest, key string) (*ApprovalRequest, error)
	CloseApproval(ctx context.Context, id string, req *CloseApprovalRequest) (*ApprovalRequest, error)
}

func LocalApprovals(impl Approvals) Approvals { return localApprovals{impl} }

type localApprovals struct{ impl Approvals }

func (l localApprovals) RequestApproval(ctx context.Context, req *RequestApprovalRequest, key string) (*ApprovalRequest, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "RequestApproval", Idempotent: true},
		func(ctx context.Context) (*ApprovalRequest, error) { return l.impl.RequestApproval(ctx, req, key) })
}

func (l localApprovals) CloseApproval(ctx context.Context, id string, req *CloseApprovalRequest) (*ApprovalRequest, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "CloseApproval", Idempotent: true},
		func(ctx context.Context) (*ApprovalRequest, error) { return l.impl.CloseApproval(ctx, id, req) })
}

// ApprovalReaderBinding 是审批请求读取接口在绑定表中的名称（channels 简报的“待我处理”区与审批链接，PRD 第5段 F1 第8条、F5）。
const ApprovalReaderBinding = "identity.ApprovalReader"

// 审批读取的契约类型。
type (
	ApprovalList        = identityapi.ApprovalList
	ListApprovalsParams = identityapi.ListApprovalsParams
)

// ApprovalReader 以请求上下文中的用户主体读取审批请求，可见性与公开接口 listApprovals、getApproval 相同：申请人本人，或此刻对
// 请求资源有批准关系的成员（逐个资源实时判断）；不可见与不存在同为 404。只读、幂等。
type ApprovalReader interface {
	ListApprovals(ctx context.Context, params ListApprovalsParams) (*ApprovalList, error)
	GetApproval(ctx context.Context, id string) (*ApprovalRequest, error)
}

// LocalApprovalReader 把本地实现包装为经调用管线的接口（与远程绑定行为一致）。
func LocalApprovalReader(impl ApprovalReader) ApprovalReader { return localApprovalReader{impl} }

type localApprovalReader struct{ impl ApprovalReader }

func (l localApprovalReader) ListApprovals(ctx context.Context, params ListApprovalsParams) (*ApprovalList, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ListApprovals", Idempotent: true},
		func(ctx context.Context) (*ApprovalList, error) { return l.impl.ListApprovals(ctx, params) })
}

func (l localApprovalReader) GetApproval(ctx context.Context, id string) (*ApprovalRequest, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "GetApproval", Idempotent: true},
		func(ctx context.Context) (*ApprovalRequest, error) { return l.impl.GetApproval(ctx, id) })
}
