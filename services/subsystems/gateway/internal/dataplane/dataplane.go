// Package dataplane 是 gateway 对象页数据平面读取（api.PublicData）的生产适配器：经 data-api 内部读取接口（kernel/dataread）读取
// domain-intelligence 的领域事实（验收线 D-11 的采购项目部分）。
//
// 范围：
//   - 只解析采购项目（pp_）。项目（prj_）与组织（org_）依赖 entity-graph 的读取，data-api 尚未提供，返回 api.ErrNotWired（对象页 503）；
//   - 采购项目的分区：summary（事实字段的白名单）、lots（已确认标包，按“含我方品类”筛选标的品类）、notice_timeline（按聚合固定的
//     公告版本逐条读取）；parent_projects 依赖 entity-graph 的 part_of 归属边，返回 not_connected，不显示为“没有记录”。
//
// 口径：current 对应 data-api 的 current；signal_time 对应 known_at，记录截止时点为信号的 evaluated_at；视图快照固定了该对象的版本时按
// 固定版本读取。响应的对象、类型、信息领域与口径逐项核对，不一致时按读取失败处理（不展示），不以当前数据或更晚的版本代替。
//
// 授权：只以请求上下文中的用户主体调用（data-api 按该用户、租户与用途做来源权利准入与最终确认），没有用户上下文时不发起读取；不缓存
// 任何结果，每次读取都经 data-api 重新判断。
package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/dataread"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/api"
)

// Reader 是 data-api 读取客户端中本适配器使用的部分（*dataread.Client 实现）。
type Reader interface {
	GetFact(ctx context.Context, factID string, basis dataread.Basis, version *int, out any) error
}

// PublicData 实现 api.PublicData。
type PublicData struct {
	Data Reader
}

var (
	ppID     = regexp.MustCompile(`^pp_[0-9A-HJKMNP-TV-Z]{26}$`)
	noticeID = regexp.MustCompile(`^ntc_[0-9A-HJKMNP-TV-Z]{26}$`)
	region   = regexp.MustCompile(`^[0-9]{6}$`)
)

// 采购项目概要的字段白名单（契约 procurement-project.schema.json）；版本元数据（权利标签、快照与修订引用、分级）不返回。
var summaryFields = []string{"title", "stage", "trading_system", "method", "procurement_category", "project_code", "approval_code",
	"investment_project_id", "buyer", "agency", "project_owner", "location", "budget", "first_published_at"}

// 公告时间线每条的字段白名单（契约 notice.schema.json）。
var noticeFields = []string{"notice_type", "title", "notice_code", "published_at"}

const (
	maxNotices = 200 // 聚合成员的上限：超过时按读取失败处理，不截断
	procDomain = "INF-PROC"
)

type wireBasis struct {
	View           string  `json:"view"`
	RecordedCutoff *string `json:"recorded_cutoff"`
}

type factRead struct {
	Basis wireBasis       `json:"basis"`
	Fact  json.RawMessage `json:"fact"`
}

type factMeta struct {
	ID         string `json:"id"`
	Version    int    `json:"version"`
	InfoDomain string `json:"info_domain"`
	RecordedAt string `json:"recorded_at"`
}

type noticeRef struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// fact 是一次读取得到的、已核对过对象与口径的事实。
type fact struct {
	meta factMeta
	raw  map[string]any
	at   time.Time
}

// 读取失败（不是“不存在”）：调用方对象页 503、分区 unavailable。
var errInvalid = errors.New("dataplane：data-api 响应与请求不一致")

func requireUser(ctx context.Context) error {
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Validate() != nil || rc.Principal.Kind != "user" || rc.Tenant == "" {
		return errors.New("dataplane：数据平面读取只以租户内的用户主体调用")
	}
	return nil
}

func toWire(b api.Basis) (dataread.Basis, error) {
	switch b.View {
	case api.ViewCurrent:
		return dataread.Basis{View: "current"}, nil
	case api.ViewSignalTime:
		if b.RecordedCutoff.IsZero() {
			return dataread.Basis{}, errors.New("dataplane：signal_time 缺少记录截止时点")
		}
		t := b.RecordedCutoff.UTC()
		return dataread.Basis{View: "known_at", RecordedCutoff: &t}, nil
	}
	return dataread.Basis{}, fmt.Errorf("dataplane：不支持的视角 %q", b.View)
}

func sameBasis(want dataread.Basis, got wireBasis) bool {
	if got.View != want.View {
		return false
	}
	if want.RecordedCutoff == nil {
		return got.RecordedCutoff == nil
	}
	if got.RecordedCutoff == nil {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, *got.RecordedCutoff)
	return err == nil && t.Equal(*want.RecordedCutoff)
}

// pinned 返回视图快照为该对象固定的版本（没有时为 nil）。
func pinned(b api.Basis, id string) (*int, error) {
	for _, p := range b.Pinned {
		if p.ID == id {
			if p.Version < 1 || p.Version > int64(^uint32(0)>>1) {
				return nil, fmt.Errorf("dataplane：固定版本无效 %d", p.Version)
			}
			v := int(p.Version)
			return &v, nil
		}
	}
	return nil, nil
}

// read 读取一个事实并核对：响应口径与请求一致、事实标识与所问一致、版本与固定版本一致、记录时间不晚于记录截止时点。
// 不存在（或按口径不可见）时返回 api.ErrNotFound。
func (p PublicData) read(ctx context.Context, id string, basis dataread.Basis, version *int) (*fact, error) {
	if p.Data == nil {
		return nil, api.ErrNotWired
	}
	if err := requireUser(ctx); err != nil {
		return nil, err
	}
	var res *factRead
	if err := p.Data.GetFact(ctx, id, basis, version, &res); err != nil {
		return nil, err
	}
	if res == nil || len(res.Fact) == 0 || string(res.Fact) == "null" {
		return nil, api.ErrNotFound
	}
	if !sameBasis(basis, res.Basis) {
		return nil, fmt.Errorf("%w：口径", errInvalid)
	}
	var meta factMeta
	if err := json.Unmarshal(res.Fact, &meta); err != nil {
		return nil, fmt.Errorf("%w：%v", errInvalid, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(res.Fact, &raw); err != nil {
		return nil, fmt.Errorf("%w：%v", errInvalid, err)
	}
	if meta.ID != id || meta.Version < 1 || (version != nil && meta.Version != *version) {
		return nil, fmt.Errorf("%w：事实标识或版本", errInvalid)
	}
	at, err := time.Parse(time.RFC3339Nano, meta.RecordedAt)
	if err != nil {
		return nil, fmt.Errorf("%w：记录时间", errInvalid)
	}
	if basis.RecordedCutoff != nil && at.After(*basis.RecordedCutoff) {
		return nil, fmt.Errorf("%w：记录时间晚于记录截止时点", errInvalid)
	}
	return &fact{meta: meta, raw: raw, at: at}, nil
}

func (p PublicData) project(ctx context.Context, id string, b api.Basis) (*fact, error) {
	if !ppID.MatchString(id) {
		if strings.HasPrefix(id, "prj_") || strings.HasPrefix(id, "org_") {
			return nil, api.ErrNotWired // entity-graph 的读取尚未接入
		}
		return nil, api.ErrNotFound
	}
	basis, err := toWire(b)
	if err != nil {
		return nil, err
	}
	version, err := pinned(b, id)
	if err != nil {
		return nil, err
	}
	f, err := p.read(ctx, id, basis, version)
	if err != nil {
		return nil, err
	}
	if f.meta.InfoDomain != procDomain {
		return nil, fmt.Errorf("%w：信息领域", errInvalid)
	}
	return f, nil
}

// Resolve 实现 api.PublicData。
func (p PublicData) Resolve(ctx context.Context, id string, b api.Basis) (*api.Object, error) {
	f, err := p.project(ctx, id, b)
	if err != nil {
		return nil, err
	}
	title, _ := f.raw["title"].(map[string]any)
	name, _ := title["value"].(string)
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w：缺少项目名称", errInvalid)
	}
	obj := &api.Object{ID: id, RequestedID: id, Type: "procurement_project", Name: name, InfoDomain: procDomain}
	if loc, ok := f.raw["location"].(map[string]any); ok {
		if code, _ := loc["code"].(string); region.MatchString(code) {
			obj.Region = code // 缺少实施地时为空：覆盖判断只在“全部区域”时覆盖
		}
	}
	return obj, nil
}

func pick(raw map[string]any, fields []string) map[string]any {
	out := map[string]any{}
	for _, k := range fields {
		if v, ok := raw[k]; ok {
			out[k] = v
		}
	}
	return out
}

// page 对固定的条目列表做偏移分页（游标为下一条的偏移）。
func page(items []map[string]any, cursor string, limit int) ([]map[string]any, string, error) {
	offset := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 || n > len(items) {
			return nil, "", fmt.Errorf("dataplane：分区游标无效")
		}
		offset = n
	}
	if limit < 1 {
		limit = 1
	}
	end := min(offset+limit, len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[offset:end], next, nil
}

func lotCategories(lot map[string]any) []string {
	var out []string
	items, _ := lot["items"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		cats, _ := m["categories"].([]any)
		for _, c := range cats {
			cm, _ := c.(map[string]any)
			if code, _ := cm["code"].(string); code != "" {
				out = append(out, code)
			}
		}
	}
	return out
}

// Section 实现 api.PublicData。
func (p PublicData) Section(ctx context.Context, req api.SectionRequest) (*api.SectionData, error) {
	if req.Object.Type != "procurement_project" {
		return nil, api.ErrNotWired
	}
	out := &api.SectionData{Basis: req.Basis, TimeAxis: "recorded", Items: []map[string]any{}}
	if req.Section == "parent_projects" {
		out.Status, out.ReasonCode = "not_connected", "entity_graph_not_connected"
		return out, nil
	}
	f, err := p.project(ctx, req.Object.ID, req.Basis)
	if err != nil {
		return nil, err
	}
	out.AsOf = f.at
	var all []map[string]any
	switch req.Section {
	case "summary":
		item := pick(f.raw, summaryFields)
		item["id"], item["version"] = f.meta.ID, f.meta.Version
		all = []map[string]any{item}
	case "lots":
		lots, _ := f.raw["lots"].([]any)
		for _, l := range lots {
			lot, ok := l.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w：标包", errInvalid)
			}
			if len(req.Categories) > 0 && !slices.ContainsFunc(lotCategories(lot), func(c string) bool { return slices.Contains(req.Categories, c) }) {
				continue
			}
			all = append(all, lot)
		}
		if len(all) == 0 {
			out.Status, out.ReasonCode = "empty", "no_confirmed_lots"
			if len(req.Categories) > 0 {
				out.ReasonCode = "no_matching_categories"
			}
			return out, nil
		}
	case "notice_timeline":
		return p.timeline(ctx, req, f, out)
	default:
		return nil, fmt.Errorf("dataplane：采购项目没有分区 %q", req.Section)
	}
	items, next, err := page(all, req.Cursor, req.Limit)
	if err != nil {
		return nil, err
	}
	out.Status, out.Items, out.NextCursor = "ok", items, next
	return out, nil
}

func refsOf(f *fact) ([]noticeRef, error) {
	raw, ok := f.raw["notice_refs"]
	if !ok {
		return nil, fmt.Errorf("%w：聚合缺少固定的公告版本", errInvalid)
	}
	b, _ := json.Marshal(raw)
	var refs []noticeRef
	if err := json.Unmarshal(b, &refs); err != nil || len(refs) == 0 || len(refs) > maxNotices {
		return nil, fmt.Errorf("%w：公告版本列表", errInvalid)
	}
	seen := map[string]bool{}
	for _, r := range refs {
		if !noticeID.MatchString(r.ID) || r.Version < 1 || seen[r.ID] {
			return nil, fmt.Errorf("%w：公告版本列表", errInvalid)
		}
		seen[r.ID] = true
	}
	return refs, nil
}

// timeline 按聚合固定的公告版本逐条读取本页的公告（每条都经 data-api 的权利准入）；任一条读不到时整个分区不可用，不跳过。
// signal_time 下，later_count 为当前口径的聚合中、记录截止时点之后才加入的公告数；当前口径读不到时不给出。
func (p PublicData) timeline(ctx context.Context, req api.SectionRequest, f *fact, out *api.SectionData) (*api.SectionData, error) {
	refs, err := refsOf(f)
	if err != nil {
		return nil, err
	}
	basis, err := toWire(req.Basis)
	if err != nil {
		return nil, err
	}
	all := make([]map[string]any, len(refs))
	for i, r := range refs {
		all[i] = map[string]any{"notice_id": r.ID, "version": r.Version}
	}
	items, next, err := page(all, req.Cursor, req.Limit)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		id, version := item["notice_id"].(string), item["version"].(int)
		n, err := p.read(ctx, id, basis, &version)
		if err != nil {
			if errors.Is(err, api.ErrNotFound) {
				return nil, fmt.Errorf("%w：公告 %s 版本 %d 读不到", errInvalid, id, version)
			}
			return nil, err
		}
		if n.meta.InfoDomain != procDomain {
			return nil, fmt.Errorf("%w：公告信息领域", errInvalid)
		}
		for k, v := range pick(n.raw, noticeFields) {
			item[k] = v
		}
	}
	out.Status, out.Items, out.NextCursor = "ok", items, next
	if req.Basis.View == api.ViewSignalTime {
		if cur, err := p.read(ctx, f.meta.ID, dataread.Basis{View: "current"}, nil); err == nil {
			if currentRefs, err := refsOf(cur); err == nil {
				later := 0
				for _, r := range currentRefs {
					if !slices.ContainsFunc(refs, func(o noticeRef) bool { return o.ID == r.ID }) {
						later++
					}
				}
				out.LaterCount = &later
			}
		}
	}
	return out, nil
}
