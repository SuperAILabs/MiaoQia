// Package apitest 是 identity 接口的提供方契约测试：本地实现与远程绑定运行同一套用例。
package apitest

import (
	"context"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// Fixture 是契约用例依赖的授权关系：Granted 已授予，其余主体未授予。
type Fixture struct {
	Granted  authz.Request // 例如 user:usr_op can_trigger_probe platform:miaoqia
	Stranger string        // 未授予任何关系的用户标识
}

// RunAuthorizer 运行授权决策点的契约用例。
func RunAuthorizer(t *testing.T, a api.Authorizer, f Fixture) {
	t.Helper()
	ctx := reqctx.With(context.Background(), reqctx.Context{Principal: reqctx.Principal{ID: f.Stranger, Kind: "user"}})
	t.Run("已授予的关系为允许", func(t *testing.T) {
		ok, err := a.Check(ctx, f.Granted)
		if err != nil || !ok {
			t.Fatalf("期望允许，得到 %v %v", ok, err)
		}
	})
	t.Run("缺省主体取请求上下文且未授予时拒绝", func(t *testing.T) {
		ok, err := a.Check(ctx, authz.Request{Relation: f.Granted.Relation, Object: f.Granted.Object})
		if err != nil || ok {
			t.Fatalf("期望拒绝，得到 %v %v", ok, err)
		}
	})
	t.Run("Require 对未授予主体返回 ErrForbidden", func(t *testing.T) {
		if err := authz.Require(ctx, a, f.Granted.Relation, f.Granted.Object); err == nil {
			t.Fatal("期望 ErrForbidden")
		}
	})
	t.Run("缺少请求上下文时拒绝调用", func(t *testing.T) {
		if _, err := a.Check(context.Background(), f.Granted); err == nil {
			t.Fatal("期望错误")
		}
	})
	for _, invalid := range []authz.Request{{Relation: "can:read", Object: f.Granted.Object}, {Relation: f.Granted.Relation, Object: "invalid"}} {
		t.Run("非法业务参数拒绝", func(t *testing.T) {
			if _, err := a.Check(ctx, invalid); err == nil {
				t.Fatal("非法请求必须拒绝")
			}
		})
	}
}
