package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// 读取（PRD 第6段 F8）：每次读取都按调用者当前的权限判断；跟进只含标识、状态、本租户填写的内容与时间，不复制公共事实的字段
// （目标对象的展示由对象页按来源权利与展示级别给出），因此没有需要在快照中固定的展示级别。

// SignalLinkView 是契约 SignalLink。
type SignalLinkView struct {
	LinkID   string    `json:"link_id"`
	SignalID string    `json:"signal_id"`
	Seen     int64     `json:"seen_content_version"`
	Method   string    `json:"method"`
	State    string    `json:"state"`
	LinkedAt time.Time `json:"linked_at"`
}

// TimelineEntry 是契约 TimelineEntry。
type TimelineEntry struct {
	Kind       string    `json:"kind"`
	Actor      string    `json:"actor"`
	Detail     string    `json:"detail,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Source 是跟进的来源。
type Source struct {
	Kind     string `json:"kind"`
	SignalID string `json:"signal_id,omitempty"`
	Seen     *int64 `json:"seen_content_version,omitempty"`
	Channel  string `json:"channel"`
}

// Detail 是契约 Pursuit。
type Detail struct {
	ID              string           `json:"id"`
	ObjectiveID     string           `json:"objective_id"`
	OrgUnitID       string           `json:"org_unit_id"`
	Kind            string           `json:"kind"`
	Target          Target           `json:"target"`
	Source          Source           `json:"source"`
	Previous        string           `json:"previous_pursuit_id,omitempty"`
	State           string           `json:"state"`
	Owner           string           `json:"owner"`
	OwnerValid      bool             `json:"owner_valid"`
	Collaborators   []string         `json:"collaborators"`
	Note            string           `json:"note,omitempty"`
	CreatedBy       string           `json:"created_by"`
	CreatedAt       time.Time        `json:"created_at"`
	ClosedAt        *time.Time       `json:"closed_at,omitempty"`
	Version         int64            `json:"version"`
	Authorization   Authorization    `json:"authorization"`
	Tasks           []TaskView       `json:"tasks"`
	Links           []SignalLinkView `json:"links"`
	Outcomes        []OutcomeView    `json:"outcomes"`
	Timeline        []TimelineEntry  `json:"timeline"`
	ViewerCanEdit   bool             `json:"viewer_can_edit"`
	ViewerCanManage bool             `json:"viewer_can_manage_team"`
	ViewerCanVoid   bool             `json:"viewer_can_void"`
}

// GetPursuit 读取跟进详情（契约 getPursuit）。
func (s *Service) GetPursuit(ctx context.Context, id string) (*Detail, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	p, r, err := s.access(ctx, rc, id)
	if err != nil {
		return nil, err
	}
	d := &Detail{ID: p.ID, ObjectiveID: p.ObjectiveID, OrgUnitID: p.OrgUnitID, Kind: "deal", Target: p.Target, Previous: p.Previous, State: p.State,
		Owner: p.Owner, OwnerValid: p.OwnerValid, Collaborators: p.Collaborators, Note: p.Note, CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt,
		ClosedAt: p.ClosedAt, Version: p.Version, Authorization: s.authorization(p), Tasks: []TaskView{}, Links: []SignalLinkView{}, Outcomes: []OutcomeView{},
		Timeline: []TimelineEntry{}, ViewerCanEdit: r.Edit && p.State == "open", ViewerCanManage: r.Edit && p.State == "open"}
	d.Source = Source{Kind: p.SourceKind, SignalID: p.SourceSignal, Channel: p.Channel}
	if p.SourceKind == "signal" {
		v := p.SourceContent
		d.Source.Seen = &v
	}
	d.ViewerCanVoid = p.State == "open" && p.CreatedBy == rc.Principal.ID && p.Version == 1 && s.now().Sub(p.CreatedAt) <= VoidWindow
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tasks, err := tasksOf(ctx, tx, id)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			d.Tasks = append(d.Tasks, t.view(p))
		}
		rows, err := tx.Query(ctx, `SELECT id, signal_id, seen_content_version, method, state, linked_at FROM pur_signal_links WHERE pursuit_id = $1 ORDER BY linked_at, id LIMIT 200`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var l SignalLinkView
			if err := rows.Scan(&l.LinkID, &l.SignalID, &l.Seen, &l.Method, &l.State, &l.LinkedAt); err != nil {
				rows.Close()
				return err
			}
			d.Links = append(d.Links, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT content FROM pur_outcomes WHERE pursuit_id = $1 ORDER BY recorded_at, id LIMIT 100`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return err
			}
			var o OutcomeView
			if err := jsonUnmarshal(raw, &o); err != nil {
				rows.Close()
				return err
			}
			d.Outcomes = append(d.Outcomes, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// 时间线取最近 200 条，按平台记录时间排列
		rows, err = tx.Query(ctx, `SELECT kind, actor, COALESCE(detail, ''), recorded_at FROM (SELECT * FROM pur_timeline WHERE pursuit_id = $1 ORDER BY seq DESC LIMIT 200) t
 ORDER BY seq`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e TimelineEntry
			if err := rows.Scan(&e.Kind, &e.Actor, &e.Detail, &e.RecordedAt); err != nil {
				return err
			}
			e.Detail = truncate(e.Detail, 500)
			d.Timeline = append(d.Timeline, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// PursuitList 是契约 PursuitList。
type PursuitList struct {
	Items      []Summary `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// listCursor 是跟进列表的翻页位置（签名，绑定租户、调用者、视图与目标筛选）。
type listCursor struct {
	Tenant    string     `json:"t"`
	Subject   string     `json:"u"`
	View      string     `json:"v"`
	Objective string     `json:"o,omitempty"`
	Valid     bool       `json:"ov"`
	Due       time.Time  `json:"d"`
	Closed    *time.Time `json:"c,omitempty"`
	ID        string     `json:"i"`
}

var infinity = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// listScanCap 是一页最多检查的行数：权限过滤后不足一页时以已检查的最后一行为下一页起点，避免一次请求无界扫描。
const listScanCap = 500

// ListPursuits 列出调用者可查看的跟进（契约 listPursuits）。
func (s *Service) ListPursuits(ctx context.Context, view, objective string, limit int, cursor string) (*PursuitList, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	c := listCursor{Tenant: rc.Tenant, Subject: rc.Principal.ID, View: view, Objective: objective, Due: time.Time{}, ID: ""}
	if cursor != "" {
		var got listCursor
		if !s.unseal(purposeListCursor, cursor, &got) || got.Tenant != rc.Tenant || got.Subject != rc.Principal.ID || got.View != view || got.Objective != objective {
			return nil, failure(400, "invalid_cursor")
		}
		c = got
	}
	out := &PursuitList{Items: []Summary{}}
	cache := unitRights{}
	scanned := 0
	for {
		var batch []*Pursuit
		var keys []time.Time
		err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var rows pgx.Rows
			var err error
			filter := `TRUE`
			switch view {
			case "mine":
				filter = `owner = $2 AND owner_valid`
			case "collaborating":
				filter = `$2 = ANY(collaborators)`
			}
			if view == "closed" {
				after := infinity
				if c.Closed != nil {
					after = *c.Closed
				}
				rows, err = tx.Query(ctx, `SELECT `+pursuitColumns+`, closed_at FROM pur_pursuits WHERE state = 'closed' AND ($1 = '' OR objective_id = $1) AND $2 <> ''
 AND (closed_at, id) < ($3, $4) ORDER BY closed_at DESC, id DESC LIMIT 100`, objective, rc.Principal.ID, after, cursorID(c.ID, c.Closed == nil))
			} else {
				rows, err = tx.Query(ctx, `WITH c AS (SELECT p.id AS pid, COALESCE((SELECT min(t.due_at) FROM pur_tasks t WHERE t.pursuit_id = p.id AND t.status = 'todo'), $5::timestamptz) AS nd
 FROM pur_pursuits p WHERE p.state = 'open' AND ($1 = '' OR p.objective_id = $1) AND `+qualify(filter)+`)
 SELECT `+pursuitColumns+`, c.nd FROM pur_pursuits JOIN c ON c.pid = pur_pursuits.id WHERE (owner_valid, c.nd, id) > ($3, $4, $6) ORDER BY owner_valid, c.nd, id LIMIT 100`,
					objective, rc.Principal.ID, c.Valid, c.Due, infinity, c.ID)
			}
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var key time.Time
				var closed *time.Time
				var p *Pursuit
				if view == "closed" {
					p, err = scanPursuit(rows, &closed)
					if closed != nil {
						key = *closed
					}
				} else {
					p, err = scanPursuit(rows, &key)
				}
				if err != nil {
					return err
				}
				batch, keys = append(batch, p), append(keys, key)
			}
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
		for i, p := range batch {
			scanned++
			r, err := s.rights(ctx, rc, p, cache)
			if err != nil {
				return nil, err
			}
			if r.View {
				if len(out.Items) == limit {
					return s.listPage(ctx, out, c)
				}
				out.Items = append(out.Items, s.summary(p, nil))
			}
			c.ID, c.Valid = p.ID, p.OwnerValid
			if view == "closed" {
				k := keys[i]
				c.Closed = &k
			} else {
				c.Due = keys[i]
			}
			if scanned >= listScanCap {
				return s.listPage(ctx, out, c)
			}
		}
		if len(batch) < 100 {
			return s.listPage(ctx, out, listCursor{})
		}
	}
}

// cursorID 返回翻页比较用的标识：首页（没有位置）时取比任何标识都大的值。
func cursorID(id string, first bool) string {
	if first {
		return "~"
	}
	return id
}

// qualify 给视图筛选的列加上表别名（CTE 内的子查询）。
func qualify(filter string) string {
	switch filter {
	case `owner = $2 AND owner_valid`:
		return `p.owner = $2 AND p.owner_valid`
	case `$2 = ANY(collaborators)`:
		return `$2 = ANY(p.collaborators)`
	}
	return `$2 <> ''`
}

// listPage 补齐每项的下一个截止并签发下一页游标（c 为空时没有下一页）。
func (s *Service) listPage(ctx context.Context, out *PursuitList, c listCursor) (*PursuitList, error) {
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for i := range out.Items {
			t, err := nextTask(ctx, tx, out.Items[i].ID)
			if err != nil {
				return err
			}
			if t != nil {
				out.Items[i].NextDue, out.Items[i].NextDueAt = t.Due, t.DueAt
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if c.ID != "" {
		rc, _ := reqctx.From(ctx)
		c.Tenant, c.Subject = rc.Tenant, rc.Principal.ID
		next, err := s.seal(purposeListCursor, c)
		if err != nil {
			return nil, err
		}
		out.NextCursor = next
	}
	return out, nil
}

// MyTask 是契约 MyTask。
type MyTask struct {
	Task        TaskView `json:"task"`
	ObjectiveID string   `json:"objective_id"`
	Escalated   bool     `json:"escalated"`
}

// MyTaskList 是契约 MyTaskList。
type MyTaskList struct {
	Items      []MyTask `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

type tasksCursor struct {
	Tenant  string     `json:"t"`
	Subject string     `json:"u"`
	Before  *time.Time `json:"b,omitempty"`
	Due     time.Time  `json:"d"`
	ID      string     `json:"i"`
}

// ListMyTasks 列出待我处理的任务（契约 listMyTasks；F9 第1条）：分派给调用者、调用者当前能更新的待办任务；调用者在单元上能更新跟进
// （决策者）时另含该单元内无有效负责人的跟进上的任务（escalated）。
func (s *Service) ListMyTasks(ctx context.Context, before *time.Time, limit int, cursor string) (*MyTaskList, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	c := tasksCursor{Tenant: rc.Tenant, Subject: rc.Principal.ID, Before: before}
	if cursor != "" {
		var got tasksCursor
		same := func(a, b *time.Time) bool { return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b)) }
		if !s.unseal(purposeTasksCursor, cursor, &got) || got.Tenant != rc.Tenant || got.Subject != rc.Principal.ID || !same(got.Before, before) {
			return nil, failure(400, "invalid_cursor")
		}
		c = got
	}
	out := &MyTaskList{Items: []MyTask{}}
	cache := unitRights{}
	pursuits := map[string]*Pursuit{}
	scanned := 0
	for {
		type row struct {
			t   *Task
			key time.Time
		}
		var batch []row
		err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT `+prefixed("t.", taskColumns)+`, COALESCE(t.due_at, $3::timestamptz) FROM pur_tasks t JOIN pur_pursuits p ON p.id = t.pursuit_id
 WHERE t.status = 'todo' AND p.state = 'open' AND (t.assignee = $1 OR NOT p.owner_valid OR NOT (t.assignee = p.owner OR t.assignee = ANY(p.collaborators)))
 AND ($2::timestamptz IS NULL OR t.due_at < $2) AND (COALESCE(t.due_at, $3::timestamptz), t.id) > ($4, $5)
 ORDER BY COALESCE(t.due_at, $3::timestamptz), t.id LIMIT 100`, rc.Principal.ID, before, infinity, c.Due, c.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var r row
				var t Task
				var note, value, precision, tz *string
				if err := rows.Scan(&t.ID, &t.PursuitID, &t.Title, &t.Assignee, &note, &t.Status, &value, &precision, &tz, &t.DueAt, &t.Version, &t.DeadlineVersion,
					&t.CreatedAt, &t.CompletedAt, &r.key); err != nil {
					rows.Close()
					return err
				}
				if note != nil {
					t.Note = *note
				}
				if value != nil {
					t.Due = &LocalTime{Value: *value, Precision: *precision, Timezone: *tz}
				}
				r.t = &t
				batch = append(batch, r)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, r := range batch {
				if _, ok := pursuits[r.t.PursuitID]; ok {
					continue
				}
				p, err := loadPursuit(ctx, tx, r.t.PursuitID, false)
				if err != nil {
					return err
				}
				pursuits[r.t.PursuitID] = p
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		for _, r := range batch {
			scanned++
			p := pursuits[r.t.PursuitID]
			rights, err := s.rights(ctx, rc, p, cache)
			if err != nil {
				return nil, err
			}
			escalated := !p.involves(r.t.Assignee) && rights.UnitEdit
			mine := r.t.Assignee == rc.Principal.ID && p.involves(rc.Principal.ID) && rights.Edit
			if mine || escalated {
				if len(out.Items) == limit {
					return s.tasksPage(out, c)
				}
				out.Items = append(out.Items, MyTask{Task: r.t.view(p), ObjectiveID: p.ObjectiveID, Escalated: escalated && !mine})
			}
			c.Due, c.ID = r.key, r.t.ID
			if scanned >= listScanCap {
				return s.tasksPage(out, c)
			}
		}
		if len(batch) < 100 {
			return s.tasksPage(out, tasksCursor{})
		}
	}
}

func (s *Service) tasksPage(out *MyTaskList, c tasksCursor) (*MyTaskList, error) {
	if c.ID == "" {
		return out, nil
	}
	return out, s.sealTasks(out, c)
}

// GetTaskView 是契约 TaskView。
type GetTaskView struct {
	Task         TaskView `json:"task"`
	ObjectiveID  string   `json:"objective_id"`
	PursuitState string   `json:"pursuit_state"`
	ViewerEdit   bool     `json:"viewer_can_edit"`
	ViewerIs     bool     `json:"viewer_is_assignee"`
}

// GetTask 读取任务与调用者的当前权限（契约 getTask；提醒卡片发送前与点击后的再检查）。
func (s *Service) GetTask(ctx context.Context, id string) (*GetTaskView, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	t, p, r, err := s.taskAccess(ctx, rc, id)
	if err != nil {
		return nil, err
	}
	return &GetTaskView{Task: t.view(p), ObjectiveID: p.ObjectiveID, PursuitState: p.State, ViewerEdit: r.Edit && p.State == "open",
		ViewerIs: t.Assignee == rc.Principal.ID && p.involves(rc.Principal.ID)}, nil
}

func (s *Service) sealTasks(out *MyTaskList, c tasksCursor) error {
	next, err := s.seal(purposeTasksCursor, c)
	if err != nil {
		return err
	}
	out.NextCursor = next
	return nil
}

// prefixed 给逗号分隔的列名加上表别名。
func prefixed(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// truncate 按字符截断。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }
