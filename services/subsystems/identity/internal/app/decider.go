package app

import (
	"context"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// Decider 是授权决策用例：主体缺省为请求上下文中的调用主体，再交给关系存储判断。
// 本地绑定与内部接口都经过它，保证两种绑定语义一致。
type Decider struct {
	Store authz.Decider
}

func (d Decider) Check(ctx context.Context, req authz.Request) (bool, error) {
	if err := req.Validate(); err != nil {
		return false, err
	}
	if req.Subject == "" {
		rc, ok := reqctx.From(ctx)
		if !ok || rc.Principal.ID == "" {
			return false, reqctx.ErrMissing
		}
		req.Subject = authz.Subject(rc.Principal)
	}
	return d.Store.Check(ctx, req)
}
