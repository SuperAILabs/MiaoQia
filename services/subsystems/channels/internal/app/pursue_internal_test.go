package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	pursuit "github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// grantAccess 是按目标给出关系的 ObjectiveAccess 夹具；fail 中的目标判断失败。
type grantAccess struct {
	allowed map[string][]string
	fail    map[string]bool
	calls   []string
}

func (g *grantAccess) Access(ctx context.Context, objective string, relations []string) ([]string, error) {
	rc, _ := reqctx.From(ctx)
	g.calls = append(g.calls, rc.Principal.ID+"/"+objective)
	if g.fail[objective] {
		return nil, errors.New("注入：目标访问判断不可用")
	}
	return g.allowed[objective], nil
}

type stubPursuits struct{ pursuit.Actions }

// PRD 第6段用例 45（跟进入口开关）与第5节：卡片“跟进”只在租户已开启入口、pursuit 动作可用、且收件人本人在目标上有 can_assign 时显示；
// 判断失败的目标不显示（失败关闭）；判断以收件人本人的用户主体进行。
func TestPursueButtonReleaseFlagAndFailClosed(t *testing.T) {
	sctx := reqctx.With(context.Background(), reqctx.Context{Tenant: "tnt_a", Cell: "cell-01", Principal: reqctx.Principal{ID: "svc:channels", Kind: "service"}})
	access := &grantAccess{allowed: map[string][]string{"obj_ok": {"can_view", "can_view_signals", "can_assign"}, "obj_view": {"can_view", "can_view_signals"}},
		fail: map[string]bool{"obj_down": true}}
	s := &Service{Access: access, Pursuits: stubPursuits{}, PursueTenants: map[string]bool{"tnt_a": true}}
	got := s.pursueGrants(sctx, "tnt_a", "usr_bd", []string{"obj_ok", "obj_view", "obj_down"})
	if !got["obj_ok"] || got["obj_view"] || got["obj_down"] {
		t.Fatalf("%+v", got)
	}
	if !slices.Equal(access.calls, []string{"usr_bd/obj_ok", "usr_bd/obj_view", "usr_bd/obj_down"}) {
		t.Fatalf("判断须以收件人本人进行：%v", access.calls)
	}
	// 发布开关未开启的租户：不显示，也不判断
	access.calls = nil
	if got := s.pursueGrants(sctx, "tnt_b", "usr_bd", []string{"obj_ok"}); got["obj_ok"] {
		t.Fatalf("%+v", got)
	}
	if len(access.calls) != 0 {
		t.Fatal("未开启入口的租户不应判断目标访问")
	}
	// pursuit 动作不可用：不显示
	s.Pursuits = nil
	if got := s.pursueGrants(sctx, "tnt_a", "usr_bd", []string{"obj_ok"}); got["obj_ok"] {
		t.Fatalf("%+v", got)
	}
}

// 卡片“跟进”的结果文字：已存在且点击者不能查看时只给负责人；确定失败按代码说明；不提供撤销（replyResult 另有判断）。
func TestPursueResultText(t *testing.T) {
	cases := []struct {
		res  actionResult
		want string
	}{
		{actionResult{Result: "succeeded", Pursuit: "created"}, "已创建跟进"},
		{actionResult{Result: "succeeded", Pursuit: "existing_linked", Owner: "usr_x"}, "已把这条信号关联到它"},
		{actionResult{Result: "no_change", Pursuit: "existing", Owner: "usr_x"}, "负责人 usr_x"},
		{actionResult{Result: "failed", Code: "forbidden"}, "没有在该目标上创建跟进的权限"},
		{actionResult{Result: "failed", Code: "signal_updated"}, "已更新"},
		{actionResult{Result: "failed", Code: "signal_closed"}, "已关闭"},
		{actionResult{Result: "failed", Code: "assignee_no_access"}, "assignee_no_access"},
	}
	for _, c := range cases {
		if got := pursueText(&c.res); !strings.Contains(got, c.want) {
			t.Fatalf("%+v：%s", c.res, got)
		}
	}
}
