package domain

import (
	"reflect"
	"sort"
)

// MaxSnapshotEntities 是快照中我方主体的规模上界（contracts/policy/cel-environment.yaml size_bounds）。
const MaxSnapshotEntities = 10

// View 是快照的 policy_view（策略表达式的 profile 变量）。字段缺失表示未知：
// 未关联本企业或主体超过上界时 OrgIDs 与 Entities 均为 nil（序列化时省略，不用空列表）。
type View struct {
	OrgIDs     []string                    `json:"org_ids,omitempty"`
	Categories []string                    `json:"offering_categories"`
	Regions    []string                    `json:"service_regions"`
	Brands     []string                    `json:"brands"`
	Entities   map[string]EntityQualStatus `json:"entities,omitempty"`
}

// EntityQualStatus 是一个我方主体的资质状态表。
type EntityQualStatus struct {
	Qualifications map[string]string `json:"qualifications"` // 代码 → valid / expired
	Pending        []string          `json:"qualifications_pending"`
	Complete       bool              `json:"qualifications_complete"`
}

// 资质状态。
const (
	QualValid   = "valid"
	QualExpired = "expired"
)

// status 是一条资质记录在 today 的状态；尚未生效的记录不计入（返回空串）。
// 模板声明无有效期的资质没有到期日，录入即有效。有效期最后一天当日仍有效。
func (q Qualification) status(today Date) string {
	if q.ValidFrom != "" && today < q.ValidFrom {
		return ""
	}
	if q.ValidTo == "" || today <= q.ValidTo {
		return QualValid
	}
	return QualExpired
}

// Compose 合成根单元快照的 policy_view（PRD 第2段 F3“求值输入与未知传播”）：
//   - 每个我方主体都给出状态表（没有资质记录的主体给空表），声明来自档案；
//   - 同一主体同一资质代码有多条记录时，任一有效即有效，否则有过期记录即过期；
//   - INF-QUAL 未启用，核验状态均为“未核验”，不存在来源冲突，待核对集合为空；
//   - 未关联本企业时不出现 org_ids 与 entities；主体超过上界时同样省略（不截断），warn 为真需告警。
func Compose(p Profile, today Date) (view View, warn bool) {
	view = View{Categories: sorted(p.Categories), Regions: sorted(p.Regions), Brands: sorted(p.Brands)}
	if len(p.Entities) == 0 {
		return view, false
	}
	if len(p.Entities) > MaxSnapshotEntities {
		return view, true
	}
	view.Entities = map[string]EntityQualStatus{}
	for _, e := range p.Entities {
		view.OrgIDs = append(view.OrgIDs, e.OrgID)
		view.Entities[e.OrgID] = EntityQualStatus{Qualifications: map[string]string{}, Pending: []string{}, Complete: e.QualificationsComplete}
	}
	sort.Strings(view.OrgIDs)
	for _, q := range p.Qualifications {
		s := q.status(today)
		table, ok := view.Entities[q.OrgID]
		if s == "" || !ok {
			continue
		}
		if table.Qualifications[q.Code] != QualValid {
			table.Qualifications[q.Code] = s
		}
	}
	return view, false
}

// RefreshAfter 返回 today 之后最早一个使快照资质状态可能变化的日期（某条记录开始生效或过期的那天）；
// 没有这样的日期时返回空串。上下文版本在该日之后首次被读取时递增（快照不可变，时间引起的变化也形成新版本）。
func RefreshAfter(p Profile, today Date) Date {
	var next Date
	consider := func(d Date) {
		if d > today && (next == "" || d < next) {
			next = d
		}
	}
	for _, q := range p.Qualifications {
		if q.ValidFrom != "" {
			consider(q.ValidFrom)
		}
		if q.ValidTo != "" {
			consider(q.ValidTo.AddDays(1))
		}
	}
	return next
}

// SameView 比较两个快照视图是否相同。
func SameView(a, b View) bool { return reflect.DeepEqual(a, b) }

func sorted(values []string) []string {
	out := append([]string{}, values...)
	sort.Strings(out)
	return out
}
