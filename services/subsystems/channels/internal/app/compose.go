package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/domain"
	customercontext "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	detection "github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// 简报组装（PRD 第5段 F1、F2 第3–4条、第9节）：按收件人本人的权限逐区取数，区与区不混排；每区的覆盖窗口从上一次成功取数的终点起，
// 只有取数成功并随简报保存后才前移；同一期只生成一份（幂等键：租户 + 成员 + 简报来源 + 周期起止）。

// DelayedAfter 是组装晚于计划发送时间多少算“延迟发送”。
const DelayedAfter = 10 * time.Minute

func recordAs(ctx context.Context, tx pgx.Tx, action, resource, result, basis string) error {
	return recorder.Record(ctx, tx, audit.Entry{Action: action, Resource: resource, Result: result, Basis: basis})
}

// requireService 要求调用者是带租户的服务主体（定时任务、工作进程与其他子系统）。
func requireService(ctx context.Context) (reqctx.Context, error) {
	rc, ok := reqctx.From(ctx)
	if !ok {
		return rc, reqctx.ErrMissing
	}
	if rc.Validate() != nil || rc.Tenant == "" || rc.Principal.Kind != "service" {
		return rc, failure(403, "forbidden")
	}
	return rc, nil
}

// memberActive 经 identity 判断成员当前是否有效；不可用时 503（失败关闭）。
func (s *Service) memberActive(sctx context.Context, subject string) (bool, error) {
	if s.Sessions == nil {
		return false, failure(503, "identity_unavailable")
	}
	m, err := s.Sessions.MemberLifecycle(sctx, subject)
	if err != nil || m == nil {
		return false, failure(503, "identity_unavailable")
	}
	return m.State == "active", nil
}

// activeObjectives 以 channels 服务主体读取本租户全部生效策略（目标、编号、状态与启用领域）。
func (s *Service) activeObjectives(sctx context.Context) ([]customercontext.ActivePolicyList, error) {
	if s.Policies == nil {
		return nil, failure(503, "customer_context_unavailable")
	}
	var pages []customercontext.ActivePolicyList
	cursor := ""
	for range 50 {
		page, err := s.Policies.ActivePolicies(sctx, 200, cursor)
		if err != nil || page == nil {
			return nil, failure(503, "customer_context_unavailable")
		}
		pages = append(pages, *page)
		next, ok := page.NextCursor.Get()
		if !ok || next == "" {
			return pages, nil
		}
		cursor = string(next)
	}
	return nil, failure(503, "customer_context_unavailable")
}

// composedSection 是组装中的一区与它的取数结果。
type composedSection struct {
	section
	page     *detection.BriefingSignalPage
	feedback bool
	domains  []string
}

// Compose 是 Briefings.Compose。
func (s *Service) Compose(ctx context.Context, subject string, at time.Time) (*api.ComposeResult, error) {
	rc, err := requireService(ctx)
	if err != nil {
		return nil, err
	}
	if subject == "" || s.Packs == nil || at.IsZero() {
		return nil, failure(400, "invalid_request")
	}
	at = at.UTC().Truncate(time.Second)
	sctx := serviceContext(ctx, rc.Tenant, rc.Cell)
	active, err := s.memberActive(sctx, subject)
	if err != nil {
		return nil, err
	}
	if !active {
		return &api.ComposeResult{Outcome: "skipped"}, nil
	}
	var sched domain.Schedule
	var existing string
	var start, end time.Time
	var prefs domain.Preferences
	ok := false
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if sched, err = s.schedule(ctx, tx, rc.Tenant, subject); err != nil {
			return err
		}
		if start, end, prefs, ok = sched.PeriodAt(at); !ok {
			return nil
		}
		err = tx.QueryRow(ctx, `SELECT id FROM chn_briefings WHERE tenant_id = $1 AND subject = $2 AND source = $3 AND period_start = $4 AND period_end = $5`,
			rc.Tenant, subject, s.Packs.Source, start, end).Scan(&existing)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if !ok {
		return &api.ComposeResult{Outcome: "skipped"}, nil
	}
	if existing != "" {
		return &api.ComposeResult{Outcome: "existing", BriefingID: existing}, nil
	}
	loc := prefs.Location()
	doc := &briefingDoc{ID: ids.New("brf"), Source: s.Packs.Source, Period: period{Start: start.UTC(), End: end.UTC(), Timezone: prefs.Timezone},
		AssembledAt: at, ScheduledAt: end.UTC(), Delayed: at.Sub(end) > DelayedAfter, RuleVersion: s.Packs.Source + "@" + s.Packs.Version,
		PreferenceVersion: prefs.Version, ContentAsOf: at, Sections: []section{}, Deliveries: []deliverySummary{}}
	// 当日 12:00 之后：不推送，各区窗口不前移（下一期覆盖本期，第13节第12项）
	if domain.ComposeLate(end, at, loc) {
		doc.Partial = true
		id, err := s.saveBriefing(sctx, rc, subject, doc, nil, "not_pushed", "late", nil)
		if err != nil {
			return nil, err
		}
		return &api.ComposeResult{Outcome: "late", BriefingID: id}, nil
	}
	uctx := userContext(ctx, rc.Tenant, rc.Cell, subject)
	sections, partial, audience, err := s.assemble(sctx, uctx, rc.Tenant, subject, start, end, prefs)
	if err != nil {
		return nil, err
	}
	if !audience {
		return &api.ComposeResult{Outcome: "skipped"}, nil
	}
	doc.Partial = partial
	items := 0
	doc.Empty = !partial
	for _, sec := range sections {
		doc.Sections = append(doc.Sections, sec.section)
		for _, it := range sec.Items {
			if it.Kind != "reference" {
				items++
			}
		}
		if sec.State == "unavailable" || sec.State == "source_interrupted" {
			doc.Empty = false
		}
	}
	if items > 0 {
		doc.Empty = false
	}
	// 推送决定：空简报默认不推送；没有有效绑定只在 Web 与站内
	pushState, reason := "pushing", ""
	var target *api.DeliveryTarget
	if doc.Empty && !prefs.PushEmpty {
		pushState, reason = "not_pushed", "empty"
	} else {
		if target, err = s.DeliveryTarget(sctx, subject); err != nil {
			return nil, err
		}
		if target.Channel != Channel {
			pushState, reason = "not_pushed", "not_bound"
		}
	}
	var plan *deliveryPlan
	if pushState == "pushing" {
		feedback := map[string]bool{}
		for _, sec := range sections {
			if sec.ObjectiveSection != nil {
				feedback[sec.ObjectiveID] = sec.feedback
			}
		}
		if plan, err = s.planDelivery(sctx, rc.Tenant, subject, doc, feedback, prefs, target, "briefing"); err != nil {
			return nil, err
		}
	}
	id, err := s.saveBriefing(sctx, rc, subject, doc, sections, pushState, reason, plan)
	if err != nil {
		return nil, err
	}
	out := &api.ComposeResult{Outcome: "composed", BriefingID: id}
	if id != doc.ID {
		return &api.ComposeResult{Outcome: "existing", BriefingID: id}, nil
	}
	if plan != nil {
		out.DeliveryID = plan.id
		s.deliverOne(sctx, plan.id)
	}
	return out, nil
}

// assemble 逐区取数。audience 为成员是否是本期的受众（至少一个已激活目标可看信号，或有待其处理的审批）。
func (s *Service) assemble(sctx, uctx context.Context, tenant, subject string, start, end time.Time, prefs domain.Preferences) ([]composedSection, bool, bool, error) {
	if s.Detection == nil {
		return nil, false, false, failure(503, "detection_unavailable")
	}
	pages, err := s.activeObjectives(sctx)
	if err != nil {
		return nil, false, false, err
	}
	windows := map[string]time.Time{}
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT objective_id, fetched_to FROM chn_briefing_windows WHERE tenant_id = $1 AND subject = $2`, tenant, subject)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var o string
			var t time.Time
			if err := rows.Scan(&o, &t); err != nil {
				return err
			}
			windows[o] = t
		}
		return rows.Err()
	})
	if err != nil {
		return nil, false, false, err
	}
	partial, audience := false, false
	var out []composedSection
	for _, page := range pages {
		for _, o := range page.Policies {
			visible, feedback, err := s.objectiveGrant(uctx, o.ObjectiveID)
			if err != nil {
				// 授权不可用：不列出该区（不泄露目标是否存在），窗口不前移，下一期补齐
				partial = true
				continue
			}
			if !visible {
				continue
			}
			code := o.ObjectiveCode
			title := code
			quota := 10
			if sec, ok := s.Packs.Sections[code]; ok {
				title, quota = sec.Title, sec.Quota.Default
			}
			if n, ok := prefs.SectionLimits[code]; ok {
				quota = n
			}
			cs := composedSection{section: section{Kind: "objective", Order: s.Packs.order(code), Items: []item{},
				ObjectiveSection: &ObjectiveSection{ObjectiveID: o.ObjectiveID, ObjectiveCode: code, Title: title, Quota: quota}}, feedback: feedback}
			for _, d := range o.EnabledDomains {
				cs.domains = append(cs.domains, string(d))
			}
			if o.ObjectiveState != "active" {
				cs.State = "objective_inactive"
				out = append(out, cs)
				continue
			}
			audience = true
			wstart := start
			if w, ok := windows[o.ObjectiveID]; ok && w.Before(start) {
				wstart = w
			}
			if end.Sub(wstart) > domain.MaxWindow {
				wstart = end.Add(-domain.MaxWindow)
			}
			ws, we := wstart.UTC(), end.UTC()
			cs.WindowStart, cs.WindowEnd, cs.IncludesPrevious = &ws, &we, wstart.Before(start)
			res, err := s.Detection.BriefingSignals(uctx, o.ObjectiveID, wstart, end)
			if err != nil {
				if !dependencyTransient(err) {
					continue // 取数时已失去访问：本期不列出该区
				}
				// 一区取数失败：暂不可用，窗口顺延到下一期（F1 第5条）
				cs.State, partial = "unavailable", true
				out = append(out, cs)
				continue
			}
			cs.page, cs.feedback = res, feedback && res.ViewerCanFeedback
			cs.NewCount, cs.UpdatedCount, cs.HandledCount, cs.BackfillCount = res.NewCount, res.UpdatedCount, res.HandledCount, res.BackfillCount
			cs.State = "normal"
			cs.DataAsOf = s.coverage(sctx, cs.domains)
			for _, d := range cs.DataAsOf {
				if d.State == "interrupted" {
					cs.State, partial = "source_interrupted", true
				}
			}
			out = append(out, cs)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].ObjectiveCode < out[j].ObjectiveCode
	})
	s.fillItems(sctx, out)
	// “待我处理”：待本人批准的审批（按提交时间）；任务随第6段
	if s.Approvals != nil {
		pending := composedSection{section: section{Kind: "pending", Order: 0, State: "normal", Items: []item{}}}
		list, err := s.Approvals.ListApprovals(uctx, identityapi.ListApprovalsParams{Role: identityapi.NewOptListApprovalsRole(identityapi.ListApprovalsRoleApprover),
			State: identityapi.NewOptApprovalState(identityapi.ApprovalStatePending), Limit: identityapi.NewOptInt(50)})
		switch {
		case err != nil:
			pending.State, partial = "unavailable", true
			out = append([]composedSection{pending}, out...)
		case len(list.Approvals) > 0:
			apps := list.Approvals
			sort.SliceStable(apps, func(i, j int) bool { return apps[i].SubmittedAt.Before(apps[j].SubmittedAt) })
			for i, a := range apps {
				pending.Items = append(pending.Items, item{Kind: "approval", Position: i,
					ApprovalItem: &ApprovalItem{ApprovalID: string(a.ID), ApprovalType: string(a.Type), SubmittedAt: a.SubmittedAt.UTC()}})
			}
			audience = true
			out = append([]composedSection{pending}, out...)
		}
	}
	for i := range out {
		if out[i].State == "normal" && len(out[i].Items) == 0 {
			out[i].State = "empty"
		}
	}
	return out, partial, audience, nil
}

// fillItems 按区顺序写入条目：配额（更正类不受配额并排在区首，detection 已按此排序）、跨区去重（同一采购项目只在靠前的区完整
// 展示，其后的区为引用行，靠前的区标“也命中”）、展示级别。配额、去重与截断只影响简报展示（F1 第9、10条）。
func (s *Service) fillItems(sctx context.Context, secs []composedSection) {
	var all []string
	for _, cs := range secs {
		if cs.page != nil {
			for _, b := range cs.page.Signals {
				all = append(all, b.Signal.ID)
			}
		}
	}
	rights := s.fieldRights(sctx, all)
	type fullRef struct{ sec, item int }
	seen := map[string]fullRef{}
	for si := range secs {
		cs := &secs[si]
		if cs.page == nil {
			continue
		}
		listed := 0
		for _, b := range cs.page.Signals {
			pid := b.Signal.ProcurementProjectID.Or("")
			if ref, ok := seen[pid]; ok && pid != "" && secs[ref.sec].ObjectiveID != cs.ObjectiveID {
				full := secs[ref.sec].Items[ref.item].SignalItem
				full.AlsoMatched = append(full.AlsoMatched, cs.ObjectiveCode)
				it := signalFields(b, rights[b.Signal.ID])
				line := &SignalItem{SignalID: it.SignalID, ContentVersion: it.ContentVersion, Category: it.Category, Correction: it.Correction,
					Score: it.Score, Disposition: it.Disposition, Project: it.Project, References: secs[ref.sec].ObjectiveCode}
				cs.Items = append(cs.Items, item{Kind: "reference", Position: len(cs.Items), SignalItem: line})
				continue
			}
			if !b.Correction && listed >= cs.Quota {
				cs.OverflowCount++
				continue
			}
			if !b.Correction {
				listed++
			}
			cs.Items = append(cs.Items, item{Kind: "signal", Position: len(cs.Items), SignalItem: signalFields(b, rights[b.Signal.ID])})
			if pid != "" {
				if _, ok := seen[pid]; !ok {
					seen[pid] = fullRef{si, len(cs.Items) - 1}
				}
			}
		}
		// 窗口内仍待处理、但超出 detection 单次上限的条目同样计入“另有 N 条”
		if extra := cs.page.PendingTotal - len(cs.page.Signals); extra > 0 {
			cs.OverflowCount += extra
		}
	}
}

// coverage 返回各信息领域的数据截至时间；覆盖信息不可得时各领域为 unknown（不以“正常”代替，不当作中断）。
func (s *Service) coverage(sctx context.Context, domains []string) []dataAsOf {
	out := []dataAsOf{}
	if len(domains) == 0 {
		return out
	}
	var cov []api.DomainCoverage
	var err error
	if s.Coverage == nil {
		err = api.ErrNotWired
	} else {
		cov, err = s.Coverage.Coverage(sctx, domains)
	}
	byDomain := map[string]api.DomainCoverage{}
	if err == nil {
		for _, c := range cov {
			byDomain[c.Domain] = c
		}
	}
	for _, d := range domains {
		c, ok := byDomain[d]
		switch {
		case !ok:
			out = append(out, dataAsOf{Domain: d, State: "unknown"})
		case c.Interrupted:
			at := c.AsOf.UTC()
			out = append(out, dataAsOf{Domain: d, State: "interrupted", AsOf: &at})
		default:
			at := c.AsOf.UTC()
			out = append(out, dataAsOf{Domain: d, State: "current", AsOf: &at})
		}
	}
	return out
}

// saveBriefing 在一个事务中保存简报、前移取数成功的区的窗口并创建投递（含令牌与链接）。同一期并发组装时只有一份生效：
// 返回已有的标识，本次的窗口与投递随事务回滚。
func (s *Service) saveBriefing(sctx context.Context, rc reqctx.Context, subject string, doc *briefingDoc, secs []composedSection, pushState, reason string,
	plan *deliveryPlan) (string, error) {
	raw, err := json.Marshal(doc.Sections)
	if err != nil {
		return "", err
	}
	items := 0
	for _, sec := range doc.Sections {
		for _, it := range sec.Items {
			if it.Kind != "reference" {
				items++
			}
		}
	}
	id := doc.ID
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "chn-briefing/"+rc.Tenant+"/"+subject); err != nil {
			return err
		}
		var prior string
		err := tx.QueryRow(ctx, `SELECT id FROM chn_briefings WHERE tenant_id = $1 AND subject = $2 AND source = $3 AND period_start = $4 AND period_end = $5`,
			rc.Tenant, subject, doc.Source, doc.Period.Start, doc.Period.End).Scan(&prior)
		if err == nil {
			id = prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var np *string
		if reason != "" {
			np = &reason
		}
		if _, err := tx.Exec(ctx, `INSERT INTO chn_briefings (id, tenant_id, subject, source, period_start, period_end, timezone, assembled_at, delayed, empty, partial,
 rule_version, preference_version, sections, item_count, push_state, not_pushed_reason) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
			doc.ID, rc.Tenant, subject, doc.Source, doc.Period.Start, doc.Period.End, doc.Period.Timezone, doc.AssembledAt, doc.Delayed, doc.Empty, doc.Partial,
			doc.RuleVersion, doc.PreferenceVersion, raw, items, pushState, np); err != nil {
			return err
		}
		// 只有取数成功（正常、空或来源中断）的区前移窗口
		for _, cs := range secs {
			if cs.page == nil || cs.WindowEnd == nil {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO chn_briefing_windows (tenant_id, subject, objective_id, fetched_to, briefing_id) VALUES ($1, $2, $3, $4, $5)
 ON CONFLICT (tenant_id, subject, objective_id) DO UPDATE SET fetched_to = GREATEST(chn_briefing_windows.fetched_to, EXCLUDED.fetched_to), briefing_id = EXCLUDED.briefing_id`,
				rc.Tenant, subject, cs.ObjectiveID, *cs.WindowEnd, doc.ID); err != nil {
				return err
			}
		}
		if plan != nil {
			if err := plan.insert(ctx, tx, rc.Tenant, doc.ID); err != nil {
				return err
			}
		}
		return recordAs(ctx, tx, "channels.briefing.composed", doc.ID, "succeeded", "briefing;"+pushState+";"+reason)
	})
	return id, err
}
