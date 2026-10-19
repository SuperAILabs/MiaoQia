package app

import (
	"context"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// Decider 是授权决策用例：主体缺省为请求上下文中的调用主体，再交给关系存储判断。
// 本地绑定与内部接口都经过它，保证两种绑定语义一致。
type Decider struct {
	Store     authz.Decider
	Directory *Directory
}

func (d Decider) Check(ctx context.Context, req authz.Request) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
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
	rc, ok := reqctx.From(ctx)
	if !ok {
		return false, reqctx.ErrMissing
	}
	if rc.Tenant == "" {
		if req.Object != "platform:miaoqia" {
			return false, nil
		}
		return d.Store.Check(ctx, req)
	}
	// 带租户上下文时只判断调用主体自身；租户、单元与业务资源须属于上下文租户（Directory 解析，否则为拒绝）。
	if req.Subject != authz.Subject(rc.Principal) {
		return false, nil
	}
	switch rc.Principal.Kind {
	case "user":
		return d.Directory.CheckMember(ctx, req.Relation, req.Object)
	case "service":
		return d.Directory.CheckService(ctx, req.Relation, req.Object)
	}
	return false, nil
}
