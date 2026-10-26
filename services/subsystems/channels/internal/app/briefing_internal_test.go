package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	det "github.com/superailabs/miaoqia/services/contracts/apis/detection"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	detection "github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

type fixedRights map[string]map[string]api.FieldRight

func (f fixedRights) FieldRights(context.Context, []string) (map[string]map[string]api.FieldRight, error) {
	return f, nil
}

type failingRights struct{}

func (failingRights) FieldRights(context.Context, []string) (map[string]map[string]api.FieldRight, error) {
	return nil, errors.New("注入：权利不可得")
}

func row(id, project string, correction bool) detection.BriefingSignal {
	r := detection.BriefingSignal{Category: "new", Correction: correction, Signal: detection.SignalRow{ID: id, Title: "夹具项目 " + id,
		Disposition: "pursue_candidate", Score: 80, Reasons: []string{"理由一", "理由二", "理由三"}, Categories: []string{"CAT-PIPE-PE"},
		Location: det.NewOptString("330106"), CreatedAt: time.Date(2026, 10, 19, 23, 0, 0, 0, time.UTC)}}
	r.Signal.Winner = det.NewOptSignalRowWinner(det.SignalRowWinner{Name: det.NewOptString("夹具中标方")})
	r.Signal.Amount = det.NewOptSignalRowAmount(det.SignalRowAmount{Value: "6000000", Currency: "CNY"})
	if project != "" {
		r.Signal.ProcurementProjectID = det.NewOptString(project)
	}
	return r
}

func sectionOf(id, code string, quota int, pending int, rows ...detection.BriefingSignal) composedSection {
	return composedSection{section: section{Kind: "objective", Items: []item{}, ObjectiveSection: &ObjectiveSection{ObjectiveID: id, ObjectiveCode: code, Quota: quota}},
		page: &detection.BriefingSignalPage{ObjectiveID: id, Signals: rows, PendingTotal: pending}}
}

// 用例 10、11、12：配额只限制普通条目（更正类不受配额）、超出计入“另有 N 条”（含超出 detection 单次上限的待处理条目）；
// 同一采购项目跨区只在靠前的区完整展示，其后的区为引用行、靠前的区标“也命中”；同一区内的同一项目不互相引用。
func TestFillItemsQuotaCorrectionDedup(t *testing.T) {
	s := &Service{Rights: fixedRights{}}
	secs := []composedSection{
		sectionOf("obj_A", "A02", 2, 5, row("sig_c", "prj_1", true), row("sig_1", "prj_1", false), row("sig_2", "prj_2", false), row("sig_3", "prj_3", false)),
		sectionOf("obj_B", "A01", 3, 2, row("sig_4", "prj_2", false), row("sig_5", "", false)),
	}
	s.fillItems(context.Background(), secs)
	a, b := secs[0], secs[1]
	if len(a.Items) != 3 || a.Items[0].SignalID != "sig_c" || !a.Items[0].Correction || a.Items[1].SignalID != "sig_1" || a.Items[2].SignalID != "sig_2" {
		t.Fatalf("A：%+v", a.Items)
	}
	// 配额 2：sig_3 超出；PendingTotal 5 − 返回的 4 条 = 1 条未返回
	if a.OverflowCount != 2 {
		t.Fatalf("A 另有 %d", a.OverflowCount)
	}
	if len(b.Items) != 2 || b.Items[0].Kind != "reference" || b.Items[0].References != "A02" || b.Items[0].SignalID != "sig_4" || b.Items[1].Kind != "signal" {
		t.Fatalf("B：%+v", b.Items)
	}
	if got := a.Items[2].AlsoMatched; len(got) != 1 || got[0] != "A01" {
		t.Fatalf("也命中：%v", got)
	}
	// 引用行只有标识、处置与项目字段，没有理由、中标方等
	if ref := b.Items[0].SignalItem; ref.Winner != nil || ref.Reasons != nil || ref.Amount != nil {
		t.Fatalf("%+v", ref)
	}
	// 同区同项目（sig_c 与 sig_1）不互相引用
	if a.Items[1].Kind != "signal" || a.Items[0].AlsoMatched != nil {
		t.Fatalf("%+v", a.Items)
	}
	if b.OverflowCount != 0 {
		t.Fatalf("B 另有 %d", b.OverflowCount)
	}
	// 理由最多两条
	if len(a.Items[1].Reasons) != 2 {
		t.Fatalf("%v", a.Items[1].Reasons)
	}
}

// 越权与反例：展示级别逐字段白名单——低级别不得泄露值；未知级别、缺少权利、权利服务失败都失败关闭为 none。
func TestDisplayLevelWhitelist(t *testing.T) {
	r := row("sig_1", "", false)
	cases := []struct {
		name   string
		rights api.Rights
		check  func(t *testing.T, it *SignalItem)
	}{
		{"full_text", fixedRights{"sig_1": {"winner": {Levels: []string{"full_text"}}, "amount": {Levels: []string{"full_text"}},
			"project": {Levels: []string{"full_text"}}}}, func(t *testing.T, it *SignalItem) {
			if it.Winner.Value != "夹具中标方" || it.Amount.Value == "" || it.Project.Value == "" {
				t.Fatalf("%+v", it)
			}
		}},
		{"excerpt_truncates", fixedRights{"sig_1": {"project": {Levels: []string{"excerpt"}}}}, func(t *testing.T, it *SignalItem) {
			if it.Project.Level != "excerpt" || it.Project.Value == "" {
				t.Fatalf("%+v", it.Project)
			}
		}},
		{"citation_only_no_value", fixedRights{"sig_1": {"winner": {Levels: []string{"full_text", "citation_only"}, SourceURL: "https://example.com/n/1"}}},
			func(t *testing.T, it *SignalItem) {
				if it.Winner.Level != "citation_only" || it.Winner.Value != "" || it.Winner.SourceURL != "https://example.com/n/1" {
					t.Fatalf("%+v", it.Winner)
				}
			}},
		{"citation_only_rejects_non_http_url", fixedRights{"sig_1": {"winner": {Levels: []string{"citation_only"}, SourceURL: "javascript:alert(1)"}}},
			func(t *testing.T, it *SignalItem) {
				if it.Winner.SourceURL != "" || it.Winner.Value != "" {
					t.Fatalf("%+v", it.Winner)
				}
			}},
		{"none_no_value_no_url", fixedRights{"sig_1": {"winner": {Levels: []string{"excerpt", "none"}, SourceURL: "https://example.com/n/1"}}},
			func(t *testing.T, it *SignalItem) {
				if it.Winner.Level != "none" || it.Winner.Value != "" || it.Winner.SourceURL != "" {
					t.Fatalf("%+v", it.Winner)
				}
			}},
		{"unknown_level_fails_closed", fixedRights{"sig_1": {"winner": {Levels: []string{"full_text", "public_domain"}}}}, func(t *testing.T, it *SignalItem) {
			if it.Winner.Level != "none" || it.Winner.Value != "" {
				t.Fatalf("%+v", it.Winner)
			}
		}},
		{"missing_rights_fail_closed", fixedRights{}, func(t *testing.T, it *SignalItem) {
			for _, f := range []*displayField{it.Project, it.Winner, it.Amount, it.Region, it.Categories} {
				if f.Level != "none" || f.Value != "" || f.SourceURL != "" {
					t.Fatalf("%+v", f)
				}
			}
		}},
		{"rights_error_fail_closed", failingRights{}, func(t *testing.T, it *SignalItem) {
			if it.Winner.Level != "none" || it.Winner.Value != "" || it.Project.Value != "" {
				t.Fatalf("%+v", it)
			}
		}},
		{"rights_not_wired_fail_closed", api.NotWired{}, func(t *testing.T, it *SignalItem) {
			if it.Amount.Level != "none" || it.Amount.Value != "" {
				t.Fatalf("%+v", it.Amount)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Service{Rights: c.rights}
			secs := []composedSection{sectionOf("obj_A", "A02", 10, 1, r)}
			s.fillItems(context.Background(), secs)
			it := secs[0].Items[0].SignalItem
			c.check(t, it)
			// 序列化后的条目不包含来源记录中未按白名单写入的值
			raw, _ := json.Marshal(it)
			if c.name != "full_text" && strings.Contains(string(raw), "夹具中标方") && it.Winner.Level != "full_text" && it.Winner.Level != "excerpt" {
				t.Fatalf("泄露：%s", raw)
			}
			// 卡片文字同样不含被限制的值
			text := ""
			for _, l := range cardLines("A02", it, false, time.UTC, nil) {
				text += l.Text + "\n"
			}
			if it.Winner.Level != "full_text" && it.Winner.Level != "excerpt" && strings.Contains(text, "夹具中标方") {
				t.Fatalf("卡片泄露：%s", text)
			}
		})
	}
}

// 越权与反例：签名状态被篡改、伪造、换用途、换密钥、截断或超长时一律拒绝；密钥过短时签发失败关闭。
func TestSealRejectsTamper(t *testing.T) {
	key := []byte("channels-unit-test-signing-key-0123456789")
	s := &Service{Key: key}
	claims := actionClaims{ID: "atk_1", Tenant: "tnt_1", Recipient: "u1", Signal: "sig_1", Action: "relevant", Content: 3, Card: "card-1",
		Expires: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)}
	tok, err := s.seal(purposeAction, claims)
	if err != nil {
		t.Fatal(err)
	}
	var got actionClaims
	if !s.unseal(purposeAction, tok, &got) || got != claims {
		t.Fatalf("%+v", got)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(tok)
	// 篡改载荷中的每一个字节（含签名部分）都被拒绝
	for i := range raw {
		m := append([]byte{}, raw...)
		m[i] ^= 0x01
		if s.unseal(purposeAction, base64.RawURLEncoding.EncodeToString(m), &got) {
			t.Fatalf("篡改第 %d 字节仍被接受", i)
		}
	}
	// 伪造：改写载荷（换收件人）后重新拼接旧签名
	forged, _ := json.Marshal(actionClaims{ID: "atk_1", Tenant: "tnt_1", Recipient: "u2", Signal: "sig_1", Action: "relevant", Content: 3, Card: "card-1",
		Expires: claims.Expires})
	sig := raw[len(raw)-32:]
	if s.unseal(purposeAction, base64.RawURLEncoding.EncodeToString(append(forged, sig...)), &got) {
		t.Fatal("伪造的载荷被接受")
	}
	if s.unseal(purposeCursor, tok, &got) {
		t.Fatal("动作令牌被当作游标接受")
	}
	other := &Service{Key: []byte("another-deployment-signing-key-012345678")}
	if other.unseal(purposeAction, tok, &got) {
		t.Fatal("其他密钥签发的令牌被接受")
	}
	for _, bad := range []string{"", "!!", tok[:10], strings.Repeat("A", 4096), tok + "A"} {
		if s.unseal(purposeAction, bad, &got) {
			t.Fatalf("格式错误的令牌被接受：%q", bad)
		}
	}
	short := &Service{Key: []byte("short")}
	if _, err := short.seal(purposeAction, claims); err == nil {
		t.Fatal("短密钥必须失败关闭")
	}
	if short.unseal(purposeAction, tok, &got) {
		t.Fatal("短密钥不得校验")
	}
}

// 用例4：成员时区与租户时区不同时，卡片上的时刻同时写两种时区；相同时只写一种。
func TestCardClockDualTimezone(t *testing.T) {
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	at := time.Date(2026, 10, 19, 23, 30, 0, 0, time.UTC)
	if got := clock(at, tokyo, shanghai, "01-02 15:04"); got != "10-20 08:30 东京时间（10-20 07:30 北京时间）" {
		t.Fatal(got)
	}
	if got := clock(at, shanghai, shanghai, "15:04"); got != "07:30 北京时间" {
		t.Fatal(got)
	}
	it := signalFields(row("sig_1", "", false), nil)
	text := ""
	for _, l := range cardLines("进入供货", it, false, tokyo, shanghai) {
		text += l.Text + "\n"
	}
	if !strings.Contains(text, "东京时间") || !strings.Contains(text, "北京时间") {
		t.Fatal(text)
	}
}

// 越权与反例：快照不固定展示权限。组装时 full_text 的字段在权利收紧后按“快照级别与当前级别中更严格者”重新裁剪：
// full_text→none 不留值与链接、full_text→citation_only 只留当前原文链接、full_text→excerpt 截断；当前放宽不放宽快照；
// 权利服务失败或未接入时一律 none。
func TestRestrictToCurrentRights(t *testing.T) {
	long := strings.Repeat("长", 60)
	snap := &SignalItem{SignalID: "sig_1",
		Project: &displayField{Level: "full_text", State: "known", Value: long},
		Winner:  &displayField{Level: "full_text", State: "known", Value: "夹具中标方"},
		Amount:  &displayField{Level: "full_text", State: "known", Value: "600 万"},
		Region:  &displayField{Level: "citation_only", State: "known", SourceURL: "https://example.com/old"}}
	got := restrictItem(snap, map[string]api.FieldRight{
		"project": {Levels: []string{"excerpt"}},
		"winner":  {Levels: []string{"none"}, SourceURL: "https://example.com/w"},
		"amount":  {Levels: []string{"citation_only"}, SourceURL: "https://example.com/a"},
		"region":  {Levels: []string{"full_text"}},
	})
	if got.Project.Level != "excerpt" || got.Project.Value == long || got.Project.Value == "" {
		t.Fatalf("%+v", got.Project)
	}
	if got.Winner.Level != "none" || got.Winner.Value != "" || got.Winner.SourceURL != "" {
		t.Fatalf("full_text→none：%+v", got.Winner)
	}
	if got.Amount.Level != "citation_only" || got.Amount.Value != "" || got.Amount.SourceURL != "https://example.com/a" {
		t.Fatalf("full_text→citation_only：%+v", got.Amount)
	}
	if got.Region.Level != "citation_only" || got.Region.SourceURL != "https://example.com/old" {
		t.Fatalf("当前放宽不得放宽快照：%+v", got.Region)
	}
	if snap.Winner.Value != "夹具中标方" {
		t.Fatal("快照本身不得被修改")
	}
	for name, r := range map[string]api.Rights{"rights_error": failingRights{}, "not_wired": api.NotWired{}, "missing": fixedRights{}} {
		s := &Service{Rights: r}
		secs := []section{{Kind: "objective", Items: []item{{Kind: "signal", SignalItem: snap}, {Kind: "reference", SignalItem: snap}}}}
		s.restrictSections(context.Background(), secs)
		for _, it := range secs[0].Items {
			for _, f := range []*displayField{it.Project, it.Winner, it.Amount, it.Region} {
				if f.Level != "none" || f.Value != "" || f.SourceURL != "" {
					t.Fatalf("%s：%+v", name, f)
				}
			}
		}
		text := ""
		for _, l := range cardLines("进入供货", restrictItem(snap, nil), false, time.UTC, nil) {
			text += l.Text
		}
		if strings.Contains(text, "夹具中标方") || strings.Contains(text, "600 万") {
			t.Fatalf("%s：卡片泄露 %s", name, text)
		}
	}
}
