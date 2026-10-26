package bff

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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

// 展示级别的白名单：摘要不含记录原值与上下文，仅引用不含任何原文内容与定位，不展示只留来源链接；只允许保存元数据时没有正文。
func TestProjectEvidenceWhitelist(t *testing.T) {
	saved := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	e := &api.Evidence{Snippet: "allowed excerpt", Context: "FULL CONTEXT", Locator: "p2", RecordKey: "k", RecordValue: "FULL RESTRICTED RECORD",
		Transform: "t", SnapshotSavedAt: saved, Source: api.EvidenceSource{Name: "n", Title: "ti", URL: "https://example.invalid/x", Reachable: true}}
	for _, c := range []struct {
		level         string
		present, gone []string
	}{
		{api.FullText, []string{"allowed excerpt", "FULL CONTEXT", "p2", "FULL RESTRICTED RECORD", "snapshot_saved_at"}, nil},
		{api.Excerpt, []string{"allowed excerpt", "p2", `"record_key"`, "snapshot_saved_at", `"name"`}, []string{"FULL CONTEXT", "FULL RESTRICTED RECORD", "record_value", `"context"`}},
		{api.CitationOnly, []string{`"name"`, `"title"`, "example.invalid", `"transform"`}, []string{"allowed excerpt", "FULL", "p2", "record_", "snapshot_saved_at"}},
		{api.NoDisplay, []string{"example.invalid"}, []string{"allowed excerpt", "FULL", "p2", "record_", `"name"`, `"title"`, "transform", "snapshot_saved_at"}},
		{"unknown", []string{"example.invalid"}, []string{"allowed excerpt", "FULL", `"name"`}},
	} {
		out := &Evidence{Snippet: "stale", RecordValue: "stale"}
		project(e, c.level, out)
		raw, _ := json.Marshal(out)
		for _, w := range c.present {
			if !strings.Contains(string(raw), w) {
				t.Errorf("%s：缺少 %s：%s", c.level, w, raw)
			}
		}
		for _, w := range append(c.gone, "stale") {
			if strings.Contains(string(raw), w) {
				t.Errorf("%s：不应含 %s：%s", c.level, w, raw)
			}
		}
	}
	meta := *e
	meta.MetadataOnly = true
	out := &Evidence{}
	project(&meta, api.FullText, out)
	if out.Snippet != "" || out.Context != "" || out.RecordValue != "" {
		t.Fatalf("%+v", out)
	}
}

// 游标签名：整个载荷受签名保护；合法签发的超过上限的位置为 limit.safety_cap；缺少密钥时失败关闭。
func TestSectionCursorSealed(t *testing.T) {
	s := &Service{CursorKey: []byte(strings.Repeat("k", MinCursorKey))}
	bind := bindingOf("tnt_1", "usr_1", "org_1", "won_history", "current", int64(0), false)
	tok, err := s.sealCursor(sectionCursor{Binding: bind, Offset: 20, Inner: "20"})
	if err != nil {
		t.Fatal(err)
	}
	if c, err := s.openCursor(tok, bind); err != nil || c.Offset != 20 || c.Inner != "20" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := s.openCursor(tok, bindingOf("tnt_1", "usr_2", "org_1", "won_history", "current", int64(0), false)); err == nil {
		t.Fatal("他人的游标被接受")
	}
	other := &Service{CursorKey: []byte(strings.Repeat("x", MinCursorKey))}
	if _, err := other.openCursor(tok, bind); err == nil {
		t.Fatal("其他密钥签发的游标被接受")
	}
	for _, c := range []sectionCursor{{Binding: bind, Offset: 0, Inner: "220"}, {Binding: bind, Offset: 20, Inner: ""}, {Binding: bind, Offset: 20, Inner: strings.Repeat("c", maxInnerCursor+1)}} {
		tok, _ := s.sealCursor(c)
		if code, _ := status(t, func() error { _, err := s.openCursor(tok, bind); return err }()); code != 400 {
			t.Fatalf("%+v：%d", c, code)
		}
	}
	if long, _ := s.sealCursor(sectionCursor{Binding: bind, Offset: 199, Inner: strings.Repeat("c", maxInnerCursor)}); len(long) > 1024 {
		t.Fatalf("签名后的游标超过契约上限：%d", len(long))
	}
	none := &Service{}
	if code, c := status(t, func() error { _, err := none.openCursor(tok, bind); return err }()); code != 503 || c != "cursor_key_unavailable" {
		t.Fatalf("%d %s", code, c)
	}
	if _, err := none.sealCursor(sectionCursor{Binding: bind, Offset: 1, Inner: "1"}); err == nil {
		t.Fatal("无密钥时签发了游标")
	}
}
