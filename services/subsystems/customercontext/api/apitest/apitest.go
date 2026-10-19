// Package apitest 是 customer-context 上下文读取接口的提供方契约测试：本地实现与远程绑定运行同一套用例。
package apitest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
)

// Fixture 是契约用例依赖的数据：Reader 是有快照读取权限的上下文（已保存过档案，当前版本 Current ≥ 2），
// Stranger 是同租户内无权读取的主体上下文，Other 是另一个已保存档案的租户上下文。
// Want 是 Reader 租户在版本 1 与当前版本的快照（由提供方事先读出），用于核对两种绑定结果一致。
type Fixture struct {
	Reader, Stranger, Other context.Context
	Current                 int64
	Want                    map[int64]*api.Snapshot
}

func code(err error) string {
	var e *api.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func same(t *testing.T, got, want *api.Snapshot) {
	t.Helper()
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	var x, y any
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal(b, &y)
	if !reflect.DeepEqual(x, y) {
		t.Fatalf("快照不一致：\n得到 %s\n期望 %s", a, b)
	}
}

// RunContextReader 运行上下文读取接口的契约用例。
func RunContextReader(t *testing.T, r api.ContextReader, f Fixture) {
	t.Helper()
	t.Run("当前版本", func(t *testing.T) {
		v, err := r.ContextVersion(f.Reader)
		if err != nil || v.ContextVersion != f.Current {
			t.Fatalf("得到 %+v %v，期望 %d", v, err, f.Current)
		}
	})
	t.Run("缺省版本取当前版本", func(t *testing.T) {
		s, err := r.ContextSnapshot(f.Reader, api.RootUnit, 0)
		if err != nil {
			t.Fatal(err)
		}
		same(t, s, f.Want[f.Current])
	})
	t.Run("按旧版本重放得到原结果", func(t *testing.T) {
		s, err := r.ContextSnapshot(f.Reader, api.RootUnit, 1)
		if err != nil {
			t.Fatal(err)
		}
		same(t, s, f.Want[1])
	})
	t.Run("未来版本不存在", func(t *testing.T) {
		if _, err := r.ContextSnapshot(f.Reader, api.RootUnit, f.Current+1); code(err) != "context_version_not_found" {
			t.Fatalf("得到 %v", err)
		}
	})
	t.Run("负版本是无效请求，不退化为当前版本", func(t *testing.T) {
		s, err := r.ContextSnapshot(f.Reader, api.RootUnit, -1)
		var e *api.Error
		if !errors.As(err, &e) || e.Status != 400 || e.Code != "invalid_request" {
			t.Fatalf("得到 %+v %v，期望 400 invalid_request", s, err)
		}
	})
	t.Run("不属于本租户的组织单元", func(t *testing.T) {
		if _, err := r.ContextSnapshot(f.Reader, "unt_01JA8ZXXXXXXXXXXXXXXXXXXX9", 0); code(err) != "org_unit_not_found" {
			t.Fatalf("得到 %v", err)
		}
	})
	t.Run("无权主体被拒绝", func(t *testing.T) {
		if _, err := r.ContextVersion(f.Stranger); code(err) != "forbidden" {
			t.Fatalf("得到 %v", err)
		}
		if _, err := r.ContextSnapshot(f.Stranger, api.RootUnit, 0); code(err) != "forbidden" {
			t.Fatalf("得到 %v", err)
		}
	})
	t.Run("跨租户只看到本租户的快照", func(t *testing.T) {
		s, err := r.ContextSnapshot(f.Other, api.RootUnit, 1)
		if err != nil {
			t.Fatal(err)
		}
		mine, _ := reqctx.From(f.Reader)
		if s.TenantID == mine.Tenant {
			t.Fatal("另一租户读到了本租户的快照")
		}
	})
	t.Run("缺少请求上下文时拒绝调用", func(t *testing.T) {
		if _, err := r.ContextVersion(context.Background()); err == nil {
			t.Fatal("期望错误")
		}
	})
}
