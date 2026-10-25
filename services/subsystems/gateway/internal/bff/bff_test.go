package bff

import (
	"context"
	"errors"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/api"
)

type decider func(authz.Request) (bool, error)

func (d decider) Check(_ context.Context, r authz.Request) (bool, error) { return d(r) }

func status(t *testing.T, err error) (int, string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("期望 *Error，得到 %v", err)
	}
	return e.Status, e.Code
}

// 缺省组装（数据平面、证据与覆盖均为 NotWired）：对象页、分区与证据都是 503，不以空数据冒充“没有记录”；
// 身份检查的失败与拒绝都失败关闭，且先于任何数据读取。
func TestNotWiredAndFailClosed(t *testing.T) {
	user := reqctx.With(context.Background(), reqctx.Context{Tenant: "tnt_1", Cell: "cell-01", Principal: reqctx.Principal{ID: "usr_1", Kind: "user"}})
	allow := decider(func(r authz.Request) (bool, error) {
		if r.Relation != "can_search_public" || r.Object != "tenant:tnt_1" {
			t.Errorf("意外的检查 %+v", r)
		}
		return true, nil
	})
	wired := &Service{Authz: allow, Public: api.NotWired{}, Evidence: api.NotWiredEvidence{}, Entitlements: api.NotWired{}}
	if s, c := status(t, func() error { _, err := wired.Page(user, "pp_1", "current", "", "k1"); return err }()); s != 503 || c != "public_data_unavailable" {
		t.Fatalf("对象页 %d %s", s, c)
	}
	if s, c := status(t, func() error {
		_, err := wired.GetSection(user, "pp_1", "summary", "current", "", "k2", false, 20, "")
		return err
	}()); s != 503 || c != "public_data_unavailable" {
		t.Fatalf("分区 %d %s", s, c)
	}
	if s, c := status(t, func() error { _, err := wired.GetEvidence(user, "ev_1", "k3"); return err }()); s != 503 || c != "evidence_unavailable" {
		t.Fatalf("证据 %d %s", s, c)
	}

	for name, d := range map[string]decider{
		"拒绝":   func(authz.Request) (bool, error) { return false, nil },
		"检查失败": func(authz.Request) (bool, error) { return false, errors.New("down") },
	} {
		svc := &Service{Authz: d, Public: panicking{t}, Evidence: panickingEvidence{t}, Entitlements: panicking{t}}
		want := map[string]int{"拒绝": 403, "检查失败": 503}[name]
		if s, _ := status(t, func() error { _, err := svc.Page(user, "pp_1", "current", "", ""); return err }()); s != want {
			t.Fatalf("%s：对象页 %d", name, s)
		}
		if s, _ := status(t, func() error { _, err := svc.GetEvidence(user, "ev_1", ""); return err }()); s != want {
			t.Fatalf("%s：证据 %d", name, s)
		}
	}
	// 未知身份失败关闭；非用户主体（服务）不能使用对象页
	svc := &Service{Public: panicking{t}, Evidence: panickingEvidence{t}, Entitlements: panicking{t}}
	if s, _ := status(t, func() error { _, err := svc.Page(user, "pp_1", "current", "", ""); return err }()); s != 503 {
		t.Fatalf("无授权端口 %d", s)
	}
	service := reqctx.With(context.Background(), reqctx.Context{Tenant: "tnt_1", Cell: "cell-01", Principal: reqctx.Principal{ID: "svc:x", Kind: "service"}})
	svc.Authz = allow
	if s, _ := status(t, func() error { _, err := svc.Page(service, "pp_1", "current", "", ""); return err }()); s != 403 {
		t.Fatalf("服务主体 %d", s)
	}
}

func TestStrictestLevel(t *testing.T) {
	for _, c := range []struct{ a, b, want string }{
		{api.FullText, api.Excerpt, api.Excerpt},
		{api.CitationOnly, api.FullText, api.CitationOnly},
		{api.Excerpt, api.NoDisplay, api.NoDisplay},
		{api.FullText, api.FullText, api.FullText},
	} {
		if got := strictest(c.a, c.b); got != c.want {
			t.Fatalf("strictest(%s,%s)=%s", c.a, c.b, got)
		}
	}
}

// panicking 用于断言拒绝发生在任何数据读取之前。
type panicking struct{ t *testing.T }

func (p panicking) Resolve(context.Context, string, api.Basis) (*api.Object, error) {
	p.t.Fatal("拒绝之前不得解析对象")
	return nil, nil
}
func (p panicking) Section(context.Context, api.SectionRequest) (*api.SectionData, error) {
	p.t.Fatal("拒绝之前不得读取分区")
	return nil, nil
}
func (p panicking) Covered(context.Context, api.Object) (bool, error) {
	p.t.Fatal("拒绝之前不得检查覆盖")
	return false, nil
}

type panickingEvidence struct{ t *testing.T }

func (p panickingEvidence) Resolve(context.Context, string, string) (*api.Evidence, error) {
	p.t.Fatal("拒绝之前不得解析证据")
	return nil, nil
}
