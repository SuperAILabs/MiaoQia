// Package api 是 identity 的公开接口（其他子系统唯一可导入的部分）。规格见 docs/03-architecture/subsystems/identity.md。
package api

import (
	"context"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/binding"
)

// Name 是子系统名。
const Name = "identity"

// AuthorizerBinding 是授权决策点在绑定表中的名称。
const AuthorizerBinding = "identity.Authorizer"

// Authorizer 是授权决策点（Check）。只读、幂等。
type Authorizer = authz.Decider

// Local 把本地实现包装为经调用管线的接口（与远程绑定行为一致）。
func Local(impl Authorizer) Authorizer { return local{impl} }

type local struct{ impl Authorizer }

func (l local) Check(ctx context.Context, req authz.Request) (bool, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "Check", Idempotent: true},
		func(ctx context.Context) (bool, error) { return l.impl.Check(ctx, req) })
}
