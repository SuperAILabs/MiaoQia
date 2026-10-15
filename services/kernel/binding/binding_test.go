package binding_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

var ctx = reqctx.With(context.Background(), reqctx.Context{Tenant: "tnt_a", Cell: "01", Principal: reqctx.Principal{ID: "usr_1", Kind: "user"}})

func TestInvokeRetriesOnlyIdempotent(t *testing.T) {
	calls := 0
	fail := func(context.Context) (int, error) { calls++; return 0, binding.ErrTransient }
	_, _ = binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: "x", Name: "read", Idempotent: true}, fail)
	if calls != 3 {
		t.Fatalf("幂等操作应重试到 3 次，得到 %d", calls)
	}
	calls = 0
	_, _ = binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: "x", Name: "write"}, fail)
	if calls != 1 {
		t.Fatalf("非幂等操作不应重试，得到 %d", calls)
	}
	calls = 0
	_, err := binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: "x", Name: "read", Idempotent: true},
		func(context.Context) (int, error) { calls++; return 0, errors.New("业务错误") })
	if calls != 1 || err == nil {
		t.Fatal("非暂时性错误不应重试")
	}
	if _, err := binding.Invoke(context.Background(), binding.Local, binding.Op{Name: "x"}, fail); !errors.Is(err, reqctx.ErrMissing) {
		t.Fatal("缺少请求上下文应拒绝调用")
	}
}

// 远程调用：客户端传输附加请求上下文，内部服务端恢复它；缺少上下文头的请求被拒绝。
func TestContextPropagation(t *testing.T) {
	t.Setenv("MIAOQIA_ENV", "development")
	var got reqctx.Context
	srv := httptest.NewServer(binding.InternalServer("x", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = reqctx.From(r.Context())
	})))
	defer srv.Close()
	cl := &http.Client{Transport: binding.Transport{}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Tenant != "tnt_a" || got.Principal.ID != "usr_1" {
		t.Fatalf("请求上下文未传递：%+v", got)
	}
	resp, err = http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("缺少请求上下文头应返回 401，得到 %d", resp.StatusCode)
	}
}
