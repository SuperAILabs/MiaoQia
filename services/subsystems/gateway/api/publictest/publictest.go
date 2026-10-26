// Package publictest 是对象页出站端口（gateway api.PublicData、api.EvidenceResolver 与 api.Entitlements）的内存实现，只用于测试与演示。
//
// 这里的对象、分区内容与证据全部是夹具数据（FIXTURE）：不是公共事实，不来自任何真实来源，名称、金额、主体与地区均为虚构，标识只借用
// 登记前缀的格式。不做任何外部调用。生产适配器（domain-intelligence、entity-graph、evidence 的读取接口与 commerce 权益）尚未接入。
//
// 口径：每个对象版本、合并事件与分区条目带记录时间；current 取全部，signal_time 只取记录时间不晚于记录截止时点的（之后的计入
// later_count）。证据按租户的权利返回展示级别，内容不裁剪（用于验证 BFF 按级别裁剪的防线）。
package publictest

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/gateway/api"
)

// ErrInjected 是测试注入的故障。
var ErrInjected = errors.New("publictest：注入的故障")

type version struct {
	at  time.Time
	obj api.Object
}

type merge struct {
	at         time.Time
	from, into string
	reverted   bool
	basis      string
}

type entry struct {
	at   time.Time
	item map[string]any
}

type status struct{ status, reason string }

// Store 是内存的对象、分区、证据与覆盖。
type Store struct {
	mu        sync.Mutex
	versions  map[string][]version
	merges    []merge
	entries   map[string][]entry
	statuses  map[string]status
	evidence  map[string]api.Evidence
	levels    map[string]string // 租户/证据 → 展示级别
	covered   map[string]map[string]bool
	fail      map[string]bool
	requests  map[string]int
	lastBasis api.Basis
	lastCats  []string
}

// New 返回空的夹具。
func New() *Store {
	return &Store{versions: map[string][]version{}, entries: map[string][]entry{}, statuses: map[string]status{}, evidence: map[string]api.Evidence{},
		levels: map[string]string{}, covered: map[string]map[string]bool{}, fail: map[string]bool{}, requests: map[string]int{}}
}

// PutObject 登记对象的一个版本（记录时间 at）。
func (s *Store) PutObject(at time.Time, obj api.Object) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.versions[obj.ID] = append(s.versions[obj.ID], version{at: at, obj: obj})
}

// Merge 登记组织 from 并入 into（记录时间 at）；Revert 登记撤销。
func (s *Store) Merge(at time.Time, from, into, basis string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.merges = append(s.merges, merge{at: at, from: from, into: into, basis: basis})
}

func (s *Store) Revert(at time.Time, from, into string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.merges = append(s.merges, merge{at: at, from: from, into: into, reverted: true})
}

// Add 登记分区条目（记录时间 at）。
func (s *Store) Add(object, section string, at time.Time, item map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[object+"/"+section] = append(s.entries[object+"/"+section], entry{at: at, item: item})
}

// SetStatus 固定分区的状态（例如 not_connected、unavailable）。
func (s *Store) SetStatus(object, section, st, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[object+"/"+section] = status{st, reason}
}

// PutEvidence 登记证据（内容完整，不裁剪）。
func (s *Store) PutEvidence(e api.Evidence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evidence[e.ID] = e
}

// SetLevel 设置租户对证据的展示级别（模拟来源权利与用途；缺省为证据登记时的级别）。
func (s *Store) SetLevel(tenant, evidence, level string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.levels[tenant+"/"+evidence] = level
}

// Cover 设置租户对对象的覆盖。
func (s *Store) Cover(tenant, object string, covered bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.covered[tenant] == nil {
		s.covered[tenant] = map[string]bool{}
	}
	s.covered[tenant][object] = covered
}

// Fail 注入端口故障：resolve、section、evidence 或 coverage。
func (s *Store) Fail(port string, fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[port] = fail
}

// Requests 返回端口被调用的次数。
func (s *Store) Requests(port string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[port]
}

// LastSection 返回最近一次分区请求的口径与品类筛选。
func (s *Store) LastSection() (api.Basis, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastBasis, append([]string(nil), s.lastCats...)
}

func visible(at time.Time, b api.Basis) bool {
	return b.View != api.ViewSignalTime || !at.After(b.RecordedCutoff)
}

// Resolve 实现 api.PublicData：旧标识按口径经合并历史解析（只取记录时间可见的合并与撤销），另给出当前口径下的合并或撤销。
func (s *Store) Resolve(_ context.Context, id string, b api.Basis) (*api.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests["resolve"]++
	if s.fail["resolve"] {
		return nil, ErrInjected
	}
	follow := func(basis api.Basis) (string, []api.Merge, *merge) {
		cur, path := id, []api.Merge{}
		var last *merge
		for range 8 {
			var into *merge
			for i := range s.merges {
				m := &s.merges[i]
				if m.from != cur || !visible(m.at, basis) {
					continue
				}
				if m.reverted {
					into = nil
				} else {
					into = m
				}
				last = m
			}
			if into == nil {
				break
			}
			path = append(path, api.Merge{State: "merged", OrgID: into.into, At: into.at, Basis: into.basis})
			cur = into.into
		}
		return cur, path, last
	}
	target, path, _ := follow(b)
	var obj *api.Object
	for _, v := range s.versions[target] {
		if visible(v.at, b) {
			o := v.obj
			obj = &o
		}
	}
	if obj == nil {
		return nil, api.ErrNotFound
	}
	obj.RequestedID, obj.MergedFrom = id, path
	if b.View == api.ViewSignalTime {
		if current, _, last := follow(api.Basis{View: api.ViewCurrent}); last != nil && (current != target || last.reverted) {
			state := "merged"
			if last.reverted {
				state = "reverted"
			}
			obj.CurrentMerge = &api.Merge{State: state, OrgID: current, At: last.at, Basis: last.basis}
		}
	}
	return obj, nil
}

// Section 实现 api.PublicData：按口径筛选条目，偏移游标分页，回带同一口径。
func (s *Store) Section(_ context.Context, req api.SectionRequest) (*api.SectionData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests["section"]++
	s.lastBasis, s.lastCats = req.Basis, append([]string(nil), req.Categories...)
	if s.fail["section"] || s.fail["section/"+req.Section] {
		return nil, ErrInjected
	}
	out := &api.SectionData{Basis: req.Basis, TimeAxis: "source_published", AsOf: time.Date(2026, 10, 17, 0, 40, 0, 0, time.UTC), Items: []map[string]any{}}
	if st, ok := s.statuses[req.Object.ID+"/"+req.Section]; ok {
		out.Status, out.ReasonCode = st.status, st.reason
		return out, nil
	}
	var all []map[string]any
	later := 0
	for _, e := range s.entries[req.Object.ID+"/"+req.Section] {
		if !visible(e.at, req.Basis) {
			later++
			continue
		}
		if len(req.Categories) > 0 {
			cat, _ := e.item["category"].(string)
			if !slices.Contains(req.Categories, cat) {
				continue
			}
		}
		all = append(all, e.item)
	}
	if req.Basis.View == api.ViewSignalTime {
		out.LaterCount = &later
	}
	offset, _ := strconv.Atoi(req.Cursor)
	if offset > len(all) {
		offset = len(all)
	}
	end := min(offset+req.Limit, len(all))
	out.Items = append(out.Items, all[offset:end]...)
	if end < len(all) {
		out.NextCursor = strconv.Itoa(end)
	}
	out.Status = "ok"
	if len(all) == 0 {
		out.Status, out.ReasonCode = "empty", "no_records_in_coverage"
	}
	return out, nil
}

// Resolver 是 api.EvidenceResolver 的夹具实现（Store 的另一视图）。
type Resolver struct{ *Store }

// Resolve 返回证据与请求上下文中租户的展示级别（内容不裁剪）。
func (r Resolver) Resolve(ctx context.Context, id, _ string) (*api.Evidence, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests["evidence"]++
	if r.fail["evidence"] {
		return nil, ErrInjected
	}
	e, ok := r.evidence[id]
	if !ok {
		return nil, api.ErrNotFound
	}
	rc, _ := reqctx.From(ctx)
	if lvl, ok := r.levels[rc.Tenant+"/"+id]; ok {
		e.DisplayLevel = lvl
	}
	e.Inputs = append([]api.EvidenceInput(nil), e.Inputs...)
	return &e, nil
}

// Covered 实现 api.Entitlements：未登记的租户与对象为不覆盖。
func (s *Store) Covered(ctx context.Context, obj api.Object) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests["coverage"]++
	if s.fail["coverage"] {
		return false, ErrInjected
	}
	rc, _ := reqctx.From(ctx)
	return s.covered[rc.Tenant][obj.ID], nil
}
