package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/dataread"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/api"
)

// 夹具（FIXTURE）：标识只借用登记前缀的格式，名称与地区为虚构。
const (
	pp   = "pp_01JAAAAAAAAAAAAAAAAAAAAAAA"
	ntc1 = "ntc_01JAAAAAAAAAAAAAAAAAAAAAAB"
	ntc2 = "ntc_01JAAAAAAAAAAAAAAAAAAAAAAC"
	ntc3 = "ntc_01JAAAAAAAAAAAAAAAAAAAAAAD"
)

var (
	t1 = time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)
)

func text(v string) map[string]any {
	return map[string]any{"value": v, "prov": map[string]any{"evidence_ref": "ev_x"}}
}

func lot(key, cat string) map[string]any {
	return map[string]any{"id": "lot_" + strings.Repeat(key, 26), "lot_key": key,
		"items": []any{map[string]any{"id": "itm_x", "name": text("管材"), "categories": []any{map[string]any{"scheme": "s", "scheme_version": "1", "code": cat}}}}}
}

func project(version int, at time.Time, refs ...string) map[string]any {
	var nr []any
	for _, r := range refs {
		nr = append(nr, map[string]any{"id": r, "version": 1})
	}
	return map[string]any{"id": pp, "version": version, "info_domain": "INF-PROC", "recorded_at": at.Format(time.RFC3339),
		"rights_tags": []any{"corpus:x"}, "snapshot_ref": "snp_x", "revision_ref": "exr_x", "classification": "public",
		"stage": "awarded", "title": text(fmt.Sprintf("夹具供水管网项目 v%d", version)), "buyer": map[string]any{"name_raw": "夹具水务公司", "prov": map[string]any{}},
		"location": map[string]any{"code": "330881", "prov": map[string]any{}}, "notice_ids": refs, "notice_refs": nr,
		"lots": []any{lot("A", "CAT-PIPE-PE"), lot("B", "CAT-VALVE"), lot("C", "CAT-PIPE-PE")}}
}

func notice(id string, at time.Time) map[string]any {
	return map[string]any{"id": id, "version": 1, "info_domain": "INF-PROC", "recorded_at": at.Format(time.RFC3339),
		"notice_type": "tender", "title": text("夹具公告 " + id), "rights_tags": []any{"corpus:x"}}
}

// fakeDataAPI 模拟 data-api 的 get-fact：按 current / known_at 与固定版本选版本；只接受租户内的交互式用户主体；
// deny 中的事实对该租户按来源权利拒绝（403）。tamper 篡改响应用于反例。
type fakeDataAPI struct {
	mu       sync.Mutex
	versions map[string][]map[string]any
	deny     map[string]string // 事实 → 被拒绝的租户
	tamper   func(body map[string]any, resp map[string]any)
	calls    []map[string]any
	contexts []reqctx.Context
}

func (f *fakeDataAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/internal/v1/data/get-fact" {
		http.Error(w, "unexpected", 500)
		return
	}
	rc, err := reqctx.Decode(r.Header.Get(reqctx.Header))
	if err != nil || rc.Principal.Kind != "user" || rc.Tenant == "" || rc.Purpose != "interactive" {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"title":"request rejected","status":403}`))
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.calls = append(f.calls, body)
	f.contexts = append(f.contexts, rc)
	id := body["fact_id"].(string)
	if f.deny[id] == rc.Tenant {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"title":"request rejected","status":403}`))
		return
	}
	basis := body["basis"].(map[string]any)
	var cutoff time.Time
	if c, ok := basis["recorded_cutoff"].(string); ok {
		cutoff, _ = time.Parse(time.RFC3339Nano, c)
	}
	var chosen map[string]any
	for _, v := range f.versions[id] {
		at, _ := time.Parse(time.RFC3339, v["recorded_at"].(string))
		if !cutoff.IsZero() && at.After(cutoff) {
			continue
		}
		if want, ok := body["version"].(float64); ok && int(want) != v["version"].(int) {
			continue
		}
		chosen = v
	}
	w.Header().Set("Content-Type", "application/json")
	if chosen == nil {
		_, _ = w.Write([]byte("null"))
		return
	}
	resp := map[string]any{"basis": basis, "fact": chosen}
	if f.tamper != nil {
		resp = deepCopy(resp)
		f.tamper(body, resp)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func deepCopy(v map[string]any) map[string]any {
	b, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func (f *fakeDataAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeDataAPI) lastCall() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func setup(t *testing.T) (*fakeDataAPI, PublicData) {
	t.Helper()
	f := &fakeDataAPI{versions: map[string][]map[string]any{
		pp:   {project(1, t1, ntc1, ntc2), project(2, t2, ntc1, ntc2, ntc3)},
		ntc1: {notice(ntc1, t1)}, ntc2: {notice(ntc2, t1)}, ntc3: {notice(ntc3, t2)},
	}, deny: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client, err := dataread.NewWithHTTPClient(srv.URL, &http.Client{Transport: binding.Transport{}})
	if err != nil {
		t.Fatal(err)
	}
	return f, PublicData{Data: client}
}

func user(tenant string) context.Context {
	return reqctx.With(context.Background(), reqctx.Context{Tenant: tenant, Cell: "cell-01", Purpose: "interactive",
		Principal: reqctx.Principal{ID: "u-" + tenant, Kind: "user"}})
}

var (
	current = api.Basis{View: api.ViewCurrent}
	atT1    = api.Basis{View: api.ViewSignalTime, RecordedCutoff: t1.Add(time.Hour)}
)

func TestResolveProcurementProject(t *testing.T) {
	f, p := setup(t)
	obj, err := p.Resolve(user("tnt_a"), pp, current)
	if err != nil {
		t.Fatal(err)
	}
	if obj.ID != pp || obj.Type != "procurement_project" || obj.Name != "夹具供水管网项目 v2" || obj.Region != "330881" || obj.InfoDomain != "INF-PROC" {
		t.Fatalf("%+v", obj)
	}
	if b := f.lastCall()["basis"].(map[string]any); b["view"] != "current" || b["recorded_cutoff"] != nil || f.lastCall()["version"] != nil {
		t.Fatalf("current 口径应不带截止时点与版本：%v", f.lastCall())
	}
	if f.contexts[0].Tenant != "tnt_a" || f.contexts[0].Principal.ID != "u-tnt_a" {
		t.Fatalf("应以调用者本人的上下文读取：%+v", f.contexts[0])
	}
	// signal_time：当时已知口径取截止时点之前的版本
	obj, err = p.Resolve(user("tnt_a"), pp, atT1)
	if err != nil || obj.Name != "夹具供水管网项目 v1" {
		t.Fatalf("%+v %v", obj, err)
	}
	if b := f.lastCall()["basis"].(map[string]any); b["view"] != "known_at" || b["recorded_cutoff"] == nil {
		t.Fatalf("signal_time 应为 known_at：%v", f.lastCall())
	}
	// 视图快照固定的版本按固定版本读取
	pinnedBasis := api.Basis{View: api.ViewSignalTime, RecordedCutoff: t2.Add(time.Hour), Pinned: []api.Pinned{{Name: "pp", ID: pp, Version: 1}}}
	obj, err = p.Resolve(user("tnt_a"), pp, pinnedBasis)
	if err != nil || obj.Name != "夹具供水管网项目 v1" || f.lastCall()["version"] != float64(1) {
		t.Fatalf("%+v %v %v", obj, err, f.lastCall())
	}
}

func TestResolveOutsideScope(t *testing.T) {
	f, p := setup(t)
	for _, id := range []string{"prj_01JAAAAAAAAAAAAAAAAAAAAAAA", "org_01JAAAAAAAAAAAAAAAAAAAAAAA"} {
		if _, err := p.Resolve(user("tnt_a"), id, current); !errors.Is(err, api.ErrNotWired) {
			t.Fatalf("%s：项目与组织依赖 entity-graph，应为未接入：%v", id, err)
		}
	}
	for _, id := range []string{"pp_x", "ntc_01JAAAAAAAAAAAAAAAAAAAAAAB", "pp_01JAAAAAAAAAAAAAAAAAAAAAA'"} {
		if _, err := p.Resolve(user("tnt_a"), id, current); !errors.Is(err, api.ErrNotFound) {
			t.Fatalf("%s：应为不存在：%v", id, err)
		}
	}
	if f.callCount() != 0 {
		t.Fatal("范围外的标识不应调用 data-api")
	}
	if _, err := p.Resolve(user("tnt_a"), pp, api.Basis{View: api.ViewSignalTime, RecordedCutoff: t1.Add(-24 * time.Hour)}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("截止时点之前不存在的对象应为不存在：%v", err)
	}
	if _, err := (PublicData{}).Resolve(user("tnt_a"), pp, current); !errors.Is(err, api.ErrNotWired) {
		t.Fatalf("没有客户端时应为未接入：%v", err)
	}
}

// 越权与反例：没有用户上下文不发起读取；来源权利拒绝不当作“不存在”；响应与请求不一致时不展示。
func TestFailClosed(t *testing.T) {
	f, p := setup(t)
	for name, ctx := range map[string]context.Context{
		"无请求上下文": context.Background(),
		"服务主体":   reqctx.With(context.Background(), reqctx.Context{Tenant: "tnt_a", Cell: "cell-01", Principal: reqctx.Principal{ID: "svc:gateway", Kind: "service"}}),
		"无租户用户":  reqctx.With(context.Background(), reqctx.Context{Principal: reqctx.Principal{ID: "u", Kind: "user"}}),
	} {
		if _, err := p.Resolve(ctx, pp, current); err == nil || errors.Is(err, api.ErrNotFound) {
			t.Fatalf("%s：应失败（且不是不存在）：%v", name, err)
		}
	}
	if f.callCount() != 0 {
		t.Fatal("没有租户内用户上下文时不应调用 data-api")
	}

	f.deny[pp] = "tnt_b"
	if _, err := p.Resolve(user("tnt_b"), pp, current); err == nil || errors.Is(err, api.ErrNotFound) {
		t.Fatalf("来源权利拒绝应为读取失败（503），不是不存在：%v", err)
	} else if de := (*dataread.Error)(nil); !errors.As(err, &de) || de.Status != 403 {
		t.Fatalf("应保留 data-api 的拒绝：%v", err)
	}
	if _, err := p.Resolve(user("tnt_a"), pp, current); err != nil {
		t.Fatalf("其他租户不受影响：%v", err)
	}
	delete(f.deny, pp)

	for name, tamper := range map[string]func(body, resp map[string]any){
		"返回别的对象":   func(_, r map[string]any) { r["fact"].(map[string]any)["id"] = "pp_01JBBBBBBBBBBBBBBBBBBBBBBB" },
		"口径不同":     func(_, r map[string]any) { r["basis"] = map[string]any{"view": "current", "recorded_cutoff": nil} },
		"截止时点不同":   func(_, r map[string]any) { r["basis"].(map[string]any)["recorded_cutoff"] = t2.Format(time.RFC3339) },
		"固定版本不符":   func(_, r map[string]any) { r["fact"].(map[string]any)["version"] = 2 },
		"记录时间晚于截止": func(_, r map[string]any) { r["fact"].(map[string]any)["recorded_at"] = t2.Format(time.RFC3339) },
		"信息领域不符":   func(_, r map[string]any) { r["fact"].(map[string]any)["info_domain"] = "INF-ORG" },
		"缺少名称":     func(_, r map[string]any) { delete(r["fact"].(map[string]any), "title") },
		"缺少记录时间":   func(_, r map[string]any) { delete(r["fact"].(map[string]any), "recorded_at") },
	} {
		f.tamper = tamper
		basis := api.Basis{View: api.ViewSignalTime, RecordedCutoff: t1.Add(time.Hour), Pinned: []api.Pinned{{ID: pp, Version: 1}}}
		if _, err := p.Resolve(user("tnt_a"), pp, basis); err == nil || errors.Is(err, api.ErrNotFound) || errors.Is(err, api.ErrNotWired) {
			t.Fatalf("%s：应为读取失败：%v", name, err)
		}
	}
	f.tamper = nil
	if _, err := p.Resolve(user("tnt_a"), pp, api.Basis{View: api.ViewSignalTime}); err == nil {
		t.Fatal("signal_time 缺少截止时点应拒绝")
	}
	if _, err := p.Resolve(user("tnt_a"), pp, api.Basis{View: api.ViewCurrent, Pinned: []api.Pinned{{ID: pp, Version: 0}}}); err == nil {
		t.Fatal("无效的固定版本应拒绝")
	}
}

func section(t *testing.T, p PublicData, ctx context.Context, name string, b api.Basis, cursor string, limit int, cats ...string) *api.SectionData {
	t.Helper()
	obj := api.Object{ID: pp, Type: "procurement_project", Name: "x", InfoDomain: "INF-PROC"}
	d, err := p.Section(ctx, api.SectionRequest{Object: obj, Section: name, Basis: b, Cursor: cursor, Limit: limit, Categories: cats})
	if err != nil {
		t.Fatalf("%s：%v", name, err)
	}
	if !d.Basis.Same(b) {
		t.Fatalf("%s：响应应回带请求口径", name)
	}
	return d
}

func TestSections(t *testing.T) {
	f, p := setup(t)
	ctx := user("tnt_a")

	s := section(t, p, ctx, "summary", current, "", 20)
	if s.Status != "ok" || len(s.Items) != 1 || s.AsOf != t2 {
		t.Fatalf("%+v", s)
	}
	item := s.Items[0]
	for _, k := range []string{"rights_tags", "snapshot_ref", "revision_ref", "classification", "lots", "notice_refs", "recorded_at"} {
		if _, ok := item[k]; ok {
			t.Fatalf("概要不应返回 %s：%v", k, item)
		}
	}
	if item["id"] != pp || item["version"] != 2 || item["stage"] != "awarded" || item["location"] == nil {
		t.Fatalf("%v", item)
	}

	l := section(t, p, ctx, "lots", current, "", 2)
	if l.Status != "ok" || len(l.Items) != 2 || l.NextCursor != "2" {
		t.Fatalf("%+v", l)
	}
	l = section(t, p, ctx, "lots", current, "2", 2)
	if len(l.Items) != 1 || l.NextCursor != "" || l.Items[0]["lot_key"] != "C" {
		t.Fatalf("%+v", l)
	}
	l = section(t, p, ctx, "lots", current, "", 20, "CAT-VALVE")
	if len(l.Items) != 1 || l.Items[0]["lot_key"] != "B" {
		t.Fatalf("含我方品类应只返回匹配的标包：%+v", l)
	}
	if l = section(t, p, ctx, "lots", current, "", 20, "CAT-NONE"); l.Status != "empty" || l.ReasonCode != "no_matching_categories" {
		t.Fatalf("%+v", l)
	}
	for _, bad := range []string{"x", "-1", "99"} {
		obj := api.Object{ID: pp, Type: "procurement_project"}
		if _, err := p.Section(ctx, api.SectionRequest{Object: obj, Section: "lots", Basis: current, Cursor: bad, Limit: 2}); err == nil {
			t.Fatalf("无效游标 %q 应拒绝", bad)
		}
	}

	n := section(t, p, ctx, "notice_timeline", current, "", 2)
	if n.Status != "ok" || len(n.Items) != 2 || n.NextCursor != "2" || n.Items[0]["notice_id"] != ntc1 || n.Items[0]["notice_type"] != "tender" || n.LaterCount != nil {
		t.Fatalf("%+v", n)
	}
	if _, ok := n.Items[0]["rights_tags"]; ok {
		t.Fatal("公告条目不应返回权利标签")
	}
	if c := f.lastCall(); c["fact_id"] != ntc2 || c["version"] != float64(1) {
		t.Fatalf("公告按聚合固定的版本读取：%v", c)
	}
	// signal_time：当时的聚合只有两条，之后加入一条
	n = section(t, p, ctx, "notice_timeline", atT1, "", 20)
	if len(n.Items) != 2 || n.LaterCount == nil || *n.LaterCount != 1 {
		t.Fatalf("%+v", n)
	}
	if pp := section(t, p, ctx, "parent_projects", current, "", 20); pp.Status != "not_connected" || len(pp.Items) != 0 {
		t.Fatalf("所属项目未接入时应为 not_connected：%+v", pp)
	}

	// 任一条公告被来源权利拒绝时整个时间线不可用，不跳过
	f.deny[ntc2] = "tnt_a"
	obj := api.Object{ID: pp, Type: "procurement_project"}
	if _, err := p.Section(ctx, api.SectionRequest{Object: obj, Section: "notice_timeline", Basis: current, Limit: 20}); err == nil {
		t.Fatal("被拒绝的公告应使分区不可用")
	}
	if d, err := p.Section(user("tnt_b"), api.SectionRequest{Object: obj, Section: "notice_timeline", Basis: current, Limit: 20}); err != nil || len(d.Items) != 3 {
		t.Fatalf("拒绝只针对该租户：%+v %v", d, err)
	}
	if _, err := p.Section(ctx, api.SectionRequest{Object: api.Object{ID: "org_01JAAAAAAAAAAAAAAAAAAAAAAA", Type: "organization"}, Section: "registration", Basis: current}); !errors.Is(err, api.ErrNotWired) {
		t.Fatalf("组织分区应为未接入：%v", err)
	}
	if _, err := p.Section(ctx, api.SectionRequest{Object: obj, Section: "statistics", Basis: current}); err == nil {
		t.Fatal("采购项目没有的分区应拒绝")
	}
}

// 聚合的公告版本列表异常（重复、超出上限、标识无效）时不展示。
func TestTimelineRejectsMalformedRefs(t *testing.T) {
	f, p := setup(t)
	obj := api.Object{ID: pp, Type: "procurement_project"}
	for name, refs := range map[string][]any{
		"重复":   {map[string]any{"id": ntc1, "version": 1}, map[string]any{"id": ntc1, "version": 1}},
		"标识无效": {map[string]any{"id": "pp_01JAAAAAAAAAAAAAAAAAAAAAAA", "version": 1}},
		"版本无效": {map[string]any{"id": ntc1, "version": 0}},
		"为空":   {},
		"超出上限": slices.Repeat([]any{map[string]any{"id": ntc1, "version": 1}}, maxNotices+1),
	} {
		f.tamper = func(_, r map[string]any) { r["fact"].(map[string]any)["notice_refs"] = refs }
		if _, err := p.Section(user("tnt_a"), api.SectionRequest{Object: obj, Section: "notice_timeline", Basis: current, Limit: 20}); err == nil {
			t.Fatalf("%s：应拒绝", name)
		}
	}
}
