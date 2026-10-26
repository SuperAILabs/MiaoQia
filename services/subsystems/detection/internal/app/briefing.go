package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 简报取数与信号卡片（PRD 第5段 F1 第4–7条、F4、F8 第2条；detection.md 第5节）。两者只经内部端口供 channels 以收件人（或经有效绑定
// 核验的点击者）的用户主体调用，授权与收件箱相同：目标的 can_view_signals，按 customer-context 的当前口径判断。

const (
	briefingRowLimit = 200 // 一区一次取数返回的条目上限（配额与截断在 channels）
	cardLimit        = 50  // 一次读取的卡片数
)

// BriefingSignal 是简报中的一条信号（契约 BriefingSignal）。
type BriefingSignal struct {
	Signal     Row      `json:"signal"`
	Category   string   `json:"category"`
	Correction bool     `json:"correction"`
	Updates    []Update `json:"updates"`
}

// BriefingPage 是一区的取数结果（契约 BriefingSignalPage）。
type BriefingPage struct {
	ObjectiveID       string           `json:"objective_id"`
	From              time.Time        `json:"from"`
	To                time.Time        `json:"to"`
	Signals           []BriefingSignal `json:"signals"`
	PendingTotal      int              `json:"pending_total"`
	NewCount          int              `json:"new_count"`
	UpdatedCount      int              `json:"updated_count"`
	HandledCount      int              `json:"handled_count"`
	BackfillCount     int              `json:"backfill_count"`
	ViewerCanFeedback bool             `json:"viewer_can_feedback"`
}

// BriefingSignals 返回一个目标在覆盖窗口 [from, to) 内的简报条目（契约 listBriefingSignals）。
func (s *Service) BriefingSignals(ctx context.Context, objective string, from, to time.Time) (*BriefingPage, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	if !from.Before(to) || to.Sub(from) > 62*24*time.Hour {
		return nil, failure(400, "invalid_request")
	}
	canFeedback, err := s.objectiveAccess(ctx, objective)
	if err != nil {
		return nil, err
	}
	from, to = from.UTC(), to.UTC()
	out := &BriefingPage{ObjectiveID: objective, From: from, To: to, Signals: []BriefingSignal{}, ViewerCanFeedback: canFeedback}
	// 窗口内新发现（不含回填）或有实质更新的非排除信号
	inWindow := `s.objective_id = $2 AND s.disposition <> 'excluded' AND ((s.created_at >= $3 AND s.created_at < $4 AND NOT s.backfill)
 OR EXISTS (SELECT 1 FROM det_signal_updates u WHERE u.signal_id = s.id AND u.material AND u.recorded_at >= $3 AND u.recorded_at < $4))`
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// 计数不需要查看者（$1）：参数重新编号为 $1 目标、$2 起、$3 止
		counts := strings.NewReplacer("$2", "$1", "$3", "$2", "$4", "$3").Replace(`SELECT
 count(*) FILTER (WHERE s.created_at >= $3 AND s.created_at < $4 AND NOT s.backfill),
 count(*) FILTER (WHERE NOT (s.created_at >= $3 AND s.created_at < $4 AND NOT s.backfill)),
 count(*) FILTER (WHERE s.status NOT IN ('open','triaged')),
 count(*) FILTER (WHERE s.status IN ('open','triaged'))
 FROM det_signals s WHERE ` + inWindow)
		if err := tx.QueryRow(ctx, counts, objective, from, to).
			Scan(&out.NewCount, &out.UpdatedCount, &out.HandledCount, &out.PendingTotal); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM det_signals s WHERE s.objective_id = $1 AND s.backfill AND s.disposition <> 'excluded'
 AND s.created_at >= $2 AND s.created_at < $3`, objective, from, to).Scan(&out.BackfillCount); err != nil {
			return err
		}
		// 组装时待处理的才列出；更正类在前，其次排序分降序、同分按发现时间新到旧
		correction := `EXISTS (SELECT 1 FROM det_signal_updates u, unnest(u.changed_fields) f WHERE u.signal_id = s.id AND u.material
 AND u.recorded_at >= $3 AND u.recorded_at < $4 AND (lower(f) LIKE '%winner%' OR lower(f) LIKE '%amount%' OR lower(f) LIKE '%categor%'))`
		rows, err := tx.Query(ctx, `SELECT `+rowColumns+`, CASE WHEN `+correction+` THEN 1 ELSE 0 END`+rowFrom+` WHERE `+inWindow+
			` AND s.status IN ('open','triaged') ORDER BY CASE WHEN `+correction+` THEN 0 ELSE 1 END, s.score DESC, s.created_at DESC, s.id DESC`+
			fmt.Sprintf(` LIMIT %d`, briefingRowLimit), rc.Principal.ID, objective, from, to)
		if err != nil {
			return err
		}
		var list []BriefingSignal
		for rows.Next() {
			var flag int64
			r, err := s.scanRow(scanAppend{rows, &flag})
			if err != nil {
				rows.Close()
				return err
			}
			cat := "updated"
			if !r.Backfill && !r.CreatedAt.Before(from) && r.CreatedAt.Before(to) {
				cat = "new"
			}
			list = append(list, BriefingSignal{Signal: *r, Category: cat, Correction: flag == 1, Updates: []Update{}})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(list) == 0 {
			return nil
		}
		index := map[string]int{}
		ids := make([]string, len(list))
		for i, b := range list {
			index[b.Signal.ID], ids[i] = i, b.Signal.ID
		}
		urows, err := tx.Query(ctx, `SELECT signal_id, content_version, trigger, changed_fields, disposition_before, disposition_after, material, recorded_at
 FROM det_signal_updates WHERE signal_id = ANY($1) AND material AND recorded_at >= $2 AND recorded_at < $3 ORDER BY recorded_at, content_version`, ids, from, to)
		if err != nil {
			return err
		}
		defer urows.Close()
		for urows.Next() {
			var id string
			var u Update
			if err := urows.Scan(&id, &u.ContentVersion, &u.Trigger, &u.ChangedFields, &u.DispositionBefore, &u.DispositionAfter, &u.Material, &u.RecordedAt); err != nil {
				return err
			}
			if u.ChangedFields == nil {
				u.ChangedFields = []string{}
			}
			u.RecordedAt = u.RecordedAt.UTC()
			b := &list[index[id]]
			if len(b.Updates) < detailLimit {
				b.Updates = append(b.Updates, u)
			}
		}
		if err := urows.Err(); err != nil {
			return err
		}
		out.Signals = list
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StatusChange 是最近一次处理状态改变（契约 StatusChange）。
type StatusChange struct {
	Actor  string    `json:"actor"`
	Status string    `json:"status"`
	At     time.Time `json:"at"`
}

// Card 是一条信号卡片的当前值（契约 SignalCard）。
type Card struct {
	Signal            Row           `json:"signal"`
	LastStatusChange  *StatusChange `json:"last_status_change,omitempty"`
	ViewerCanFeedback bool          `json:"viewer_can_feedback"`
}

// CardList 是契约 SignalCardList。
type CardList struct {
	Cards []Card `json:"cards"`
}

// SignalCards 按标识读取调用者可见的信号的当前卡片（契约 listSignalCards）：不可见、不存在与其他租户的信号不出现。
// 授权逐目标判断，任何一个目标的判断失败都使整次失败（503），不退化为只返回部分。
func (s *Service) SignalCards(ctx context.Context, ids []string) (*CardList, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 || len(ids) > cardLimit {
		return nil, failure(400, "invalid_request")
	}
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	var rows []*Row
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q, err := tx.Query(ctx, `SELECT `+rowColumns+rowFrom+` WHERE s.id = ANY($2) ORDER BY s.id`, rc.Principal.ID, ids)
		if err != nil {
			return err
		}
		defer q.Close()
		for q.Next() {
			r, err := s.scanRow(q)
			if err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return q.Err()
	})
	if err != nil {
		return nil, err
	}
	type grant struct{ view, feedback bool }
	grants := map[string]grant{}
	for _, r := range rows {
		if _, done := grants[r.ObjectiveID]; done {
			continue
		}
		visible, granted, err := s.access(ctx, r.ObjectiveID, relViewSignals, relFeedback)
		if err != nil {
			return nil, err
		}
		grants[r.ObjectiveID] = grant{view: visible && has(granted, relViewSignals), feedback: visible && has(granted, relFeedback)}
	}
	out := &CardList{Cards: []Card{}}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, r := range rows {
			g := grants[r.ObjectiveID]
			if !g.view {
				continue
			}
			c := Card{Signal: *r, ViewerCanFeedback: g.feedback}
			var sc StatusChange
			err := tx.QueryRow(ctx, `SELECT actor, parts->'status'->>'after', occurred_at FROM det_triage_ops
 WHERE signal_id = $1 AND outcome = 'succeeded' AND parts->'status'->>'change' = 'changed' ORDER BY seq DESC LIMIT 1`, r.ID).Scan(&sc.Actor, &sc.Status, &sc.At)
			switch {
			case err == nil:
				sc.At = sc.At.UTC()
				c.LastStatusChange = &sc
			case err != pgx.ErrNoRows:
				return err
			}
			out.Cards = append(out.Cards, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
