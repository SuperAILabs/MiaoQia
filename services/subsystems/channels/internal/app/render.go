package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/domain"
	detection "github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// 条目与卡片的渲染（PRD 第5段 F1 第11条、F3）。来源字段按展示级别逐维取最严格后白名单写入：full_text 原文、excerpt 截断后的摘录、
// citation_only 只有原文链接、none 两者都没有；权利不可得时失败关闭为 none。

// sourceFields 是来自来源的展示字段（命中理由、处置、计数与时间是平台派生，不受来源权利限制）。
var sourceFields = []string{"project", "winner", "amount", "region", "categories"}

// fieldRights 读取信号各展示字段的来源权利；未接入或失败时返回空表（全部按 none 处理）。
func (s *Service) fieldRights(ctx context.Context, signals []string) map[string]map[string]api.FieldRight {
	if s.Rights == nil || len(signals) == 0 {
		return map[string]map[string]api.FieldRight{}
	}
	out, err := s.Rights.FieldRights(ctx, signals)
	if err != nil || out == nil {
		s.logger().WarnContext(ctx, "来源权利不可得，来源字段按 none 展示", "error", fmt.Sprint(err))
		return map[string]map[string]api.FieldRight{}
	}
	return out
}

// field 按最终展示级别构造一个字段：只在 full_text / excerpt 时写入值，citation_only 只写原文链接。state 描述值本身
// （known / undisclosed / unknown），与展示级别无关。
func field(right api.FieldRight, state, value string) *displayField {
	level := domain.StrictestLevel(right.Levels)
	f := &displayField{Level: level, State: state}
	switch level {
	case "full_text":
		if state == "known" {
			f.Value = domain.Truncate(value, 500)
		}
	case "excerpt":
		if state == "known" {
			f.Value = domain.Truncate(value, domain.ExcerptMax)
		}
	case "citation_only":
		if strings.HasPrefix(right.SourceURL, "https://") || strings.HasPrefix(right.SourceURL, "http://") {
			f.SourceURL = right.SourceURL
		}
	}
	return f
}

// signalFields 由 detection 的信号行与权利构造条目的来源字段。
func signalFields(row detection.BriefingSignal, rights map[string]api.FieldRight) *SignalItem {
	sig := row.Signal
	it := &SignalItem{SignalID: sig.ID, ContentVersion: sig.ContentVersion, Category: string(row.Category), Correction: row.Correction,
		Score: sig.Score, Disposition: string(sig.Disposition)}
	it.Project = field(rights["project"], known(sig.Title != ""), sig.Title)
	winner, wstate := "", "unknown"
	if w, ok := sig.Winner.Get(); ok && w.Name.Or("") != "" {
		winner, wstate = w.Name.Or(""), "known"
	}
	it.Winner = field(rights["winner"], wstate, winner)
	amount, astate := "", "undisclosed"
	if a, ok := sig.Amount.Get(); ok && a.Value != "" {
		amount, astate = a.Value, "known"
		if a.Currency == "CNY" {
			amount = domain.FormatAmountCNY(a.Value)
		} else {
			amount = a.Value + " " + a.Currency
		}
	}
	it.Amount = field(rights["amount"], astate, amount)
	it.Region = field(rights["region"], known(sig.Location.Or("") != ""), sig.Location.Or(""))
	it.Categories = field(rights["categories"], known(len(sig.Categories) > 0), strings.Join(sig.Categories, "、"))
	reasons := sig.Reasons
	if len(reasons) > 2 {
		reasons = reasons[:2]
	}
	it.Reasons = append([]string{}, reasons...)
	unknown, lot := sig.UnknownCount, max(sig.SameLotCount, 1)
	created := sig.CreatedAt.UTC()
	it.UnknownCount, it.SameLotCount, it.DiscoveredAt = &unknown, &lot, &created
	for _, u := range row.Updates {
		it.Updates = append(it.Updates, itemUpdate{RecordedAt: u.RecordedAt.UTC(), ChangedFields: append([]string{}, u.ChangedFields...),
			DispositionBefore: string(u.DispositionBefore), DispositionAfter: string(u.DispositionAfter), ContentVersion: u.ContentVersion})
	}
	return it
}

func known(ok bool) string {
	if ok {
		return "known"
	}
	return "unknown"
}

var dispositionLabel = map[string]string{"pursue_candidate": "跟进候选", "review_candidate": "复核候选", "watch_signal": "观察", "excluded": "排除"}

// fieldText 是卡片上一个来源字段的文字：按展示级别，citation_only 只写“见原文”，none 写“见详情”；值未公布或未识别时写状态词。
func fieldText(label string, f *displayField, missing string) (string, bool) {
	if f == nil {
		return label + "：见详情", false
	}
	switch {
	case f.State != "known" && (f.Level == "full_text" || f.Level == "excerpt"):
		return label + "：" + missing, true
	case f.Value != "":
		return label + "：" + f.Value, false
	case f.SourceURL != "":
		return label + "：见原文 " + f.SourceURL, true
	}
	return label + "：见详情", false
}

// cardLines 渲染信号卡片的正文行（按优先级，F3 第1条）。
func cardLines(title string, it *SignalItem, overnight bool, tz *time.Location, tenantTZ *time.Location) []domain.CardLine {
	disp := dispositionLabel[it.Disposition]
	lines := []domain.CardLine{{Text: title + " · " + disp, Keep: true}}
	project, _ := fieldText("项目", it.Project, "未能可靠识别")
	lines = append(lines, domain.CardLine{Text: project, Priority: 1})
	winner, _ := fieldText("中标方", it.Winner, "未识别")
	lines = append(lines, domain.CardLine{Text: winner, Priority: 2})
	amount, _ := fieldText("中标金额", it.Amount, "未公布")
	amount = strings.Replace(amount, "中标金额：", "中标金额 ", 1)
	if it.SameLotCount != nil && *it.SameLotCount > 1 {
		amount += fmt.Sprintf("（同标包共 %d 家中标）", *it.SameLotCount)
	}
	lines = append(lines, domain.CardLine{Text: amount, Keep: true, Atomic: true})
	region, _ := fieldText("地区", it.Region, "未能可靠识别")
	cats, _ := fieldText("品类", it.Categories, "未能可靠识别")
	lines = append(lines, domain.CardLine{Text: region + " · " + cats, Priority: 5})
	for i, r := range it.Reasons {
		lines = append(lines, domain.CardLine{Text: "理由：" + r, Priority: 3 + i})
	}
	if it.UnknownCount != nil && *it.UnknownCount > 0 {
		lines = append(lines, domain.CardLine{Text: fmt.Sprintf("未知项 %d", *it.UnknownCount), Keep: true, Atomic: true})
	}
	if it.DiscoveredAt != nil {
		lines = append(lines, domain.CardLine{Text: "发现 " + clock(*it.DiscoveredAt, tz, tenantTZ, "01-02 15:04"), Atomic: true, Priority: 3})
	}
	var marks []string
	switch it.Category {
	case "new":
		marks = append(marks, "新")
	case "updated":
		marks = append(marks, "已更新")
	}
	if it.Correction {
		marks = append(marks, "更正")
	}
	if overnight {
		marks = append(marks, "夜间到达")
	}
	for _, m := range it.AlsoMatched {
		marks = append(marks, "也命中 "+m)
	}
	if len(marks) > 0 {
		// “夜间到达”说明这条为何没有单独推送（C-08），放不下时先于地区与品类、理由与发现时间保留
		priority := 6
		if overnight {
			priority = 2
		}
		lines = append(lines, domain.CardLine{Text: "标记：" + strings.Join(marks, "、"), Priority: priority})
	}
	return lines
}

// clock 以成员时区写时刻；成员时区与租户时区不同时同时写两者（F3 第4条）。
func clock(t time.Time, tz, tenantTZ *time.Location, layout string) string {
	out := t.In(tz).Format(layout) + " " + zoneLabel(tz)
	if tenantTZ != nil && tz.String() != tenantTZ.String() {
		out += "（" + t.In(tenantTZ).Format(layout) + " " + zoneLabel(tenantTZ) + "）"
	}
	return out
}

func zoneLabel(loc *time.Location) string {
	switch loc.String() {
	case "Asia/Shanghai":
		return "北京时间"
	case "Asia/Tokyo":
		return "东京时间"
	}
	return loc.String()
}

// newCardID 生成卡片标识（渠道更新卡片时引用）。
func newCardID() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "card-" + hex.EncodeToString(raw), nil
}

// 来源权利随时可能收紧（数据架构第12节）：保存的快照只固定内容，不固定展示权限。读取完整简报、重新推送与每次发送尚未送达的消息前，
// 按当前权利重新裁剪：每个字段取快照级别与当前级别中更严格者，citation_only 与 none 不保留原值；权利不可得时当前级别为 none（失败关闭）。

// restrictField 以当前权利收紧一个已保存的字段。
func restrictField(f *displayField, cur api.FieldRight) *displayField {
	if f == nil {
		return nil
	}
	level := domain.StrictestLevel([]string{f.Level, domain.StrictestLevel(cur.Levels)})
	out := &displayField{Level: level, State: f.State}
	switch level {
	case "full_text":
		out.Value = f.Value
	case "excerpt":
		if f.Value != "" {
			out.Value = domain.Truncate(f.Value, domain.ExcerptMax)
		}
	case "citation_only":
		out.SourceURL = f.SourceURL
		if strings.HasPrefix(cur.SourceURL, "https://") || strings.HasPrefix(cur.SourceURL, "http://") {
			out.SourceURL = cur.SourceURL
		}
	}
	return out
}

// restrictItem 返回按当前权利收紧后的条目副本（不修改快照本身）。
func restrictItem(it *SignalItem, rights map[string]api.FieldRight) *SignalItem {
	if it == nil {
		return nil
	}
	cp := *it
	cp.Project = restrictField(it.Project, rights["project"])
	cp.Winner = restrictField(it.Winner, rights["winner"])
	cp.Amount = restrictField(it.Amount, rights["amount"])
	cp.Region = restrictField(it.Region, rights["region"])
	cp.Categories = restrictField(it.Categories, rights["categories"])
	return &cp
}

// restrictSections 按当前权利收紧各区条目的来源字段（含引用行）。
func (s *Service) restrictSections(sctx context.Context, secs []section) {
	var ids []string
	for _, sec := range secs {
		for _, it := range sec.Items {
			if it.SignalItem != nil {
				ids = append(ids, it.SignalID)
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	rights := s.fieldRights(sctx, uniq(ids))
	for si := range secs {
		for ii := range secs[si].Items {
			it := &secs[si].Items[ii]
			if it.SignalItem != nil {
				it.SignalItem = restrictItem(it.SignalItem, rights[it.SignalID])
			}
		}
	}
}
