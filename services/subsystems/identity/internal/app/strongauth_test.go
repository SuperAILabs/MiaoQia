package app

import (
	"testing"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// 强认证时效的边界（PRD 第1段第12节用例 6）：以 IdP 的 auth_time 计算，第14分59秒与第15分00秒满足、第15分01秒不满足；
// 只有 amr=mfa 而无 auth_time、免登会话、未知会话来源、auth_time 超出未来偏差的一律不满足。
func TestFreshStrongAuth(t *testing.T) {
	now := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) reqctx.Assurance {
		return reqctx.Assurance{MFA: true, AuthTime: now.Add(-ago), Source: reqctx.SourceIdP}
	}
	for _, c := range []struct {
		name string
		a    reqctx.Assurance
		want bool
	}{
		{"刚完成", at(0), true},
		{"第14分59秒", at(14*time.Minute + 59*time.Second), true},
		{"第15分00秒", at(15 * time.Minute), true},
		{"第15分01秒", at(15*time.Minute + time.Second), false},
		{"刷新后的旧认证时间", at(2 * time.Hour), false},
		{"只有 amr=mfa", reqctx.Assurance{MFA: true, Source: reqctx.SourceIdP}, false},
		{"没有第二因素", reqctx.Assurance{AuthTime: now, Source: reqctx.SourceIdP}, false},
		{"免登会话", reqctx.Assurance{MFA: true, AuthTime: now, Source: reqctx.SourceChannel}, false},
		{"未知会话来源", reqctx.Assurance{MFA: true, AuthTime: now, Source: reqctx.SourceUnknown}, false},
		{"缺少会话来源", reqctx.Assurance{MFA: true, AuthTime: now}, false},
		{"未来5秒以内", at(-5 * time.Second), true},
		{"未来超过5秒", at(-6 * time.Second), false},
	} {
		if got := freshStrongAuth(c.a, now); got != c.want {
			t.Errorf("%s：得到 %v，期望 %v", c.name, got, c.want)
		}
	}
}
