package app

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	purapi "github.com/superailabs/miaoqia/services/contracts/apis/pursuit"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	chn "github.com/superailabs/miaoqia/services/contracts/apis/channels"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/domain"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// Web“今日简报”、完整简报、重新推送、简报链接与投递统计（PRD 第5段 F6、F8）。简报、投递与链接只属于一名成员：他人的、其他租户的与
// 不存在的简报同为 404 briefing_not_found；管理员只看到投递统计。

const historyPage = 30

// ListBriefings 列出本人的简报历史（签名游标）。
func (s *Service) ListBriefings(ctx context.Context, params chn.ListBriefingsParams) (*chn.BriefingList, error) {
	rc, err := s.member(ctx)
	if err != nil {
		return nil, err
	}
	limit := params.Limit.Or(historyPage)
	if limit < 1 || limit > historyPage {
		return nil, failure(400, "invalid_request")
	}
	var after *historyCursor
	if c, ok := params.Cursor.Get(); ok {
		var v historyCursor
		// 签名在任何读取之前核对；签发给其他租户或其他成员的游标同样无效
		if !s.unseal(purposeCursor, string(c), &v) || v.Tenant != rc.Tenant || v.Subject != rc.Principal.ID || v.ID == "" {
			return nil, failure(400, "invalid_cursor")
		}
		after = &v
	}
	out := &briefingListDoc{Briefings: []briefingSummary{}}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := `SELECT id, period_start, period_end, timezone, assembled_at, delayed, empty, partial, item_count, push_state, not_pushed_reason
 FROM chn_briefings WHERE tenant_id = $1 AND subject = $2`
		args := []any{rc.Tenant, rc.Principal.ID}
		if after != nil {
			q += ` AND (period_end, id) < ($3, $4)`
			args = append(args, after.End, after.ID)
		}
		q += ` ORDER BY period_end DESC, id DESC LIMIT ` + itoa(limit+1)
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b briefingSummary
			var np *string
			if err := rows.Scan(&b.ID, &b.Period.Start, &b.Period.End, &b.Period.Timezone, &b.AssembledAt, &b.Delayed, &b.Empty, &b.Partial, &b.ItemCount,
				&b.PushState, &np); err != nil {
				return err
			}
			b.Period.Start, b.Period.End, b.AssembledAt = b.Period.Start.UTC(), b.Period.End.UTC(), b.AssembledAt.UTC()
			b.ScheduledAt = b.Period.End
			if np != nil {
				b.NotPushedReason = *np
			}
			out.Briefings = append(out.Briefings, b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if len(out.Briefings) > limit {
		out.Briefings = out.Briefings[:limit]
		last := out.Briefings[limit-1]
		next, err := s.seal(purposeCursor, historyCursor{Tenant: rc.Tenant, Subject: rc.Principal.ID, End: last.Period.End, ID: last.ID})
		if err != nil {
			return nil, err
		}
		out.NextCursor = next
	}
	return convert[chn.BriefingList](out)
}

type briefingListDoc struct {
	Briefings  []briefingSummary `json:"briefings"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// ownBriefing 读取本人的一期简报；不属于本人的与不存在的同为 404。
func (s *Service) ownBriefing(ctx context.Context, rc reqctx.Context, id string) (*briefingDoc, string, error) {
	doc := &briefingDoc{ID: id, Deliveries: []deliverySummary{}}
	var raw []byte
	var push string
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT source, period_start, period_end, timezone, assembled_at, delayed, empty, partial, rule_version, preference_version, sections, push_state
 FROM chn_briefings WHERE id = $1 AND tenant_id = $2 AND subject = $3`, id, rc.Tenant, rc.Principal.ID).Scan(&doc.Source, &doc.Period.Start, &doc.Period.End,
			&doc.Period.Timezone, &doc.AssembledAt, &doc.Delayed, &doc.Empty, &doc.Partial, &doc.RuleVersion, &doc.PreferenceVersion, &raw, &push)
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(404, "briefing_not_found")
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, kind, state, attempts, reason, created_at, submitted_at, accepted_at FROM chn_deliveries
 WHERE tenant_id = $1 AND briefing_id = $2 AND recipient = $3 ORDER BY created_at, id LIMIT 32`, rc.Tenant, id, rc.Principal.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d := deliverySummary{BriefingID: id, ReadReceipts: "unsupported"}
			var reason *string
			if err := rows.Scan(&d.ID, &d.Kind, &d.State, &d.Attempts, &reason, &d.CreatedAt, &d.SubmittedAt, &d.AcceptedAt); err != nil {
				return err
			}
			if reason != nil && d.State != "pending" && d.State != "delivered" {
				d.Reason = *reason
			}
			d.CreatedAt = d.CreatedAt.UTC()
			doc.Deliveries = append(doc.Deliveries, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}
	doc.Period.Start, doc.Period.End, doc.AssembledAt = doc.Period.Start.UTC(), doc.Period.End.UTC(), doc.AssembledAt.UTC()
	doc.ScheduledAt, doc.ContentAsOf = doc.Period.End, doc.AssembledAt
	if err := json.Unmarshal(raw, &doc.Sections); err != nil {
		return nil, "", err
	}
	return doc, push, nil
}

// recheck 按调用者当前的权限再检查各目标区：失去 can_view_signals 的区只保留种类、顺序与 no_permission（不返回条目、标题与编号）。
// 返回各可见目标的 can_feedback。
func (s *Service) recheck(ctx context.Context, doc *briefingDoc) (map[string]bool, error) {
	feedback := map[string]bool{}
	for i := range doc.Sections {
		sec := &doc.Sections[i]
		if sec.ObjectiveSection == nil {
			continue
		}
		visible, fb, err := s.objectiveGrant(ctx, sec.ObjectiveID)
		if err != nil {
			return nil, err
		}
		if !visible {
			*sec = section{Kind: sec.Kind, Order: sec.Order, State: "no_permission", Items: []item{}}
			continue
		}
		feedback[sec.ObjectiveID] = fb
	}
	s.recheckTasks(ctx, doc.Sections)
	return feedback, nil
}

// recheckTasks 以调用者本人经 pursuit getTask 再检查“待我处理”中的任务（快照不固定权限）：已不能查看的整条移除；其余叠加当前状态
// （task_current）。pursuit 不可用时移除全部任务条目（失败关闭），区内不再有条目时本区为暂不可用。
func (s *Service) recheckTasks(uctx context.Context, secs []section) {
	now := s.businessNow()
	for i := range secs {
		sec := &secs[i]
		if sec.Kind != "pending" {
			continue
		}
		kept := sec.Items[:0:0]
		dropped := false
		for _, it := range sec.Items {
			if it.TaskItem == nil {
				kept = append(kept, it)
				continue
			}
			if s.Pursuits == nil {
				dropped = true
				continue
			}
			v, err := s.Pursuits.GetTask(uctx, it.TaskID)
			if err != nil {
				dropped = dropped || dependencyTransient(err)
				continue
			}
			st := &taskStatus{Status: "todo", StatusAsOf: now}
			switch {
			case v.PursuitState != purapi.PursuitStatusOpen:
				st.Status = "pursuit_closed"
			case v.Task.Status != purapi.TaskStatusTodo:
				st.Status = string(v.Task.Status)
			case !v.ViewerIsAssignee && !v.ViewerEscalated.Or(false):
				st.Status = "reassigned"
			}
			it.TaskCurrent = st
			kept = append(kept, it)
		}
		sec.Items = kept
		if dropped && len(kept) == 0 {
			sec.State = "unavailable"
		}
	}
}

// GetBriefing 返回本人的完整简报：条目为组装时的快照，当前处理状态经 detection 以调用者的权限读取并按“当前”叠加。
func (s *Service) GetBriefing(ctx context.Context, id string) (*chn.Briefing, error) {
	rc, err := s.member(ctx)
	if err != nil {
		return nil, err
	}
	doc, _, err := s.ownBriefing(ctx, rc, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.recheck(ctx, doc); err != nil {
		return nil, err
	}
	// 快照不固定展示权限：按当前来源权利收紧
	s.restrictSections(serviceContext(ctx, rc.Tenant, rc.Cell), doc.Sections)
	var ids []string
	for _, sec := range doc.Sections {
		for _, it := range sec.Items {
			if it.SignalItem != nil {
				ids = append(ids, it.SignalID)
			}
		}
	}
	now := s.businessNow()
	if len(ids) > 0 {
		if s.Detection == nil {
			return nil, failure(503, "detection_unavailable")
		}
		for start := 0; start < len(ids); start += 50 {
			chunk := ids[start:min(start+50, len(ids))]
			list, err := s.Detection.SignalCards(ctx, uniq(chunk))
			if err != nil {
				return nil, failure(503, "detection_unavailable")
			}
			for _, c := range list.Cards {
				st := &itemStatus{Status: string(c.Signal.Status), StatusAsOf: now}
				if ch, ok := c.LastStatusChange.Get(); ok {
					at := ch.At.UTC()
					st.HandledBy, st.HandledAt = ch.Actor, &at
				}
				cv := c.Signal.ContentVersion
				for si := range doc.Sections {
					for ii := range doc.Sections[si].Items {
						it := &doc.Sections[si].Items[ii]
						if it.SignalItem != nil && it.SignalID == c.Signal.ID {
							cp := *st
							cp.ContentUpdated = cv != it.ContentVersion
							it.Current = &cp
						}
					}
				}
			}
		}
	}
	doc.StatusAsOf = &now
	return convert[chn.Briefing](doc)
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// ResendBriefing 本人对当日一期发起新的投递（不重新组装，新卡片使用新的动作令牌）。
func (s *Service) ResendBriefing(ctx context.Context, id, key string) (*chn.DeliverySummary, error) {
	rc, err := s.member(ctx)
	if err != nil {
		return nil, err
	}
	if !commandKey.MatchString(key) {
		return nil, failure(400, "invalid_idempotency_key")
	}
	request := map[string]string{"briefing_id": id}
	// 同一键的重放先返回首次的投递，不再做任何检查与渲染
	var prior *deliverySummary
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var op, fp string
		var resp []byte
		err := tx.QueryRow(ctx, `SELECT operation, fingerprint, response FROM chn_commands WHERE tenant_id = $1 AND actor = $2 AND request_key = $3`,
			rc.Tenant, rc.Principal.ID, key).Scan(&op, &fp, &resp)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(request)
		sum := sha256.Sum256(raw)
		if op != "resendBriefing" || fp != hex.EncodeToString(sum[:]) {
			return failure(409, "idempotency_conflict")
		}
		prior = &deliverySummary{}
		return json.Unmarshal(resp, prior)
	})
	if err != nil {
		return nil, err
	}
	if prior != nil {
		return convert[chn.DeliverySummary](prior)
	}
	doc, _, err := s.ownBriefing(ctx, rc, id)
	if err != nil {
		return nil, err
	}
	var sched domain.Schedule
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		sched, err = s.schedule(ctx, tx, rc.Tenant, rc.Principal.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	prefs := sched[len(sched)-1]
	_, _, p, ok := sched.PeriodAt(doc.Period.End)
	if ok {
		prefs = p
	}
	loc := prefs.Location()
	now := s.now()
	if y1, d1 := doc.Period.End.In(loc).Year(), doc.Period.End.In(loc).YearDay(); y1 != now.In(loc).Year() || d1 != now.In(loc).YearDay() {
		return nil, failure(409, "resend_not_allowed")
	}
	sctx := serviceContext(ctx, rc.Tenant, rc.Cell)
	target, err := s.DeliveryTarget(sctx, rc.Principal.ID)
	if err != nil {
		return nil, err
	}
	if target.Channel != Channel {
		return nil, failure(409, "not_bound")
	}
	feedback, err := s.recheck(ctx, doc)
	if err != nil {
		return nil, err
	}
	s.restrictSections(sctx, doc.Sections)
	var kept []section
	for _, sec := range doc.Sections {
		if sec.State != "no_permission" {
			kept = append(kept, sec)
		}
	}
	doc.Sections = kept
	plan, err := s.planDelivery(sctx, rc.Tenant, rc.Principal.ID, doc, feedback, prefs, target, "resend")
	if err != nil {
		return nil, err
	}
	out := &deliverySummary{}
	fresh := false
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		fresh, err = command(ctx, tx, key, "resendBriefing", request, out, func() error {
			if err := plan.insert(ctx, tx, rc.Tenant, id); err != nil {
				return err
			}
			*out = deliverySummary{ID: plan.id, BriefingID: id, Kind: "resend", State: "pending", Attempts: 0, CreatedAt: plan.created.UTC(), ReadReceipts: "unsupported"}
			return recordAs(ctx, tx, "channels.briefing.resent", plan.id, "succeeded", "wecom;briefing="+id)
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if fresh {
		s.deliverOne(sctx, plan.id)
	}
	return convert[chn.DeliverySummary](out)
}

// OpenBriefingLink 以当前成员打开简报或卡片上的链接（F6）：不切换账号；不存在、其他租户、对象不存在与无权限是同一个 404。
func (s *Service) OpenBriefingLink(ctx context.Context, req *chn.OpenCardLinkRequest) (*chn.BriefingLinkTarget, error) {
	if req == nil || req.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, err := s.member(ctx)
	if err != nil {
		return nil, err
	}
	var recipient, kind, target string
	var expires time.Time
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT recipient, kind, target, expires_at FROM chn_briefing_links WHERE token_digest = $1`, tokenDigest(string(req.Token))).
			Scan(&recipient, &kind, &target, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(404, "content_unavailable")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if recipient != rc.Principal.ID {
		return nil, &api.Error{Status: 409, Code: "account_mismatch", Recipient: recipient}
	}
	if !s.now().Before(expires) {
		return nil, failure(410, "card_link_expired")
	}
	// 按对象所有者的当前口径授权
	switch kind {
	case "signal":
		if s.Detection == nil {
			return nil, failure(503, "detection_unavailable")
		}
		list, err := s.Detection.SignalCards(ctx, []string{target})
		if err != nil {
			if dependencyTransient(err) {
				return nil, failure(503, "detection_unavailable")
			}
			return nil, failure(404, "content_unavailable")
		}
		if len(list.Cards) == 0 {
			return nil, failure(404, "content_unavailable")
		}
	case "briefing":
		if _, _, err := s.ownBriefing(ctx, rc, target); err != nil {
			var safe *api.Error
			if errors.As(err, &safe) && safe.Status == 404 {
				return nil, failure(404, "content_unavailable")
			}
			return nil, err
		}
	case "approval":
		if s.Approvals == nil {
			return nil, failure(503, "identity_unavailable")
		}
		if _, err := s.Approvals.GetApproval(ctx, target); err != nil {
			var safe *identity.Error
			if errors.As(err, &safe) && safe.Status < 500 {
				return nil, failure(404, "content_unavailable")
			}
			return nil, failure(503, "identity_unavailable")
		}
	default:
		return nil, failure(404, "content_unavailable")
	}
	out := &chn.BriefingLinkTarget{Kind: chn.BriefingLinkTargetKind(kind), ID: target}
	if kind == "signal" {
		out.FromSignal = chn.NewOptBool(true)
	}
	return out, nil
}

// DeliveryStats 返回本租户的投递统计（需要 can_manage_members）：只有状态、原因与计数，不含条目、标题或收件人。C-11 追加渠道通知
// （notices）：按种类、状态与原因计数（每个收件人一条；转给决策者尚未展开的行不计），原因按状态白名单（noticeReasons，其余记 other），
// 不含收件人、跟进、任务或标题。只统计调用者所在租户（显式租户条件 + 行级安全）。
func (s *Service) DeliveryStats(ctx context.Context, params chn.GetDeliveryStatsParams) (*chn.DeliveryStats, error) {
	rc, err := s.member(ctx)
	if err != nil {
		return nil, err
	}
	ok, err := s.allowed(ctx, rc, "can_manage_members", "tenant:"+rc.Tenant)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, failure(403, "forbidden")
	}
	since := params.Since.Or(s.businessNow().Add(-7 * 24 * time.Hour)).UTC()
	type row struct {
		State  string `json:"state"`
		Reason string `json:"reason,omitempty"`
		Count  int    `json:"count"`
	}
	type noticeStat struct {
		Kind   string `json:"kind"`
		State  string `json:"state"`
		Reason string `json:"reason,omitempty"`
		Count  int    `json:"count"`
	}
	out := struct {
		Since   time.Time    `json:"since"`
		Rows    []row        `json:"deliveries"`
		Empty   int          `json:"empty_briefings"`
		Partial int          `json:"partial_briefings"`
		Notices []noticeStat `json:"notices"`
	}{Since: since, Rows: []row{}, Notices: []noticeStat{}}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT state, CASE WHEN state IN ('pending', 'delivered') THEN '' ELSE COALESCE(reason, '') END, count(*) FROM chn_deliveries WHERE tenant_id = $1 AND created_at >= $2
 GROUP BY 1, 2 ORDER BY 1, 2 LIMIT 32`, rc.Tenant, since)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.State, &r.Reason, &r.Count); err != nil {
				rows.Close()
				return err
			}
			out.Rows = append(out.Rows, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE empty), count(*) FILTER (WHERE partial) FROM chn_briefings WHERE tenant_id = $1 AND assembled_at >= $2`,
			rc.Tenant, since).Scan(&out.Empty, &out.Partial); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT kind, state, COALESCE(reason, ''), count(*) FROM chn_notices WHERE tenant_id = $1 AND created_at >= $2 AND recipient <> '*'
 AND state IN ('pending', 'retrying', 'delivered', 'in_app', 'cancelled') GROUP BY 1, 2, 3`, rc.Tenant, since)
		if err != nil {
			return err
		}
		merged := map[noticeStat]int{}
		for rows.Next() {
			var r noticeStat
			if err := rows.Scan(&r.Kind, &r.State, &r.Reason, &r.Count); err != nil {
				rows.Close()
				return err
			}
			key := noticeStat{Kind: r.Kind, State: r.State, Reason: noticeKindStatsReason(r.Kind, r.State, r.Reason)}
			merged[key] += r.Count
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for k, c := range merged {
			k.Count = c
			out.Notices = append(out.Notices, k)
		}
		slices.SortFunc(out.Notices, func(a, b noticeStat) int {
			return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.State, b.State), cmp.Compare(a.Reason, b.Reason))
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return convert[chn.DeliveryStats](out)
}

// noticeKindStatsReason 按通知种类与状态白名单投影统计的原因：即时通知（signal_immediate）用 signalNoticeReasons，其余同 noticeStatsReason。
func noticeKindStatsReason(kind, state, reason string) string {
	if kind != NoticeSignalImmediate {
		return noticeStatsReason(state, reason)
	}
	switch {
	case state == "pending" || state == "delivered":
		return ""
	case signalNoticeReasons[state][reason]:
		return reason
	default:
		return "other"
	}
}

// noticeStatsReason 按状态白名单投影通知统计的原因：pending 与 delivered 不带原因，白名单外的记为 other（不把未登记的文本交给调用者）。
func noticeStatsReason(state, reason string) string {
	switch {
	case state == "pending" || state == "delivered":
		return ""
	case noticeReasons[state][reason]:
		return reason
	default:
		return "other"
	}
}
