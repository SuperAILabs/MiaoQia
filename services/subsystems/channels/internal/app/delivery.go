package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/domain"
)

// 推送到企业微信（PRD 第5段 F2、第9节）：每期一张总览卡加每区前 3 条、合计最多 6 张信号卡片。投递保存已渲染的消息，重试发送同一批
// 消息（卡片与令牌不变）；每次发送前按收件人当前状态再检查（成员有效、绑定仍为“已绑定”、各目标的 can_view_signals），失去访问的区
// 删除；失败退避重试到当日 18:00，之后停止渠道重试，简报留在 Web 与站内。

// 投递事件类型。
const (
	DeliverySent   = "channels.delivery.sent.v1"
	DeliveryFailed = "channels.delivery.failed.v1"
	ActionExecuted = "channels.action.executed.v1"
)

// ResendRetryWindow 是本人重新推送的渠道重试时限（从发起时起）。
const ResendRetryWindow = 2 * time.Hour

// deliveryMessage 是投递中的一条消息：Objective 为信号卡片所属的目标（总览卡为空）；Sent 为已被渠道接受；Dropped 为发送前再检查时
// 因收件人失去访问而删除。
type deliveryMessage struct {
	Objective string      `json:"objective,omitempty"`
	Message   api.Message `json:"message"`
	// 信号卡片的渲染来源（区标题与条目快照）：每次发送前按当前来源权利收紧后重新渲染正文，按钮、令牌与链接不变
	Title   string      `json:"title,omitempty"`
	Item    *SignalItem `json:"item,omitempty"`
	Sent    bool        `json:"sent,omitempty"`
	Dropped bool        `json:"dropped,omitempty"`
}

type tokenRow struct {
	id, recipient, signal, objective, action, reason, card, target string
	content                                                        int64
	expires                                                        time.Time
}

type linkRow struct {
	id, digest, recipient, kind, target string
	expires                             time.Time
}

// deliveryPlan 是待写入的一次投递：消息、动作令牌与链接（与简报或重新推送在同一事务中写入）。
type deliveryPlan struct {
	id, kind, recipient, binding, corp, user string
	messages                                 []deliveryMessage
	tokens                                   []tokenRow
	links                                    []linkRow
	cutoff, created                          time.Time
}

func (p *deliveryPlan) insert(ctx context.Context, tx pgx.Tx, tenant, briefing string) error {
	raw, err := json.Marshal(p.messages)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO chn_deliveries (id, tenant_id, briefing_id, recipient, kind, binding_id, corp_id, channel_user_id, state, messages, sections,
 next_attempt_at, cutoff_at, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', $9, '[]', $10, $11, $10)`,
		p.id, tenant, briefing, p.recipient, p.kind, p.binding, p.corp, p.user, raw, p.created, p.cutoff); err != nil {
		return err
	}
	if err := insertTokens(ctx, tx, tenant, p.id, p.tokens, p.created); err != nil {
		return err
	}
	return insertLinks(ctx, tx, tenant, p.links, p.created)
}

func insertTokens(ctx context.Context, tx pgx.Tx, tenant, delivery string, tokens []tokenRow, at time.Time) error {
	var dlv *string
	if delivery != "" {
		dlv = &delivery
	}
	for _, t := range tokens {
		var reason, target *string
		if t.reason != "" {
			reason = &t.reason
		}
		if t.target != "" {
			target = &t.target
		}
		if _, err := tx.Exec(ctx, `INSERT INTO chn_action_tokens (id, tenant_id, recipient, signal_id, objective_id, action, reason, content_version, target_token,
 card_id, delivery_id, expires_at, state, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'open', $13)`,
			t.id, tenant, t.recipient, t.signal, t.objective, t.action, reason, t.content, target, t.card, dlv, t.expires, at); err != nil {
			return err
		}
	}
	return nil
}

func insertLinks(ctx context.Context, tx pgx.Tx, tenant string, links []linkRow, at time.Time) error {
	for _, l := range links {
		if _, err := tx.Exec(ctx, `INSERT INTO chn_briefing_links (id, tenant_id, token_digest, recipient, kind, target, expires_at, created_at)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, l.id, tenant, l.digest, l.recipient, l.kind, l.target, l.expires, at); err != nil {
			return err
		}
	}
	return nil
}

// minter 签发一次渲染所需的动作令牌与链接。
type minter struct {
	s         *Service
	tenant    string
	recipient string
	now       time.Time
	tokens    []tokenRow
	links     []linkRow
	pursue    map[string]bool // 目标 → 收件人此刻能在该目标上创建跟进（租户已开启跟进入口且有 can_assign）
}

// link 生成一条只指向对象的链接（7 天有效），返回打开它的地址。
func (m *minter) link(kind, target string) (string, error) {
	token, digest, err := newToken()
	if err != nil {
		return "", err
	}
	m.links = append(m.links, linkRow{id: ids.New("lnk"), digest: digest, recipient: m.recipient, kind: kind, target: target, expires: m.now.Add(domain.BriefingLinkTTL)})
	return "/briefing-links/open?link=" + token, nil
}

// token 签发一个动作令牌（令牌标识即操作标识）。
func (m *minter) token(signal, objective, action, reason, card, target string, content int64, ttl time.Duration) (string, error) {
	t := tokenRow{id: ids.New("atk"), recipient: m.recipient, signal: signal, objective: objective, action: action, reason: reason, card: card,
		target: target, content: content, expires: m.now.Add(ttl)}
	sealed, err := m.s.seal(purposeAction, actionClaims{ID: t.id, Tenant: m.tenant, Recipient: t.recipient, Signal: signal, Action: action, Reason: reason,
		Content: content, Card: card, Expires: t.expires})
	if err != nil {
		return "", err
	}
	m.tokens = append(m.tokens, t)
	return sealed, nil
}

// signalCard 渲染一张信号卡片（F3）：字段按优先级、按渠道能力截断；有 can_feedback 时带“相关”与“不相关 · 原因”按钮（前 5 个原因），
// “其他”与“查看”是链接。
func (m *minter) signalCard(code, title, objective string, it *SignalItem, feedback bool, prefs domain.Preferences) (api.Message, error) {
	caps := m.s.provider().Capabilities()
	card, err := newCardID()
	if err != nil {
		return api.Message{}, err
	}
	tenantTZ := tenantLocation()
	lines, _ := domain.FitCard(cardLines(title, it, false, prefs.Location(), tenantTZ), caps.LineMax, caps.LinesMax)
	view, err := m.link("signal", it.SignalID)
	if err != nil {
		return api.Message{}, err
	}
	var buttons []api.Button
	if feedback {
		tok, err := m.token(it.SignalID, objective, "relevant", "", card, "", it.ContentVersion, domain.ActionTokenTTL)
		if err != nil {
			return api.Message{}, err
		}
		buttons = append(buttons, api.Button{Key: "relevant", Label: "相关", Token: tok})
		for _, r := range m.s.Packs.Quick[code] {
			tok, err := m.token(it.SignalID, objective, "irrelevant", r.Code, card, "", it.ContentVersion, domain.ActionTokenTTL)
			if err != nil {
				return api.Message{}, err
			}
			buttons = append(buttons, api.Button{Key: "irrelevant:" + r.Code, Label: "不相关 · " + r.Say, Token: tok})
		}
		buttons = append(buttons, api.Button{Key: "irrelevant:other", Label: "不相关 · 其他", URL: view + "&feedback=irrelevant"})
	}
	// “跟进”（PRD 第6段 F1 第1条）：排除信号不能创建跟进，不显示
	if m.pursue[objective] && it.Disposition != "excluded" {
		tok, err := m.token(it.SignalID, objective, "pursue", "", card, "", it.ContentVersion, domain.ActionTokenTTL)
		if err != nil {
			return api.Message{}, err
		}
		buttons = append(buttons, api.Button{Key: "pursue", Label: "跟进", Token: tok})
	}
	buttons = append(buttons, api.Button{Key: "view", Label: "查看", URL: view})
	return api.Message{Kind: "signal_card", Card: &api.Card{CardID: card, Title: domain.Truncate(title, caps.TitleMax), Lines: lines, Buttons: buttons}}, nil
}

// pursueGrants 判断收件人此刻能在哪些目标上创建跟进：租户已开启跟进入口（发布开关）、pursuit 的渠道内动作可用，且收件人本人在目标上
// 有 can_assign（customer-context 的当前口径）。判断失败的目标不显示按钮（失败关闭）；点击时 pursuit 仍按点击者当前的权限再判断。
func (s *Service) pursueGrants(sctx context.Context, tenant, subject string, objectives []string) map[string]bool {
	out := map[string]bool{}
	if s.Pursuits == nil || !s.PursueTenants[tenant] || s.Access == nil {
		return out
	}
	rc, _ := reqctx.From(sctx)
	uctx := userContext(sctx, tenant, rc.Cell, subject)
	for _, o := range objectives {
		granted, err := s.Access.Access(uctx, o, []string{"can_view_signals", "can_assign"})
		out[o] = err == nil && slices.Contains(granted, "can_assign")
	}
	return out
}

// tenantLocation 是租户时区（M2 的租户时区固定为缺省值 Asia/Shanghai）。
func tenantLocation() *time.Location {
	loc, err := time.LoadLocation(domain.DefaultTimezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// overview 渲染简报总览卡（F2 第1条）：日期与周期、各区条目数与前 3 条标题、数据截至时间、本期提示与“查看完整简报”。
func overview(doc *briefingDoc, secs []section, url string, prefs domain.Preferences, caps api.Capabilities) api.Message {
	loc, tenantTZ := prefs.Location(), tenantLocation()
	title := "妙洽简报 " + doc.Period.End.In(loc).Format("01-02")
	if doc.Delayed {
		title += "（延迟发送，原定 " + doc.Period.End.In(loc).Format("15:04") + "）"
	}
	lines := []string{
		"周期 " + clock(doc.Period.Start, loc, tenantTZ, "01-02 15:04") + " – " + clock(doc.Period.End, loc, tenantTZ, "01-02 15:04"),
		"内容截至 " + clock(doc.AssembledAt, loc, tenantTZ, "15:04"),
	}
	if len(secs) == 0 {
		lines = append(lines, "今日没有可显示的目标区")
	}
	for _, sec := range secs {
		if sec.Kind == "pending" {
			if sec.State == "unavailable" {
				lines = append(lines, "待我处理：暂不可用")
			} else {
				lines = append(lines, fmt.Sprintf("待我处理：%d 项（去 Web 处理）", len(sec.Items)))
			}
			continue
		}
		head := sec.Title + "："
		switch sec.State {
		case "objective_inactive":
			lines = append(lines, head+"目标未激活")
			continue
		case "unavailable":
			lines = append(lines, head+"暂不可用，下一期补齐")
			continue
		}
		head += fmt.Sprintf("新增 %d 条、更新 %d 条", sec.NewCount, sec.UpdatedCount)
		if sec.HandledCount > 0 {
			head += fmt.Sprintf("，其中 %d 条已由团队处理", sec.HandledCount)
		}
		if sec.State == "empty" {
			head += "（无新信号）"
		}
		lines = append(lines, head)
		if sec.IncludesPrevious {
			lines = append(lines, "  含上一期未能生成的部分")
		}
		shown := 0
		for _, it := range sec.Items {
			if shown == domain.CardsPerSection || it.SignalItem == nil {
				break
			}
			text, _ := fieldText("", it.Project, "未能可靠识别")
			text = strings.TrimPrefix(text, "：")
			if it.Kind == "reference" {
				text += "（也命中 " + it.References + "，见上方）"
			}
			lines = append(lines, "  · "+domain.Truncate(text, max(caps.LineMax, 8)))
			shown++
		}
		if sec.OverflowCount > 0 {
			lines = append(lines, fmt.Sprintf("  另有 %d 条，在收件箱查看", sec.OverflowCount))
		}
		if sec.BackfillCount > 0 {
			lines = append(lines, fmt.Sprintf("  回填产生 %d 条，在收件箱查看", sec.BackfillCount))
		}
		for _, d := range sec.DataAsOf {
			switch d.State {
			case "interrupted":
				lines = append(lines, "  "+d.Domain+" 来源中断，数据截至 "+clock(*d.AsOf, loc, tenantTZ, "01-02 15:04")+"；恢复后补齐的信号进入下一期")
			case "current":
				lines = append(lines, "  "+d.Domain+" 数据截至 "+clock(*d.AsOf, loc, tenantTZ, "01-02 15:04"))
			default:
				lines = append(lines, "  "+d.Domain+" 数据截至时间未知")
			}
		}
	}
	return api.Message{Kind: "briefing_overview", Card: &api.Card{CardID: "overview-" + doc.ID, Title: title, Lines: lines,
		Buttons: []api.Button{{Key: "open", Label: "查看完整简报", URL: url}}}}
}

// planDelivery 渲染一次投递的消息并签发令牌与链接（不写库）。
func (s *Service) planDelivery(sctx context.Context, tenant, subject string, doc *briefingDoc, feedback map[string]bool, prefs domain.Preferences,
	target *api.DeliveryTarget, kind string) (*deliveryPlan, error) {
	if target == nil || target.Channel != Channel {
		return nil, failure(409, "not_bound")
	}
	var corp, user string
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT corp_id, channel_user_id FROM chn_bindings WHERE id = $1 AND state = 'bound'`, target.BindingID).Scan(&corp, &user)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, failure(409, "not_bound")
	}
	if err != nil {
		return nil, err
	}
	now := s.businessNow()
	m := &minter{s: s, tenant: tenant, recipient: subject, now: now}
	var objectives []string
	for _, sec := range doc.Sections {
		if sec.Kind == "objective" && sec.ObjectiveSection != nil {
			objectives = append(objectives, sec.ObjectiveID)
		}
	}
	m.pursue = s.pursueGrants(sctx, tenant, subject, objectives)
	url, err := m.link("briefing", doc.ID)
	if err != nil {
		return nil, err
	}
	plan := &deliveryPlan{id: ids.New("dlv"), kind: kind, recipient: subject, binding: target.BindingID, corp: corp, user: user,
		cutoff: domain.DeliveryCutoff(doc.Period.End, prefs.Location()), created: now}
	if kind == "resend" {
		// 本人发起的重新推送：从发起时起重试 ResendRetryWindow，不受当日 18:00 截止限制
		plan.cutoff = now.Add(ResendRetryWindow)
	}
	plan.messages = append(plan.messages, deliveryMessage{Message: overview(doc, doc.Sections, url, prefs, s.provider().Capabilities())})
	cards := 0
	for _, sec := range doc.Sections {
		if sec.Kind != "objective" || (sec.State != "normal" && sec.State != "source_interrupted") {
			continue
		}
		n := 0
		for _, it := range sec.Items {
			if it.Kind != "signal" || n == domain.CardsPerSection || cards == domain.CardsPerBriefing {
				continue
			}
			msg, err := m.signalCard(sec.ObjectiveCode, sec.Title, sec.ObjectiveID, it.SignalItem, feedback[sec.ObjectiveID], prefs)
			if err != nil {
				return nil, err
			}
			plan.messages = append(plan.messages, deliveryMessage{Objective: sec.ObjectiveID, Message: msg, Title: sec.Title, Item: it.SignalItem})
			n++
			cards++
		}
	}
	plan.tokens, plan.links = m.tokens, m.links
	return plan, nil
}

// DeliverDue 是 Briefings.DeliverDue。
func (s *Service) DeliverDue(ctx context.Context, limit int) (int, error) {
	rc, err := requireService(ctx)
	if err != nil {
		return 0, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	sctx := serviceContext(ctx, rc.Tenant, rc.Cell)
	var due []string
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM chn_deliveries WHERE state IN ('pending', 'retrying') AND next_attempt_at <= $1 ORDER BY next_attempt_at, id LIMIT $2`,
			s.now(), limit)
		if err != nil {
			return err
		}
		due, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return 0, err
	}
	for _, id := range due {
		s.deliverOne(sctx, id)
	}
	return len(due), nil
}

type deliveryRow struct {
	id, briefing, recipient, kind, binding, corp, user string
	messages                                           []deliveryMessage
	attempts, dropped                                  int
	cutoff                                             time.Time
}

// deliverOne 尝试一次投递。先以租约认领（next_attempt_at 推后），并发的执行者只有一个取得；任何依赖不可用时按退避重排，不把未知当作
// 成员退出或绑定失效。
func (s *Service) deliverOne(sctx context.Context, id string) {
	rc, _ := requireService(sctx)
	now := s.now().UTC()
	var d deliveryRow
	claimed := false
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx, `UPDATE chn_deliveries SET next_attempt_at = $2 WHERE id = $1 AND state IN ('pending', 'retrying') AND next_attempt_at <= $3
 RETURNING briefing_id, recipient, kind, binding_id, corp_id, channel_user_id, messages, attempts, dropped_sections, cutoff_at`,
			id, now.Add(2*time.Minute), now).Scan(&d.briefing, &d.recipient, &d.kind, &d.binding, &d.corp, &d.user, &raw, &d.attempts, &d.dropped, &d.cutoff)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		d.id, claimed = id, true
		return json.Unmarshal(raw, &d.messages)
	})
	if err != nil || !claimed {
		if err != nil {
			s.logger().WarnContext(sctx, "投递认领失败", "delivery", id, "error", err.Error())
		}
		return
	}
	// 当日 18:00 之后停止渠道重试（第13节第11项）
	if !now.Before(d.cutoff) {
		s.finishDelivery(sctx, &d, "in_app", "cutoff")
		return
	}
	var prefs domain.Preferences
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		sched, err := s.schedule(ctx, tx, rc.Tenant, d.recipient)
		if err == nil {
			_, _, prefs, _ = sched.PeriodAt(now)
			if prefs.SendTime == "" {
				prefs = sched[len(sched)-1]
			}
		}
		return err
	})
	if err != nil {
		s.reschedule(sctx, &d, now.Add(time.Minute))
		return
	}
	// 安静时段内不推送，延后到结束（F2 第2条）；本人发起的重新推送不受安静时段限制
	if until := domain.QuietUntil(prefs, now); !until.IsZero() && d.kind != "resend" {
		s.reschedule(sctx, &d, until)
		return
	}
	// 发送前再检查：成员有效、绑定仍为“已绑定”且仍是投递时的绑定、各目标的 can_view_signals
	active, err := s.memberActive(sctx, d.recipient)
	if err != nil {
		s.reschedule(sctx, &d, now.Add(time.Minute))
		return
	}
	if !active {
		s.finishDelivery(sctx, &d, "cancelled", "member_inactive")
		return
	}
	target, err := s.DeliveryTarget(sctx, d.recipient)
	if err != nil {
		s.reschedule(sctx, &d, now.Add(time.Minute))
		return
	}
	if target.Channel != Channel || target.BindingID != d.binding {
		reason := "binding_inactive"
		if target.Reason == "channel_not_enabled" {
			reason = "channel_not_enabled"
		}
		s.finishDelivery(sctx, &d, "cancelled", reason)
		return
	}
	uctx := userContext(sctx, rc.Tenant, rc.Cell, d.recipient)
	lost := map[string]bool{}
	checked := map[string]bool{}
	var secs []section
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT sections FROM chn_briefings WHERE id = $1`, d.briefing).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &secs)
	})
	if err != nil {
		s.reschedule(sctx, &d, now.Add(time.Minute))
		return
	}
	for _, sec := range secs {
		if sec.ObjectiveSection == nil || checked[sec.ObjectiveID] {
			continue
		}
		checked[sec.ObjectiveID] = true
		visible, _, err := s.objectiveGrant(uctx, sec.ObjectiveID)
		if err != nil {
			s.reschedule(sctx, &d, now.Add(time.Minute))
			return
		}
		if !visible {
			lost[sec.ObjectiveID] = true
		}
	}
	var kept []section
	for _, sec := range secs {
		if sec.ObjectiveSection == nil || !lost[sec.ObjectiveID] {
			kept = append(kept, sec)
		}
	}
	d.dropped = len(lost)
	// 发送前按当前来源权利收紧（快照不固定展示权限，数据架构第12节）：尚未送达的总览卡按剩余的区重新渲染（链接不变），
	// 尚未送达的信号卡片按收紧后的条目重新渲染正文（按钮与令牌不变）；权利不可得时按 none（失败关闭）
	s.restrictSections(sctx, kept)
	var cardSignals []string
	for _, m := range d.messages {
		if m.Item != nil && !m.Sent {
			cardSignals = append(cardSignals, m.Item.SignalID)
		}
	}
	rights := s.fieldRights(sctx, uniq(cardSignals))
	caps := s.provider().Capabilities()
	for i := range d.messages {
		m := &d.messages[i]
		if m.Sent || m.Dropped {
			continue
		}
		switch {
		case m.Objective != "" && lost[m.Objective]:
			m.Dropped = true
		case m.Objective != "" && m.Message.Card != nil:
			if m.Item == nil {
				m.Dropped = true // 没有渲染来源，无法按当前权利重新渲染：不发送
				continue
			}
			it := restrictItem(m.Item, rights[m.Item.SignalID])
			m.Message.Card.Lines, _ = domain.FitCard(cardLines(m.Title, it, false, prefs.Location(), tenantLocation()), caps.LineMax, caps.LinesMax)
		case m.Objective == "" && m.Message.Card != nil && len(m.Message.Card.Buttons) > 0:
			var doc briefingDoc
			if err := s.briefingHeader(sctx, d.briefing, &doc); err != nil {
				s.reschedule(sctx, &d, now.Add(time.Minute))
				return
			}
			m.Message = overview(&doc, kept, m.Message.Card.Buttons[0].URL, prefs, caps)
		}
	}
	// 发送尚未送达的消息（重试发送同一条消息，卡片与令牌不变）
	failed := false
	for i := range d.messages {
		m := &d.messages[i]
		if m.Sent || m.Dropped {
			continue
		}
		if err := s.provider().Send(sctx, d.corp, d.user, m.Message); err != nil {
			failed = true
			break
		}
		m.Sent = true
	}
	d.attempts++
	if failed {
		s.recordAttempt(sctx, &d, now, "retrying", now.Add(domain.Backoff(d.attempts)))
		return
	}
	s.recordAttempt(sctx, &d, now, "delivered", time.Time{})
}

// briefingHeader 读取简报的周期与时间（重新渲染总览卡）。
func (s *Service) briefingHeader(sctx context.Context, id string, doc *briefingDoc) error {
	return s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		doc.ID = id
		return tx.QueryRow(ctx, `SELECT period_start, period_end, timezone, assembled_at, delayed FROM chn_briefings WHERE id = $1`, id).
			Scan(&doc.Period.Start, &doc.Period.End, &doc.Period.Timezone, &doc.AssembledAt, &doc.Delayed)
	})
}

func (s *Service) reschedule(sctx context.Context, d *deliveryRow, at time.Time) {
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE chn_deliveries SET next_attempt_at = $2 WHERE id = $1 AND state IN ('pending', 'retrying')`, d.id, at)
		return err
	})
	if err != nil {
		s.logger().WarnContext(sctx, "投递重排失败", "delivery", d.id, "error", err.Error())
	}
}

// recordAttempt 记录一次发送尝试：全部送达为 delivered（发布 channels.delivery.sent.v1），否则为 retrying（发布 final 为假的
// channels.delivery.failed.v1）。
func (s *Service) recordAttempt(sctx context.Context, d *deliveryRow, now time.Time, state string, next time.Time) {
	raw, _ := json.Marshal(d.messages)
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		if state == "delivered" {
			tag, err := tx.Exec(ctx, `UPDATE chn_deliveries SET state = 'delivered', messages = $2, attempts = $3, dropped_sections = $4, submitted_at = $5, accepted_at = $5
 WHERE id = $1 AND state IN ('pending', 'retrying')`, d.id, raw, d.attempts, d.dropped, now)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			sent := 0
			for _, m := range d.messages {
				if m.Sent {
					sent++
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE chn_briefings SET push_state = 'delivered', not_pushed_reason = NULL WHERE id = $1`, d.briefing); err != nil {
				return err
			}
			if _, err := writer.Append(ctx, tx, outbox.Event{Type: DeliverySent, Subject: d.id, PartitionKey: d.recipient, Data: map[string]any{
				"delivery_id": d.id, "briefing_id": d.briefing, "recipient": d.recipient, "channel": Channel, "kind": d.kind, "attempt": d.attempts,
				"messages": max(sent, 1), "dropped_sections": d.dropped, "accepted_at": now.Format(time.RFC3339Nano)}}); err != nil {
				return err
			}
			return recordAs(ctx, tx, "channels.delivery.delivered", d.id, "succeeded", fmt.Sprintf("wecom;attempt=%d;dropped=%d", d.attempts, d.dropped))
		}
		tag, err := tx.Exec(ctx, `UPDATE chn_deliveries SET state = 'retrying', reason = 'channel_error', messages = $2, attempts = $3, dropped_sections = $4,
 submitted_at = $5, next_attempt_at = $6 WHERE id = $1 AND state IN ('pending', 'retrying')`, d.id, raw, d.attempts, d.dropped, now, next)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = writer.Append(ctx, tx, outbox.Event{Type: DeliveryFailed, Subject: d.id, PartitionKey: d.recipient, Data: map[string]any{
			"delivery_id": d.id, "briefing_id": d.briefing, "recipient": d.recipient, "channel": Channel, "kind": d.kind, "attempt": d.attempts,
			"reason": "channel_error", "final": false}})
		return err
	})
	if err != nil {
		s.logger().WarnContext(sctx, "投递结果写入失败", "delivery", d.id, "error", err.Error())
	}
}

// finishDelivery 结束投递：in_app（截止后仍失败）或 cancelled（发送前再检查时取消）；发布 final 为真的 channels.delivery.failed.v1。
// 简报推送的投递结束时更新简报的推送状态（重新推送的不改变）。
func (s *Service) finishDelivery(sctx context.Context, d *deliveryRow, state, reason string) {
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE chn_deliveries SET state = $2, reason = $3 WHERE id = $1 AND state IN ('pending', 'retrying')`, d.id, state, reason)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if d.kind == "briefing" {
			push, np := "in_app", (*string)(nil)
			if state == "cancelled" {
				nb := "not_bound"
				push, np = "not_pushed", &nb
			}
			if _, err := tx.Exec(ctx, `UPDATE chn_briefings SET push_state = $2, not_pushed_reason = $3 WHERE id = $1`, d.briefing, push, np); err != nil {
				return err
			}
		}
		if _, err := writer.Append(ctx, tx, outbox.Event{Type: DeliveryFailed, Subject: d.id, PartitionKey: d.recipient, Data: map[string]any{
			"delivery_id": d.id, "briefing_id": d.briefing, "recipient": d.recipient, "channel": Channel, "kind": d.kind, "attempt": d.attempts,
			"reason": reason, "final": true}}); err != nil {
			return err
		}
		return recordAs(ctx, tx, "channels.delivery."+state, d.id, "failed", "wecom;"+reason)
	})
	if err != nil {
		s.logger().WarnContext(sctx, "投递结束写入失败", "delivery", d.id, "error", err.Error())
	}
}
