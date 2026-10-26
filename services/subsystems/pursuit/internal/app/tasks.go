package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// 任务、截止时间与提醒（PRD 第6段 F5；第13节第6项）。截止保存所填的值、精度与时区；提醒在任务的截止版本下按种类各计划一次
// （持久化在 pur_reminders，进程重启不丢失），到时由 Reminders.RunDue 再检查后发布。只到日的截止在所填时区的前一日与当日 09:00
// 提醒（负责人时区没有所有者，取所填时区，见 PRD 附“已知设计缺口”第4项）；精确到分钟的提前 24 小时与到期时提醒；逾期次日 09:00
// 再提醒一次。计划时已过的提醒不再计划。

type plannedReminder struct {
	kind string
	at   time.Time
}

func reminderPlan(due *LocalTime, now time.Time) []plannedReminder {
	if due == nil {
		return nil
	}
	loc, err := time.LoadLocation(due.Timezone)
	if err != nil {
		return nil
	}
	nine := func(d time.Time) time.Time { return time.Date(d.Year(), d.Month(), d.Day(), 9, 0, 0, 0, loc).UTC() }
	var out []plannedReminder
	switch due.Precision {
	case "minute":
		at, err := time.ParseInLocation("2006-01-02T15:04", due.Value, loc)
		if err != nil {
			return nil
		}
		out = []plannedReminder{{"before_24h", at.Add(-24 * time.Hour).UTC()}, {"at_due", at.UTC()}, {"overdue_next_day_0900", nine(at.AddDate(0, 0, 1))}}
	case "day":
		d, err := time.ParseInLocation("2006-01-02", due.Value, loc)
		if err != nil {
			return nil
		}
		out = []plannedReminder{{"day_before_0900", nine(d.AddDate(0, 0, -1))}, {"due_day_0900", nine(d)}, {"overdue_next_day_0900", nine(d.AddDate(0, 0, 1))}}
	}
	kept := out[:0]
	for _, r := range out {
		if r.at.After(now) {
			kept = append(kept, r)
		}
	}
	return kept
}

func planReminders(ctx context.Context, tx pgx.Tx, tenant string, t *Task, now time.Time) error {
	for _, r := range reminderPlan(t.Due, now) {
		if _, err := tx.Exec(ctx, `INSERT INTO pur_reminders (id, tenant_id, task_id, pursuit_id, deadline_version, kind, fire_at, state)
 VALUES ($1,$2,$3,$4,$5,$6,$7,'planned') ON CONFLICT (task_id, deadline_version, kind) DO NOTHING`, ids.New("rmd"), tenant, t.ID, t.PursuitID, t.DeadlineVersion, r.kind, r.at); err != nil {
			return err
		}
	}
	return nil
}

// insertTask 写入一项任务与它的提醒（调用方已校验负责人与截止）。
func (s *Service) insertTask(ctx context.Context, tx pgx.Tx, rc reqctx.Context, p *Pursuit, in TaskInput, now time.Time) (*Task, error) {
	t := &Task{ID: ids.New("tsk"), PursuitID: p.ID, Title: in.Title, Assignee: in.Assignee, Note: in.Note, Status: "todo", Due: in.Due, Version: 1, DeadlineVersion: 1,
		CreatedAt: now}
	if t.Assignee == "" {
		t.Assignee = p.Owner
	}
	var value, precision, tz *string
	if in.Due != nil {
		at, err := in.Due.Instant()
		if err != nil {
			return nil, err
		}
		t.DueAt = &at
		value, precision, tz = &in.Due.Value, &in.Due.Precision, &in.Due.Timezone
	}
	if _, err := tx.Exec(ctx, `INSERT INTO pur_tasks (id, tenant_id, pursuit_id, title, assignee, due_value, due_precision, due_tz, due_at, note, status, version,
 deadline_version, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'todo',1,1,$11)`, t.ID, rc.Tenant, p.ID, t.Title, t.Assignee, value, precision, tz, t.DueAt,
		optional(t.Note), now); err != nil {
		return nil, err
	}
	if err := planReminders(ctx, tx, rc.Tenant, t, now); err != nil {
		return nil, err
	}
	return t, timeline(ctx, tx, rc.Tenant, p.ID, "task_created", rc.Principal.ID, t.Title, now)
}

// editRights 要求调用者能更新跟进：不能查看 404（任务或跟进），本地仍是负责人或协作者但 identity 尚未同步时 403 authorization_syncing，
// 其余 403 forbidden。
func editDenied(p *Pursuit, r Rights, s *Service) error {
	if r.Involved && s.authorization(p).State != "effective" {
		return failure(403, "authorization_syncing")
	}
	return failure(403, "forbidden")
}

// CreateTaskRequest 是契约 CreateTaskRequest。
type CreateTaskRequest struct {
	OperationID string    `json:"operation_id"`
	Expected    int64     `json:"expected_version"`
	Task        TaskInput `json:"task"`
}

// CreateTask 新增任务（契约 createTask）。
func (s *Service) CreateTask(ctx context.Context, id, key string, req CreateTaskRequest) (*CommandResult, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	fp := fingerprint("create_task", id, req)
	return s.run(ctx, rc, "create_task", key, fp, func() (*Fixed, error) {
		o := op{id: req.OperationID, fp: fp, action: "create_task", channel: "web", pursuit: id}
		if f, found, err := s.lookup(ctx, rc, o); found || err != nil {
			return f, err
		}
		p, r, err := s.access(ctx, rc, id)
		if err != nil {
			return s.fix(ctx, rc, o, err)
		}
		if !r.Edit {
			return s.fix(ctx, rc, o, editDenied(p, r, s))
		}
		if req.Task.Due != nil {
			if _, err := req.Task.Due.Instant(); err != nil {
				return s.fix(ctx, rc, o, err)
			}
		}
		return s.commit(ctx, rc, o, func(ctx context.Context, tx pgx.Tx) (*Fixed, any, error) {
			now := s.businessNow()
			p, err := loadPursuit(ctx, tx, id, true)
			if err != nil {
				return nil, nil, err
			}
			if p.State != "open" || p.Version != req.Expected {
				return nil, nil, conflict("conflict", p, nil)
			}
			if a := req.Task.Assignee; a != "" && !p.involves(a) {
				return nil, nil, failure(422, "invalid_assignee")
			}
			t, err := s.insertTask(ctx, tx, rc, p, req.Task, now)
			if err != nil {
				return nil, nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_pursuits SET version = version + 1, changed_by = $2, updated_at = $3 WHERE id = $1`, id, rc.Principal.ID, now); err != nil {
				return nil, nil, err
			}
			if err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.task.create", Resource: fmt.Sprintf("%s/%s/op:%s/version:%d", id, t.ID, o.id, p.Version+1),
				Result: "succeeded", Basis: relEdit}); err != nil {
				return nil, nil, err
			}
			return &Fixed{Result: "succeeded", PursuitID: id, TaskID: t.ID}, nil, nil
		})
	})
}

// taskAccess 读取任务与所属跟进并判断调用者的当前权限；不存在、他租户与无权查看同为 404 task_not_found。
func (s *Service) taskAccess(ctx context.Context, rc reqctx.Context, id string) (*Task, *Pursuit, Rights, error) {
	var t *Task
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		t, err = loadTask(ctx, tx, id, false)
		return err
	})
	if err != nil {
		return nil, nil, Rights{}, err
	}
	if t == nil {
		return nil, nil, Rights{}, failure(404, "task_not_found")
	}
	p, r, err := s.access(ctx, rc, t.PursuitID)
	var safe *api.Error
	if err != nil {
		if errors.As(err, &safe) && safe.Code == "pursuit_not_found" {
			return nil, nil, Rights{}, failure(404, "task_not_found")
		}
		return nil, nil, Rights{}, err
	}
	return t, p, r, nil
}

// UpdateTaskRequest 是契约 UpdateTaskRequest。
type UpdateTaskRequest struct {
	OperationID string     `json:"operation_id"`
	Expected    int64      `json:"expected_version"`
	Title       string     `json:"title,omitempty"`
	Due         *LocalTime `json:"due,omitempty"`
	ClearDue    bool       `json:"clear_due,omitempty"`
	Reason      string     `json:"reason,omitempty"`
}

func sameDue(a, b *LocalTime) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// UpdateTask 修改任务的标题或截止（契约 updateTask；F5 第5、7条）：截止改变时截止版本加一，取消旧提醒、按新时间重建。
func (s *Service) UpdateTask(ctx context.Context, id, key string, req UpdateTaskRequest) (*CommandResult, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	if req.Due != nil && req.ClearDue {
		return nil, failure(400, "invalid_request")
	}
	fp := fingerprint("update_task", id, req)
	return s.run(ctx, rc, "update_task", key, fp, func() (*Fixed, error) {
		o := op{id: req.OperationID, fp: fp, action: "update_task", channel: "web", task: id}
		if f, found, err := s.lookup(ctx, rc, o); found || err != nil {
			return f, err
		}
		_, p, r, err := s.taskAccess(ctx, rc, id)
		if err != nil {
			return s.fix(ctx, rc, o, err)
		}
		o.pursuit = p.ID
		if !r.Edit {
			return s.fix(ctx, rc, o, editDenied(p, r, s))
		}
		if req.Due != nil {
			if _, err := req.Due.Instant(); err != nil {
				return s.fix(ctx, rc, o, err)
			}
		}
		return s.commit(ctx, rc, o, func(ctx context.Context, tx pgx.Tx) (*Fixed, any, error) {
			now := s.businessNow()
			p, err := loadPursuit(ctx, tx, p.ID, true)
			if err != nil {
				return nil, nil, err
			}
			t, err := loadTask(ctx, tx, id, true)
			if err != nil {
				return nil, nil, err
			}
			if p.State != "open" || t.Status != "todo" {
				return nil, nil, conflict("task_closed", p, t)
			}
			title := t.Title
			if req.Title != "" {
				title = req.Title
			}
			due := t.Due
			if req.Due != nil {
				due = req.Due
			}
			if req.ClearDue {
				due = nil
			}
			dueChanged := !sameDue(due, t.Due)
			if title == t.Title && !dueChanged {
				return &Fixed{Result: "no_change", PursuitID: p.ID, TaskID: id}, nil, nil
			}
			if t.Version != req.Expected {
				return nil, nil, conflict("conflict", p, t)
			}
			dv := t.DeadlineVersion
			var value, precision, tz *string
			var dueAt *time.Time
			if due != nil {
				at, err := due.Instant()
				if err != nil {
					return nil, nil, err
				}
				dueAt, value, precision, tz = &at, &due.Value, &due.Precision, &due.Timezone
			}
			if dueChanged {
				dv++
				if _, err := tx.Exec(ctx, `UPDATE pur_reminders SET state = 'cancelled', cancelled_by = $2 WHERE task_id = $1 AND state = 'planned'`, id, o.id); err != nil {
					return nil, nil, err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_tasks SET title = $2, due_value = $3, due_precision = $4, due_tz = $5, due_at = $6, deadline_version = $7,
 version = version + 1 WHERE id = $1`, id, title, value, precision, tz, dueAt, dv); err != nil {
				return nil, nil, err
			}
			if dueChanged {
				nt := &Task{ID: id, PursuitID: p.ID, Due: due, DeadlineVersion: dv}
				if err := planReminders(ctx, tx, rc.Tenant, nt, now); err != nil {
					return nil, nil, err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_pursuits SET version = version + 1, changed_by = $2, updated_at = $3 WHERE id = $1`, p.ID, rc.Principal.ID, now); err != nil {
				return nil, nil, err
			}
			before, after := "无", "无"
			if t.Due != nil {
				before = t.Due.Value + " " + t.Due.Timezone
			}
			if due != nil {
				after = due.Value + " " + due.Timezone
			}
			detail := fmt.Sprintf("%s：截止 %s → %s", title, before, after)
			if req.Reason != "" {
				detail += "；原因：" + req.Reason
			}
			if err := timeline(ctx, tx, rc.Tenant, p.ID, "task_updated", rc.Principal.ID, detail, now); err != nil {
				return nil, nil, err
			}
			if err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.task.update", Resource: fmt.Sprintf("%s/op:%s/version:%d/deadline:%d->%d", id, o.id,
				t.Version+1, t.DeadlineVersion, dv), Result: "succeeded", Basis: relEdit}); err != nil {
				return nil, nil, err
			}
			return &Fixed{Result: "succeeded", PursuitID: p.ID, TaskID: id}, nil, nil
		})
	})
}

// CompleteRequest 是契约 CompleteTaskRequest。
type CompleteRequest struct {
	OperationID string `json:"operation_id"`
	Expected    int64  `json:"expected_version"`
	Channel     string `json:"channel,omitempty"`
}

// completion 是完成固定在操作上的前值（撤销完成时恢复）。
type completion struct {
	TaskVersion int64  `json:"task_version_after"`
	Channel     string `json:"channel"`
}

// CompleteTask 完成任务（契约 completeTask；F5 第6条）：完成取消未发送的提醒（记在本操作上，撤销完成时恢复）。
func (s *Service) CompleteTask(ctx context.Context, id, key string, req CompleteRequest) (*CommandResult, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	if req.Channel == "" {
		req.Channel = "web"
	}
	if req.Channel == "wecom" && !isInternal(ctx) {
		return nil, failure(403, "forbidden")
	}
	fp := fingerprint("complete_task", id, req)
	return s.run(ctx, rc, "complete_task", key, fp, func() (*Fixed, error) {
		o := op{id: req.OperationID, fp: fp, action: "complete_task", channel: req.Channel, task: id}
		if f, found, err := s.lookup(ctx, rc, o); found || err != nil {
			return f, err
		}
		_, p, r, err := s.taskAccess(ctx, rc, id)
		if err != nil {
			return s.fix(ctx, rc, o, err)
		}
		o.pursuit = p.ID
		if !r.Edit {
			return s.fix(ctx, rc, o, editDenied(p, r, s))
		}
		return s.commit(ctx, rc, o, func(ctx context.Context, tx pgx.Tx) (*Fixed, any, error) {
			now := s.businessNow()
			p, err := loadPursuit(ctx, tx, p.ID, true)
			if err != nil {
				return nil, nil, err
			}
			t, err := loadTask(ctx, tx, id, true)
			if err != nil {
				return nil, nil, err
			}
			if t.Status == "done" {
				return &Fixed{Result: "no_change", PursuitID: p.ID, TaskID: id}, nil, nil
			}
			if p.State != "open" || t.Status != "todo" {
				return nil, nil, conflict("task_closed", p, t)
			}
			if t.Version != req.Expected {
				return nil, nil, conflict("conflict", p, t)
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_tasks SET status = 'done', completed_at = $2, version = version + 1 WHERE id = $1`, id, now); err != nil {
				return nil, nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_reminders SET state = 'cancelled', cancelled_by = $2 WHERE task_id = $1 AND state = 'planned'`, id, o.id); err != nil {
				return nil, nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_pursuits SET version = version + 1, changed_by = $2, updated_at = $3 WHERE id = $1`, p.ID, rc.Principal.ID, now); err != nil {
				return nil, nil, err
			}
			if err := timeline(ctx, tx, rc.Tenant, p.ID, "task_completed", rc.Principal.ID, t.Title+"（"+req.Channel+"）", now); err != nil {
				return nil, nil, err
			}
			if err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.task.complete", Resource: fmt.Sprintf("%s/op:%s/version:%d", id, o.id, t.Version+1),
				Result: "succeeded", Basis: relEdit + ";" + req.Channel}); err != nil {
				return nil, nil, err
			}
			return &Fixed{Result: "succeeded", PursuitID: p.ID, TaskID: id}, completion{TaskVersion: t.Version + 1, Channel: req.Channel}, nil
		})
	})
}

// ReopenRequest 是契约 ReopenTaskRequest。
type ReopenRequest struct {
	OperationID string `json:"operation_id"`
	Target      string `json:"target_operation_id"`
	Channel     string `json:"channel,omitempty"`
}

// ReopenTask 撤销本人的一次完成（契约 reopenTask；F5 第6条、F11 第4条）：在撤销时限内（按完成的渠道：Web 10 秒、渠道 10 分钟）、
// 任务版本仍为完成后的版本时恢复为待办，并恢复由该完成取消的提醒（已过时的在下一次 RunDue 时只发一次）。
func (s *Service) ReopenTask(ctx context.Context, id, key string, req ReopenRequest) (*CommandResult, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	if req.Channel == "" {
		req.Channel = "web"
	}
	if req.Channel == "wecom" && !isInternal(ctx) {
		return nil, failure(403, "forbidden")
	}
	fp := fingerprint("reopen_task", id, req)
	return s.run(ctx, rc, "reopen_task", key, fp, func() (*Fixed, error) {
		o := op{id: req.OperationID, fp: fp, action: "reopen_task", channel: req.Channel, task: id}
		if f, found, err := s.lookup(ctx, rc, o); found || err != nil {
			return f, err
		}
		_, p, r, err := s.taskAccess(ctx, rc, id)
		if err != nil {
			return s.fix(ctx, rc, o, err)
		}
		o.pursuit = p.ID
		if !r.Edit {
			return s.fix(ctx, rc, o, editDenied(p, r, s))
		}
		return s.commit(ctx, rc, o, func(ctx context.Context, tx pgx.Tx) (*Fixed, any, error) {
			now := s.businessNow()
			var actor, action, outcome, task string
			var raw []byte
			var at time.Time
			err := tx.QueryRow(ctx, `SELECT actor, action, outcome, COALESCE(task_id, ''), parts, recorded_at FROM pur_operations WHERE operation_id = $1`, req.Target).
				Scan(&actor, &action, &outcome, &task, &raw, &at)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && (actor != rc.Principal.ID || action != "complete_task" || outcome != "succeeded" || task != id)) {
				return nil, nil, failure(404, "operation_not_found")
			}
			if err != nil {
				return nil, nil, err
			}
			var c completion
			if err := json.Unmarshal(raw, &c); err != nil {
				return nil, nil, err
			}
			window := ReopenWeb
			if c.Channel == "wecom" {
				window = ReopenChannel
			}
			p, err := loadPursuit(ctx, tx, p.ID, true)
			if err != nil {
				return nil, nil, err
			}
			t, err := loadTask(ctx, tx, id, true)
			if err != nil {
				return nil, nil, err
			}
			if now.Sub(at) > window {
				return nil, nil, conflict("undo_window_expired", p, t)
			}
			if p.State != "open" || t.Status != "done" || t.Version != c.TaskVersion {
				return nil, nil, conflict("conflict", p, t)
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_tasks SET status = 'todo', completed_at = NULL, version = version + 1 WHERE id = $1`, id); err != nil {
				return nil, nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_reminders SET state = 'planned', cancelled_by = NULL WHERE task_id = $1 AND cancelled_by = $2 AND state = 'cancelled'
 AND deadline_version = $3`, id, req.Target, t.DeadlineVersion); err != nil {
				return nil, nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_pursuits SET version = version + 1, changed_by = $2, updated_at = $3 WHERE id = $1`, p.ID, rc.Principal.ID, now); err != nil {
				return nil, nil, err
			}
			if err := timeline(ctx, tx, rc.Tenant, p.ID, "task_reopened", rc.Principal.ID, t.Title, now); err != nil {
				return nil, nil, err
			}
			if err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.task.reopen", Resource: fmt.Sprintf("%s/op:%s/undo:%s/version:%d", id, o.id, req.Target,
				t.Version+1), Result: "succeeded", Basis: relEdit + ";" + req.Channel}); err != nil {
				return nil, nil, err
			}
			return &Fixed{Result: "succeeded", PursuitID: p.ID, TaskID: id}, nil, nil
		})
	})
}
