// Package bff 是对象页与证据面板的聚合（PRD 第4段；gateway.md 第10节）。每个请求按调用方现场裁剪，顺序固定：身份与 tenant 上的
// can_search_public → 信号上下文（只对能看到信号的用户）→ 对象解析（按视角）→ 覆盖范围 → 安全上限 → 读取内容 → 计量 → 响应。
// 任何一步不通过即拒绝，之后的步骤不执行；计量服务不可用时放行并以发件箱事件事后补记，其余检查不放宽。不缓存响应。
package bff

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	commerce "github.com/superailabs/miaoqia/services/subsystems/commerce/api"
	customercontext "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	detection "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/api"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// ViewDeferred 是计量补记事件（contracts/events/gateway）。
const ViewDeferred = "gateway.view_meter.deferred.v1"

const (
	pageLimit = 20  // 列表每页条数（PRD 第4段第13节第4项）
	depthCap  = 200 // 翻页深度上限
	purpose   = "object_page"
)

var writer = outbox.Writer{Subsystem: api.Name, Namespace: "gateway", Zone: reqctx.ZoneTenant}

// 各对象类型的分区（契约 SectionName）。
var sections = map[string][]string{
	"procurement_project": {"summary", "lots", "parent_projects", "notice_timeline"},
	"project":             {"summary", "procurement_projects", "project_timeline", "investment"},
	"organization":        {"registration", "people", "won_history", "supplied_brands", "procured_history", "statistics", "credit"},
}

// 历史视角下的分区取值（PRD 第4段 F7 第5条）：statistics 不支持历史（统计汇总表与画像只维护当前值）；snapshotSections 依赖视图快照
// 还原（求值单位的事实版本、组织的实体版本），快照已过保存期时历史不可用，不改用记录截止时点的查询冒充求值所用的输入。
var (
	noHistory        = []string{"statistics"}
	snapshotSections = []string{"summary", "lots", "registration", "people"}
	typeByPrefix     = map[string]string{"pp": "procurement_project", "prj": "project", "org": "organization"}
)

// Service 是对象页的聚合。
type Service struct {
	DB           *db.DB
	Authz        identity.Authorizer
	Meter        commerce.ViewMetering
	Signals      detection.SignalExplainer
	Context      customercontext.ContextReader
	Public       api.PublicData
	Evidence     api.EvidenceResolver
	Entitlements api.Entitlements
	Now          func() time.Time
	Log          *slog.Logger
	// CursorKey 是分区游标的签名密钥（至少 MinCursorKey 字节；同一部署的全部 api 实例共用）。
	CursorKey []byte
}

// Error 是可在传输边界暴露的错误。
type Error struct {
	Status   int
	Code     string
	Detail   string
	Object   *ObjectName
	Used     int64
	Limit    int64
	ResetsAt time.Time
}

func (e *Error) Error() string { return e.Code }

func failure(status int, code string) error { return &Error{Status: status, Code: code} }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// 响应类型（字段与契约同名，经 JSON 转换为生成类型）。

type ObjectName struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
}

type Merge struct {
	State string    `json:"state"`
	OrgID string    `json:"org_id"`
	At    time.Time `json:"at"`
	Basis string    `json:"basis,omitempty"`
}

type ObjectRef struct {
	ID           string  `json:"id"`
	RequestedID  string  `json:"requested_id"`
	Type         string  `json:"type"`
	Name         string  `json:"name"`
	MergedFrom   []Merge `json:"merged_from,omitempty"`
	CurrentMerge *Merge  `json:"current_merge,omitempty"`
}

type Pinned struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

type Basis struct {
	View           string     `json:"view"`
	RecordedCutoff *time.Time `json:"recorded_cutoff,omitempty"`
	Pinned         []Pinned   `json:"pinned,omitempty"`
}

type SignalContext struct {
	SignalID          string    `json:"signal_id"`
	Disposition       string    `json:"disposition"`
	SubjectID         string    `json:"subject_id"`
	SubjectVersion    int64     `json:"subject_version"`
	PartID            string    `json:"part_id,omitempty"`
	EvaluatedAt       time.Time `json:"evaluated_at"`
	CreatedAt         time.Time `json:"created_at"`
	ContextVersion    int64     `json:"context_snapshot_version"`
	SnapshotAvailable bool      `json:"snapshot_available"`
}

type Meter struct {
	Kind        string     `json:"kind"`
	Outcome     string     `json:"outcome"`
	MeteringDay string     `json:"metering_day,omitempty"`
	Used        *int64     `json:"used,omitempty"`
	Limit       *int64     `json:"limit,omitempty"`
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
}

type Watermark struct {
	TenantID string    `json:"tenant_id"`
	Member   string    `json:"member"`
	IssuedAt time.Time `json:"issued_at"`
}

type Page struct {
	Object             ObjectRef      `json:"object"`
	Basis              Basis          `json:"basis"`
	SignalContext      *SignalContext `json:"signal_context,omitempty"`
	ContextUnavailable bool           `json:"signal_context_unavailable,omitempty"`
	Sections           []string       `json:"sections"`
	Meter              Meter          `json:"meter"`
	Watermark          Watermark      `json:"watermark"`
}

type Section struct {
	ObjectID      string           `json:"object_id"`
	Section       string           `json:"section"`
	Status        string           `json:"status"`
	ReasonCode    string           `json:"reason_code,omitempty"`
	Basis         Basis            `json:"basis"`
	AsOf          *time.Time       `json:"as_of,omitempty"`
	TimeAxis      string           `json:"time_axis,omitempty"`
	CoverageStart string           `json:"coverage_start,omitempty"`
	StatsVersion  string           `json:"stats_version,omitempty"`
	Items         []map[string]any `json:"items"`
	NextCursor    string           `json:"next_cursor,omitempty"`
	Capped        bool             `json:"capped,omitempty"`
	LaterCount    *int             `json:"later_count,omitempty"`
	Meter         *Meter           `json:"meter,omitempty"`
}

type EvidenceSource struct {
	Name             string     `json:"name,omitempty"`
	Title            string     `json:"title,omitempty"`
	URL              string     `json:"url,omitempty"`
	Reachable        *bool      `json:"reachable,omitempty"`
	UnreachableSince *time.Time `json:"unreachable_since,omitempty"`
}

type EvidenceInput struct {
	EvidenceID   string `json:"evidence_id"`
	DisplayLevel string `json:"display_level"`
}

type Evidence struct {
	EvidenceID      string          `json:"evidence_id"`
	Kind            string          `json:"kind"`
	DisplayLevel    string          `json:"display_level"`
	ReasonCode      string          `json:"reason_code,omitempty"`
	ObjectID        string          `json:"object_id,omitempty"`
	Source          EvidenceSource  `json:"source"`
	SnapshotSavedAt *time.Time      `json:"snapshot_saved_at,omitempty"`
	MetadataOnly    bool            `json:"metadata_only"`
	Retracted       bool            `json:"retracted"`
	Snippet         string          `json:"snippet,omitempty"`
	Context         string          `json:"context,omitempty"`
	Locator         string          `json:"locator,omitempty"`
	RecordKey       string          `json:"record_key,omitempty"`
	RecordValue     string          `json:"record_value,omitempty"`
	Transform       string          `json:"transform,omitempty"`
	Inputs          []EvidenceInput `json:"inputs,omitempty"`
	Meter           Meter           `json:"meter"`
}

// view 是一次请求的视角上下文。
type view struct {
	rc                reqctx.Context
	basis             api.Basis
	signal            *SignalContext
	unavailable       bool
	snapshotAvailable bool
	orgUnit           string
	contextVersion    int64
}

// member 要求带租户的用户主体，并经 identity 判断 tenant 上的 can_search_public（外部协作者不是租户成员，没有该关系）。
// 判断失败按失败关闭处理（503，不当作无权限）。
func (s *Service) member(ctx context.Context) (reqctx.Context, error) {
	rc, ok := reqctx.From(ctx)
	if !ok {
		return rc, reqctx.ErrMissing
	}
	if rc.Validate() != nil || rc.Tenant == "" {
		return rc, failure(403, "invalid_tenant_context")
	}
	if rc.Principal.Kind != "user" {
		return rc, failure(403, "forbidden")
	}
	if s.Authz == nil {
		return rc, failure(503, "identity_unavailable")
	}
	ok, err := s.Authz.Check(ctx, authz.Request{Subject: authz.Subject(rc.Principal), Relation: "can_search_public", Object: "tenant:" + rc.Tenant})
	if err != nil {
		return rc, failure(503, "identity_unavailable")
	}
	if !ok {
		return rc, failure(403, "forbidden")
	}
	return rc, nil
}

// begin 完成身份、视角与信号上下文：signal_time 只在用户能看到信号时可用，信号不属于本租户、不存在或看不到时与不带信号一样
// 返回当前视角的公共部分，不提示其存在。信号读取暂时失败时，当前视角照常（只缺上下文条），signal_time 视角失败关闭。
func (s *Service) begin(ctx context.Context, viewName, signalID string) (*view, error) {
	rc, err := s.member(ctx)
	if err != nil {
		return nil, err
	}
	if viewName == "" {
		viewName = api.ViewCurrent
	}
	if viewName != api.ViewCurrent && viewName != api.ViewSignalTime {
		return nil, failure(400, "invalid_request")
	}
	if viewName == api.ViewSignalTime && signalID == "" {
		return nil, &Error{Status: 400, Code: "invalid_request", Detail: "signal_time 视角需要 signal_id"}
	}
	v := &view{rc: rc, basis: api.Basis{View: api.ViewCurrent}, orgUnit: customercontext.RootUnit}
	if signalID == "" {
		return v, nil
	}
	if s.Signals == nil {
		return nil, failure(503, "detection_unavailable")
	}
	e, err := s.Signals.Explain(ctx, signalID)
	if err != nil {
		var safe *detection.Error
		if errors.As(err, &safe) && safe.Status == 404 {
			return v, nil
		}
		if errors.As(err, &safe) && safe.Status < 500 {
			return nil, failure(safe.Status, safe.Code)
		}
		if viewName == api.ViewSignalTime {
			return nil, failure(503, "detection_unavailable")
		}
		v.unavailable = true
		return v, nil
	}
	in := e.Inputs
	// 对外时间只到秒（契约 date-time 序列化精度）；本地与远程两种模式拿到的 evaluated_at 精度不同，
	// 一律截到秒，使展示值、记录截止与游标绑定在两种模式下一致（截止偏保守：同一秒内求值前的记录不纳入）
	in.EvaluatedAt = in.EvaluatedAt.UTC().Truncate(time.Second)
	in.CreatedAt = in.CreatedAt.UTC().Truncate(time.Second)
	v.signal = &SignalContext{SignalID: e.SignalID, Disposition: string(e.Disposition), SubjectID: in.Subject.ID,
		SubjectVersion: in.Subject.Version, PartID: in.Subject.PartID.Or(""), EvaluatedAt: in.EvaluatedAt.UTC(), CreatedAt: in.CreatedAt.UTC(),
		ContextVersion: in.ContextSnapshotVersion, SnapshotAvailable: in.SnapshotAvailable}
	if viewName == api.ViewSignalTime {
		v.basis = api.Basis{View: api.ViewSignalTime, RecordedCutoff: in.EvaluatedAt.UTC()}
		for _, it := range in.ViewItems {
			id, okID := it.ID.Get()
			version, okVersion := it.Version.Get()
			if okID && okVersion {
				v.basis.Pinned = append(v.basis.Pinned, api.Pinned{Name: it.Name, ID: id, Version: version})
			}
		}
		v.snapshotAvailable, v.orgUnit, v.contextVersion = in.SnapshotAvailable, in.OrgUnitID, in.ContextSnapshotVersion
	}
	return v, nil
}

// resolve 按口径解析对象并判断覆盖：覆盖不足时只给出对象名称（不返回内容）。
func (s *Service) resolve(ctx context.Context, id string, basis api.Basis) (*api.Object, error) {
	if s.Public == nil {
		return nil, failure(503, "public_data_unavailable")
	}
	obj, err := s.Public.Resolve(ctx, id, basis)
	switch {
	case errors.Is(err, api.ErrNotFound):
		return nil, failure(404, "object_not_found")
	case err != nil:
		if !errors.Is(err, api.ErrNotWired) {
			s.logger().WarnContext(ctx, "对象解析失败", "error", err)
		}
		return nil, failure(503, "public_data_unavailable")
	}
	prefix, _, _ := strings.Cut(obj.ID, "_")
	want, _, _ := strings.Cut(id, "_")
	if obj.Type != typeByPrefix[want] || typeByPrefix[prefix] != obj.Type || obj.Name == "" {
		s.logger().WarnContext(ctx, "对象解析结果与请求的对象类型不符", "requested", id, "resolved", obj.ID, "type", obj.Type)
		return nil, failure(503, "public_data_unavailable")
	}
	if s.Entitlements == nil {
		return nil, failure(503, "entitlement_unavailable")
	}
	covered, err := s.Entitlements.Covered(ctx, *obj)
	if err != nil {
		return nil, failure(503, "entitlement_unavailable")
	}
	if !covered {
		return nil, &Error{Status: 403, Code: "entitlement.coverage_insufficient", Object: &ObjectName{ID: obj.ID, Type: obj.Type, Name: obj.Name}}
	}
	return obj, nil
}

// requestKey 返回本次页面请求的尝试键（客户端未给出时由网关生成，不可重放）。
func requestKey(key string) string {
	if key != "" {
		return key
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "gw-" + hex.EncodeToString(b)
}

// meter 计一次查看。额度用完为 429 quota.exhausted（带已用量与重置时间）；commerce 不可用时放行并在本子系统的发件箱中登记补记事件
// （同一事务失败则 503，不放行未登记的查看）。
func (s *Service) meter(ctx context.Context, rc reqctx.Context, kind, target, key string) (*Meter, error) {
	if s.Meter != nil {
		r, err := s.Meter.RecordView(ctx, kind, target, key)
		if err == nil {
			used, limit, resets := r.Used, r.Limit, r.ResetsAt.UTC()
			return &Meter{Kind: kind, Outcome: string(r.Outcome), MeteringDay: r.MeteringDay.Format("2006-01-02"), Used: &used, Limit: &limit, ResetsAt: &resets}, nil
		}
		var safe *commerce.Error
		if errors.As(err, &safe) && safe.Code == "quota.exhausted" {
			return nil, &Error{Status: 429, Code: "quota.exhausted", Used: safe.Used, Limit: safe.Limit, ResetsAt: safe.ResetsAt.UTC()}
		}
		if errors.As(err, &safe) && safe.Status < 500 {
			return nil, failure(safe.Status, safe.Code)
		}
		s.logger().WarnContext(ctx, "计量服务不可用，放行并补记", "error", err)
	}
	if s.DB == nil {
		return nil, failure(503, "metering_unavailable")
	}
	data := map[string]any{"kind": kind, "target": target, "member": rc.Principal.ID, "occurred_at": s.now().Format(time.RFC3339Nano), "request_id": key}
	err := s.DB.Tx(ctx, func(tx pgx.Tx) error {
		_, err := writer.Append(ctx, tx, outbox.Event{Type: ViewDeferred, Subject: target, PartitionKey: rc.Principal.ID, Data: data})
		return err
	})
	if err != nil {
		s.logger().WarnContext(ctx, "补记事件写入失败", "error", err)
		return nil, failure(503, "metering_unavailable")
	}
	return &Meter{Kind: kind, Outcome: "deferred"}, nil
}

func basisOut(b api.Basis) Basis {
	out := Basis{View: b.View}
	if b.View == api.ViewSignalTime {
		t := b.RecordedCutoff.UTC()
		out.RecordedCutoff = &t
		for _, p := range b.Pinned {
			out.Pinned = append(out.Pinned, Pinned{Name: p.Name, ID: p.ID, Version: p.Version})
		}
	}
	return out
}

func objectOut(o *api.Object) ObjectRef {
	ref := ObjectRef{ID: o.ID, RequestedID: o.RequestedID, Type: o.Type, Name: o.Name}
	if ref.RequestedID == "" {
		ref.RequestedID = o.ID
	}
	for _, m := range o.MergedFrom {
		ref.MergedFrom = append(ref.MergedFrom, Merge{State: m.State, OrgID: m.OrgID, At: m.At.UTC(), Basis: m.Basis})
	}
	if m := o.CurrentMerge; m != nil {
		ref.CurrentMerge = &Merge{State: m.State, OrgID: m.OrgID, At: m.At.UTC(), Basis: m.Basis}
	}
	return ref
}

// Page 返回对象页页头并计一次对象查看。
func (s *Service) Page(ctx context.Context, objectID, viewName, signalID, key string) (*Page, error) {
	v, err := s.begin(ctx, viewName, signalID)
	if err != nil {
		return nil, err
	}
	obj, err := s.resolve(ctx, objectID, v.basis)
	if err != nil {
		return nil, err
	}
	m, err := s.meter(ctx, v.rc, commerce.ObjectView, obj.ID, requestKey(key))
	if err != nil {
		return nil, err
	}
	return &Page{Object: objectOut(obj), Basis: basisOut(v.basis), SignalContext: v.signal, ContextUnavailable: v.unavailable,
		Sections: sections[obj.Type], Meter: *m, Watermark: Watermark{TenantID: v.rc.Tenant, Member: v.rc.Principal.ID, IssuedAt: s.now()}}, nil
}

// sectionCursor 是分区的分页位置：偏移（执行翻页深度上限）与数据平面的游标，绑定租户、查看者、对象、分区、口径与筛选。
// 对外的游标是整个载荷连同其 HMAC-SHA256 签名（sealCursor）：绑定、偏移与内层游标任何一项被改动都在读取与计量之前拒绝，
// 调用方不能通过重置偏移或替换内层游标绕过翻页深度上限。
type sectionCursor struct {
	Binding string `json:"b"`
	Offset  int    `json:"o"`
	Inner   string `json:"c"`
}

const (
	// MinCursorKey 是游标签名密钥的最短长度（字节）。
	MinCursorKey   = 32
	maxInnerCursor = 512 // 数据平面游标的上限：签名后的游标须在契约的 1024 字符以内
	cursorDomain   = "miaoqia/gateway/section-cursor/v1"
)

func (s *Service) cursorMAC(payload []byte) []byte {
	mac := hmac.New(sha256.New, s.CursorKey)
	mac.Write([]byte(cursorDomain))
	mac.Write([]byte{0})
	mac.Write(payload)
	return mac.Sum(nil)
}

// sealCursor 签发游标：base64url(载荷 JSON ‖ HMAC-SHA256(载荷))，字符集符合契约的游标格式。
func (s *Service) sealCursor(c sectionCursor) (string, error) {
	if len(s.CursorKey) < MinCursorKey {
		return "", failure(503, "cursor_key_unavailable")
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(append(raw, s.cursorMAC(raw)...)), nil
}

// openCursor 先校验签名再解析游标；签名不符、格式错误、绑定不一致或位置越界都是 400 invalid_cursor。
func (s *Service) openCursor(value, bind string) (sectionCursor, error) {
	var c sectionCursor
	if len(s.CursorKey) < MinCursorKey {
		return c, failure(503, "cursor_key_unavailable")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) <= sha256.Size {
		return c, failure(400, "invalid_cursor")
	}
	payload, mac := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	if !hmac.Equal(mac, s.cursorMAC(payload)) {
		return c, failure(400, "invalid_cursor")
	}
	if json.Unmarshal(payload, &c) != nil || c.Binding != bind || c.Offset <= 0 || c.Inner == "" || len(c.Inner) > maxInnerCursor {
		return sectionCursor{}, failure(400, "invalid_cursor")
	}
	return c, nil
}

func bindingOf(parts ...any) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:12])
}

// GetSection 返回一个分区。
func (s *Service) GetSection(ctx context.Context, objectID, section, viewName, signalID, key string, mine bool, limit int, after string) (*Section, error) {
	v, err := s.begin(ctx, viewName, signalID)
	if err != nil {
		return nil, err
	}
	obj, err := s.resolve(ctx, objectID, v.basis)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(sections[obj.Type], section) {
		return nil, failure(404, "section_not_found")
	}
	if limit <= 0 || limit > pageLimit {
		limit = pageLimit
	}
	bind := bindingOf(v.rc.Tenant, v.rc.Principal.ID, obj.ID, section, v.basis.View, v.basis.RecordedCutoff.UnixNano(), mine)
	pos := sectionCursor{Binding: bind}
	if after != "" {
		if pos, err = s.openCursor(after, bind); err != nil {
			return nil, err
		}
	}
	// 安全上限在读取内容与计量之前判断：超过翻页深度不读取、不计量
	if pos.Offset >= depthCap {
		return nil, &Error{Status: 422, Code: "limit.safety_cap", Detail: fmt.Sprintf("列表最多翻到 %d 条，请缩小范围（年份、地区或品类）", depthCap)}
	}
	limit = min(limit, depthCap-pos.Offset)
	out := &Section{ObjectID: obj.ID, Section: section, Basis: basisOut(v.basis), Items: []map[string]any{}}
	if v.basis.View == api.ViewSignalTime {
		if slices.Contains(noHistory, section) {
			out.Status, out.ReasonCode = "history_unavailable", "history_not_supported"
			return out, nil
		}
		if slices.Contains(snapshotSections, section) && !v.snapshotAvailable {
			out.Status, out.ReasonCode = "history_unavailable", "view_snapshot_expired"
			return out, nil
		}
	}
	var categories []string
	if mine {
		if s.Context == nil {
			out.Status, out.ReasonCode = "unavailable", "customer_context_unavailable"
			return out, nil
		}
		snap, err := s.Context.ContextSnapshot(ctx, v.orgUnit, v.contextVersion)
		if err != nil {
			out.Status, out.ReasonCode = "unavailable", "customer_context_unavailable"
			return out, nil
		}
		categories = append([]string{}, snap.PolicyView.OfferingCategories...)
		if len(categories) == 0 {
			out.Status, out.ReasonCode = "empty", "no_profile_categories"
			return out, nil
		}
	}
	if s.Public == nil {
		out.Status, out.ReasonCode = "unavailable", "public_data_unavailable"
		return out, nil
	}
	data, err := s.Public.Section(ctx, api.SectionRequest{Object: *obj, Section: section, Basis: v.basis, Cursor: pos.Inner, Limit: limit, Categories: categories})
	if err != nil {
		if !errors.Is(err, api.ErrNotWired) {
			s.logger().WarnContext(ctx, "分区读取失败", "section", section, "error", err)
		}
		out.Status, out.ReasonCode = "unavailable", "public_data_unavailable"
		return out, nil
	}
	// 响应口径须与请求一致：不一致时不展示（不以当前数据或更晚的版本代替历史）
	if !data.Basis.Same(v.basis) {
		s.logger().WarnContext(ctx, "分区口径与请求不一致", "section", section)
		out.Status, out.ReasonCode = "unavailable", "basis_mismatch"
		return out, nil
	}
	out.Status, out.ReasonCode, out.TimeAxis, out.StatsVersion = data.Status, data.ReasonCode, data.TimeAxis, data.StatsVersion
	if !data.AsOf.IsZero() {
		t := data.AsOf.UTC()
		out.AsOf = &t
	}
	if !data.CoverageStart.IsZero() {
		out.CoverageStart = data.CoverageStart.Format("2006-01-02")
	}
	if data.Status != "ok" && data.Status != "empty" {
		return out, nil
	}
	items := data.Items
	if len(items) > limit {
		items = items[:limit]
	}
	if items != nil {
		out.Items = items
	}
	if v.basis.View == api.ViewSignalTime {
		out.LaterCount = data.LaterCount
	}
	if data.NextCursor != "" {
		next := pos.Offset + len(out.Items)
		switch {
		case next >= depthCap:
			out.Capped = true
		case len(data.NextCursor) > maxInnerCursor || len(out.Items) == 0:
			s.logger().WarnContext(ctx, "数据平面游标无效", "section", section, "length", len(data.NextCursor), "items", len(out.Items))
			return &Section{ObjectID: obj.ID, Section: section, Basis: out.Basis, Items: []map[string]any{}, Status: "unavailable", ReasonCode: "public_data_unavailable"}, nil
		default:
			if out.NextCursor, err = s.sealCursor(sectionCursor{Binding: bind, Offset: next, Inner: data.NextCursor}); err != nil {
				return nil, err
			}
		}
	}
	m, err := s.meter(ctx, v.rc, commerce.ObjectView, obj.ID, requestKey(key))
	if err != nil {
		return nil, err
	}
	out.Meter = m
	return out, nil
}

// 展示级别从宽到严的顺序；未知级别按“不展示”处理。
var levels = []string{api.FullText, api.Excerpt, api.CitationOnly, api.NoDisplay}

func strictest(a, b string) string {
	ia, ib := slices.Index(levels, a), slices.Index(levels, b)
	if ia < 0 || ib < 0 {
		return api.NoDisplay
	}
	return levels[max(ia, ib)]
}

// project 按最终展示级别以白名单填入证据内容（PRD 第4段 F6 第3条），不从数据平面的结果整体复制，未列出的字段一律不返回：
//   - full_text：片段、上下文、定位、记录键与原值、转换说明、快照保存时间与完整来源信息；
//   - excerpt：只有被引用的片段，加定位所需的元数据（定位、记录键、转换说明、快照保存时间与来源信息）；不含上下文与记录原值；
//   - citation_only：来源名称、公告标题、原文链接与可达状态，以及平台自己的转换说明；不含任何原文内容与定位；
//   - none（及未知级别）：只保留来源链接作为替代。
//
// 来源只允许保存元数据（metadata_only）时没有快照正文：任何级别都不返回片段、上下文与记录原值。
func project(e *api.Evidence, level string, out *Evidence) {
	out.Snippet, out.Context, out.Locator, out.RecordKey, out.RecordValue, out.Transform, out.SnapshotSavedAt = "", "", "", "", "", "", nil
	source := func() EvidenceSource {
		src := EvidenceSource{Name: e.Source.Name, Title: e.Source.Title, URL: e.Source.URL}
		reachable := e.Source.Reachable
		src.Reachable = &reachable
		if !e.Source.UnreachableSince.IsZero() {
			t := e.Source.UnreachableSince.UTC()
			src.UnreachableSince = &t
		}
		return src
	}
	saved := func() *time.Time {
		if e.SnapshotSavedAt.IsZero() {
			return nil
		}
		t := e.SnapshotSavedAt.UTC()
		return &t
	}
	switch level {
	case api.FullText:
		out.Source, out.SnapshotSavedAt = source(), saved()
		out.Snippet, out.Context, out.Locator, out.RecordKey, out.RecordValue, out.Transform = e.Snippet, e.Context, e.Locator, e.RecordKey, e.RecordValue, e.Transform
	case api.Excerpt:
		out.Source, out.SnapshotSavedAt = source(), saved()
		out.Snippet, out.Locator, out.RecordKey, out.Transform = e.Snippet, e.Locator, e.RecordKey, e.Transform
	case api.CitationOnly:
		out.Source = source()
		out.Transform = e.Transform
	default:
		out.Source = EvidenceSource{URL: e.Source.URL}
	}
	if e.MetadataOnly {
		out.Snippet, out.Context, out.RecordValue = "", "", ""
	}
}

// GetEvidence 返回证据面板并计一次原文查看。展示级别按 evidence 返回的级别与派生证据全部输入的级别取最严格的一个，
// 并按该级别再次裁剪内容（不依赖数据平面已经裁剪）。
func (s *Service) GetEvidence(ctx context.Context, evidenceID, key string) (*Evidence, error) {
	rc, err := s.member(ctx)
	if err != nil {
		return nil, err
	}
	if s.Evidence == nil {
		return nil, failure(503, "evidence_unavailable")
	}
	e, err := s.Evidence.Resolve(ctx, evidenceID, purpose)
	switch {
	case errors.Is(err, api.ErrNotFound):
		return nil, failure(404, "evidence_not_found")
	case err != nil:
		if !errors.Is(err, api.ErrNotWired) {
			s.logger().WarnContext(ctx, "证据解析失败", "error", err)
		}
		return nil, failure(503, "evidence_unavailable")
	}
	if e.ID != evidenceID || e.ObjectID == "" {
		return nil, failure(503, "evidence_unavailable")
	}
	obj, err := s.resolve(ctx, e.ObjectID, api.Basis{View: api.ViewCurrent})
	if err != nil {
		var safe *Error
		if errors.As(err, &safe) && safe.Status == 404 {
			return nil, failure(404, "evidence_not_found")
		}
		return nil, err
	}
	level := e.DisplayLevel
	if !slices.Contains(levels, level) {
		level = api.NoDisplay
	}
	out := &Evidence{EvidenceID: e.ID, Kind: e.Kind, ObjectID: obj.ID, MetadataOnly: e.MetadataOnly, Retracted: e.Retracted, ReasonCode: e.ReasonCode}
	for _, in := range e.Inputs {
		lvl := in.DisplayLevel
		if !slices.Contains(levels, lvl) {
			lvl = api.NoDisplay
		}
		level = strictest(level, lvl)
		out.Inputs = append(out.Inputs, EvidenceInput{EvidenceID: in.ID, DisplayLevel: lvl})
	}
	out.DisplayLevel = level
	if level != api.FullText && out.ReasonCode == "" {
		out.ReasonCode = "rights.restricted"
	}
	project(e, level, out)
	m, err := s.meter(ctx, rc, commerce.EvidenceView, e.ID, requestKey(key))
	if err != nil {
		return nil, err
	}
	out.Meter = *m
	return out, nil
}
