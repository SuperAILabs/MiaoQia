package infra

import (
	"context"
	"slices"
)

// ServiceGrants 是服务主体的授权清单：哪些子系统可以在带有效租户上下文时执行 tenant 上的服务动作（model.fga 的 platform 服务关系）。
// 清单随代码评审变更，启动时与 OpenFGA 对齐：缺少的写入，清单外的同类关系删除，因此 OpenFGA 中的服务授权只来自本清单。
//   - context_reader：读取客户上下文版本与快照（customer-context 第6节的读取方：detection 求值、learning 仿真、research 与
//     assessment 固定引用）；
//   - <资源>_registrar：同步该类资源的参与关系（SyncResourceParticipants），只授予资源的所属子系统（contracts/identifiers.yaml 的 owner）；
//   - channel_session_issuer：签发、撤销与对账渠道免登会话（IssueChannelSession / RevokeChannelSessions），只授予渠道绑定的权威 channels。
var ServiceGrants = map[string][]string{
	"context_reader":         {"assessment", "detection", "learning", "research"},
	"objective_registrar":    {"customer-context"},
	"pursuit_registrar":      {"pursuit"},
	"research_registrar":     {"research"},
	"channel_session_issuer": {"channels"},
}

// PlatformObject 是平台对象，服务授权挂在它上面。
const PlatformObject = "platform:miaoqia"

func (o *OpenFGA) reconcileServiceGrants(ctx context.Context) error {
	var adds, deletes [][3]string
	for relation, services := range ServiceGrants {
		r := relation
		object := PlatformObject
		current, err := o.read(ctx, clientRead(&r, &object))
		if err != nil {
			return err
		}
		want := map[string]bool{}
		for _, s := range services {
			want["service:"+s] = true
		}
		have := map[string]bool{}
		for _, t := range current {
			have[t[0]] = true
			if !want[t[0]] {
				deletes = append(deletes, t)
			}
		}
		for _, s := range services {
			if !have["service:"+s] {
				adds = append(adds, [3]string{"service:" + s, relation, PlatformObject})
			}
		}
	}
	slices.SortFunc(adds, compareTuple)
	slices.SortFunc(deletes, compareTuple)
	return o.Write(ctx, adds, deletes)
}

func compareTuple(a, b [3]string) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
