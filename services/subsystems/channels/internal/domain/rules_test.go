package domain

import (
	"testing"
	"time"
)

func TestRevocationComplete(t *testing.T) {
	now := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	last := now.Add(-CredentialSkew) // 凭据刚好在5秒前到期：边界上视为已过期
	recent := now.Add(-CredentialSkew + time.Second)
	for _, c := range []struct {
		name               string
		state              string
		version, confirmed int64
		last               *time.Time
		want               bool
	}{
		{"撤销已确认、从未签发凭据", Revoking, 2, 2, nil, true},
		{"撤销已确认、凭据过期加偏差", Revoking, 2, 2, &last, true},
		{"撤销已确认、凭据仍在偏差内", Revoking, 2, 2, &recent, false},
		{"撤销未确认", Revoking, 2, 0, nil, false},
		{"只确认了旧版本", Revoking, 3, 2, nil, false},
		{"仍有效的绑定", Bound, 1, 1, nil, false},
		{"已失效的绑定", Invalidated, 2, 2, nil, false},
	} {
		if got := RevocationComplete(c.state, c.version, c.confirmed, c.last, now); got != c.want {
			t.Errorf("%s：得到 %v", c.name, got)
		}
	}
}

func TestConfirmationCoversEveryField(t *testing.T) {
	base := []string{"cbr_1", "wecom", "corp", "user", "tnt", "usr", "sid"}
	digest := Confirmation(base[0], base[1], base[2], base[3], base[4], base[5], base[6])
	for i := range base {
		changed := append([]string(nil), base...)
		changed[i] += "x"
		if Confirmation(changed[0], changed[1], changed[2], changed[3], changed[4], changed[5], changed[6]) == digest {
			t.Errorf("第 %d 项改变后摘要不变", i)
		}
	}
	// 分隔符避免拼接歧义
	if Confirmation("a", "b", "", "", "", "", "") == Confirmation("ab", "", "", "", "", "", "") {
		t.Error("字段拼接有歧义")
	}
}

func TestIssuableUntil(t *testing.T) {
	exp := time.Date(2026, 10, 19, 8, 0, 30, 0, time.UTC)
	if !IssuableUntil(exp).Equal(exp.Add(-5 * time.Second)) {
		t.Fatal(IssuableUntil(exp))
	}
}
