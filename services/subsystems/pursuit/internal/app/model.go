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
)

// LocalTime 是所填或原始的时间：值、精度（day / minute）与 IANA 时区（契约 LocalTime）。
type LocalTime struct {
	Value     string `json:"value"`
	Precision string `json:"precision"`
	Timezone  string `json:"timezone"`
}

// Instant 返回时点：minute 为所填时区的本地时刻；day 为该日期在所填时区的次日 00:00（“当日结束前”）。精度与取值格式不一致、
// 时区不是有效的 IANA 名称时 422 invalid_time。
func (t *LocalTime) Instant() (time.Time, error) {
	if t == nil || t.Timezone == "" || t.Timezone == "Local" {
		return time.Time{}, failure(422, "invalid_time")
	}
	loc, err := time.LoadLocation(t.Timezone)
	if err != nil {
		return time.Time{}, failure(422, "invalid_time")
	}
	switch t.Precision {
	case "day":
		d, err := time.ParseInLocation("2006-01-02", t.Value, loc)
		if err != nil {
			return time.Time{}, failure(422, "invalid_time")
		}
		return d.AddDate(0, 0, 1).UTC(), nil
	case "minute":
		d, err := time.ParseInLocation("2006-01-02T15:04", t.Value, loc)
		if err != nil {
			return time.Time{}, failure(422, "invalid_time")
		}
		return d.UTC(), nil
	}
	return time.Time{}, failure(422, "invalid_time")
}

// Target 是跟进的目标对象（契约 Target）。
type Target struct {
	FactType    string `json:"fact_type"`
	SubjectID   string `json:"subject_id"`
	PartID      string `json:"part_id,omitempty"`
	WinnerOrgID string `json:"winner_org_id,omitempty"`
}

// key 是唯一性的目标对象键：事实类型、主体、部分与中标方（PRD 第6段 F2 第1条）。
func (t Target) key() string {
	return strings.Join([]string{t.FactType, t.SubjectID, t.PartID, t.WinnerOrgID}, "|")
}

// Pursuit 是一个跟进的当前行。
type Pursuit struct {
	ID, TenantID, ObjectiveID, OrgUnitID, TemplateID string
	Target                                           Target
	SourceKind, SourceSignal, Channel, Previous      string
	SourceContent                                    int64
	SignalCreatedAt                                  *time.Time
	State, Owner                                     string
	OwnerValid                                       bool
	Collaborators                                    []string
	Note, CreatedBy, CreateOp, ChangedBy             string
	Version, Participation, Applied                  int64
	PendingSince                                     *time.Time
	NotifyVersion                                    *int64
	NotifyAssignee, NotifyBy, NotifyKind             string
	Result, CurrentOutcome                           string
	CreatedAt, UpdatedAt                             time.Time
	ClosedAt, VoidedAt                               *time.Time
	Grants                                           map[string]Grant
	// Kind 是形态：deal 交易型、continuous 持续型（持续跟踪，C-10）。交易型的接口对持续型的标识按不存在处理，反之亦然。
	Kind string
}

// Grant 是一个参与成员当前角色的授予记录：角色（owner / collaborator）、授予时的参与版本与授予时该成员在 identity 的成员版本。
type Grant struct {
	Role   string `json:"role"`
	PV     int64  `json:"pv"`
	Member int64  `json:"mv"`
}

// role 返回成员在本地期望关系中的当前角色（不参与为空）。
func (p *Pursuit) role(subject string) string {
	switch {
	case p.Owner == subject && p.OwnerValid:
		return "owner"
	case slices.Contains(p.Collaborators, subject):
		return "collaborator"
	}
	return ""
}

// granted 判断成员当前角色所在的参与版本是否已在 identity 应用（F12 第3条：新关系同步前不放行；identity 中旧版本的同名关系不算）。
func (p *Pursuit) granted(subject string) bool {
	g, ok := p.Grants[subject]
	return ok && g.Role == p.role(subject) && g.Role != "" && p.Applied >= g.PV
}

// regrant 按新的负责人与协作者计算授予记录：角色不变的成员保留原记录，新加入或角色改变的成员记为参与版本 pv，成员版本取 members
// （调用方须为这些成员重新判断并给出此刻的成员版本；缺少时记 0，任何退出事件都判无效，失败关闭）。
func regrant(old map[string]Grant, prevRole func(string) string, owner string, collaborators []string, pv int64, members map[string]int64) map[string]Grant {
	out := map[string]Grant{}
	put := func(subject, role string) {
		if g, ok := old[subject]; ok && g.Role == role && prevRole(subject) == role {
			out[subject] = g
			return
		}
		out[subject] = Grant{Role: role, PV: pv, Member: members[subject]}
	}
	put(owner, "owner")
	for _, c := range collaborators {
		put(c, "collaborator")
	}
	return out
}

// involves 判断成员是否在本地期望关系中：有效的负责人或协作者（失效的负责人不算，重新加入不恢复）。
func (p *Pursuit) involves(subject string) bool {
	return (p.Owner == subject && p.OwnerValid) || slices.Contains(p.Collaborators, subject)
}

// desired 返回授权同步的期望参与关系：撤销后清空；失效的负责人不同步（关闭后保留，参与人仍可查看已关闭的跟进）。
func (p *Pursuit) desired() (owner string, collaborators []string) {
	if p.State == "voided" {
		return "", nil
	}
	if p.OwnerValid {
		owner = p.Owner
	}
	return owner, p.Collaborators
}

const pursuitColumns = `id, tenant_id, objective_id, org_unit_id, template_id, fact_type, subject_id, part_id, winner_org_id, source_kind, source_signal_id,
 source_content_version, source_signal_created_at, channel, previous_pursuit_id, state, owner, owner_valid, collaborators, note, created_by, create_operation_id,
 changed_by, version, participation_version, applied_version, sync_pending_since, notify_version, notify_assignee, notify_by, notify_kind, result,
 current_outcome_id, created_at, updated_at, closed_at, voided_at, grants, kind`

func scanPursuit(row pgx.Row, extra ...any) (*Pursuit, error) {
	var p Pursuit
	var part, winner, signal, prev, note, nAssignee, nBy, nKind, result, outcome *string
	var content *int64
	var grants []byte
	dest := []any{&p.ID, &p.TenantID, &p.ObjectiveID, &p.OrgUnitID, &p.TemplateID, &p.Target.FactType, &p.Target.SubjectID, &part, &winner, &p.SourceKind,
		&signal, &content, &p.SignalCreatedAt, &p.Channel, &prev, &p.State, &p.Owner, &p.OwnerValid, &p.Collaborators, &note, &p.CreatedBy, &p.CreateOp,
		&p.ChangedBy, &p.Version, &p.Participation, &p.Applied, &p.PendingSince, &p.NotifyVersion, &nAssignee, &nBy, &nKind, &result, &outcome,
		&p.CreatedAt, &p.UpdatedAt, &p.ClosedAt, &p.VoidedAt, &grants, &p.Kind}
	err := row.Scan(append(dest, extra...)...)
	if err != nil {
		return nil, err
	}
	str := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	p.Target.PartID, p.Target.WinnerOrgID, p.SourceSignal, p.Previous, p.Note = str(part), str(winner), str(signal), str(prev), str(note)
	p.NotifyAssignee, p.NotifyBy, p.NotifyKind, p.Result, p.CurrentOutcome = str(nAssignee), str(nBy), str(nKind), str(result), str(outcome)
	if content != nil {
		p.SourceContent = *content
	}
	if p.Collaborators == nil {
		p.Collaborators = []string{}
	}
	p.Grants = map[string]Grant{}
	if len(grants) > 0 {
		if err := json.Unmarshal(grants, &p.Grants); err != nil {
			return nil, err
		}
	}
	return &p, nil
}

// loadPursuit 读取跟进（lock 为真时加行锁）；不存在（含他租户，行级安全）返回 nil。
func loadPursuit(ctx context.Context, tx pgx.Tx, id string, lock bool) (*Pursuit, error) {
	q := `SELECT ` + pursuitColumns + ` FROM pur_pursuits WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	p, err := scanPursuit(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// Task 是一项任务的当前行。
type Task struct {
	ID, PursuitID, Title, Assignee, Note, Status string
	Due                                          *LocalTime
	DueAt                                        *time.Time
	Version, DeadlineVersion                     int64
	CreatedAt                                    time.Time
	CompletedAt                                  *time.Time
}

const taskColumns = `id, pursuit_id, title, assignee, note, status, due_value, due_precision, due_tz, due_at, version, deadline_version, created_at, completed_at`

func scanTask(row pgx.Row) (*Task, error) {
	var t Task
	var note, value, precision, tz *string
	if err := row.Scan(&t.ID, &t.PursuitID, &t.Title, &t.Assignee, &note, &t.Status, &value, &precision, &tz, &t.DueAt, &t.Version, &t.DeadlineVersion,
		&t.CreatedAt, &t.CompletedAt); err != nil {
		return nil, err
	}
	if note != nil {
		t.Note = *note
	}
	if value != nil {
		t.Due = &LocalTime{Value: *value, Precision: *precision, Timezone: *tz}
	}
	return &t, nil
}

func loadTask(ctx context.Context, tx pgx.Tx, id string, lock bool) (*Task, error) {
	q := `SELECT ` + taskColumns + ` FROM pur_tasks WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	t, err := scanTask(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

func tasksOf(ctx context.Context, tx pgx.Tx, pursuit string) ([]*Task, error) {
	rows, err := tx.Query(ctx, `SELECT `+taskColumns+` FROM pur_tasks WHERE pursuit_id = $1 ORDER BY due_at NULLS LAST, created_at, id LIMIT 200`, pursuit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TaskView 是契约 Task 的 JSON。
type TaskView struct {
	ID              string     `json:"id"`
	PursuitID       string     `json:"pursuit_id"`
	Title           string     `json:"title"`
	Assignee        string     `json:"assignee"`
	Due             *LocalTime `json:"due,omitempty"`
	DueAt           *time.Time `json:"due_at,omitempty"`
	Note            string     `json:"note,omitempty"`
	Status          string     `json:"status"`
	PursuitClosed   bool       `json:"pursuit_closed,omitempty"`
	Version         int64      `json:"version"`
	DeadlineVersion int64      `json:"deadline_version"`
	CreatedAt       time.Time  `json:"created_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

func (t *Task) view(p *Pursuit) TaskView {
	return TaskView{ID: t.ID, PursuitID: t.PursuitID, Title: t.Title, Assignee: t.Assignee, Due: t.Due, DueAt: t.DueAt, Note: t.Note, Status: t.Status,
		PursuitClosed: p != nil && p.State == "closed", Version: t.Version, DeadlineVersion: t.DeadlineVersion, CreatedAt: t.CreatedAt, CompletedAt: t.CompletedAt}
}

// Authorization 是授权同步状态（契约 Authorization）。
type Authorization struct {
	State          string     `json:"state"`
	DesiredVersion int64      `json:"desired_version"`
	AppliedVersion int64      `json:"applied_version"`
	PendingSince   *time.Time `json:"pending_since,omitempty"`
}

func (s *Service) authorization(p *Pursuit) Authorization {
	a := Authorization{State: "effective", DesiredVersion: p.Participation, AppliedVersion: p.Applied, PendingSince: p.PendingSince}
	if p.Applied < p.Participation {
		a.State = "syncing"
		if p.PendingSince != nil && s.now().Sub(*p.PendingSince) > SyncFailedWait {
			a.State = "failed"
		}
	}
	return a
}

// Summary 是契约 PursuitSummary 的 JSON。
type Summary struct {
	ID            string        `json:"id"`
	ObjectiveID   string        `json:"objective_id"`
	Target        Target        `json:"target"`
	State         string        `json:"state"`
	Owner         string        `json:"owner"`
	OwnerValid    bool          `json:"owner_valid"`
	Version       int64         `json:"version"`
	Authorization Authorization `json:"authorization"`
	NextDue       *LocalTime    `json:"next_due,omitempty"`
	NextDueAt     *time.Time    `json:"next_due_at,omitempty"`
	Result        string        `json:"result,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
	ClosedAt      *time.Time    `json:"closed_at,omitempty"`
}

func (s *Service) summary(p *Pursuit, next *Task) Summary {
	out := Summary{ID: p.ID, ObjectiveID: p.ObjectiveID, Target: p.Target, State: p.State, Owner: p.Owner, OwnerValid: p.OwnerValid, Version: p.Version,
		Authorization: s.authorization(p), Result: p.Result, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, ClosedAt: p.ClosedAt}
	if next != nil {
		out.NextDue, out.NextDueAt = next.Due, next.DueAt
	}
	return out
}

// nextTask 返回跟进最早到期的待办任务（下一个截止）。
func nextTask(ctx context.Context, tx pgx.Tx, pursuit string) (*Task, error) {
	t, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM pur_tasks WHERE pursuit_id = $1 AND status = 'todo' AND due_at IS NOT NULL
 ORDER BY due_at, id LIMIT 1`, pursuit))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// State 是冲突时返回的当前值（契约 PursuitState）。
type State struct {
	PursuitID   string     `json:"pursuit_id"`
	State       string     `json:"state"`
	Version     int64      `json:"version"`
	Owner       string     `json:"owner"`
	TaskID      string     `json:"task_id,omitempty"`
	TaskStatus  string     `json:"task_status,omitempty"`
	TaskVersion int64      `json:"task_version,omitempty"`
	OutcomeID   string     `json:"outcome_id,omitempty"`
	ChangedBy   string     `json:"changed_by,omitempty"`
	ChangedAt   *time.Time `json:"changed_at,omitempty"`
}

func conflict(code string, p *Pursuit, t *Task) error {
	st := State{PursuitID: p.ID, State: p.State, Version: p.Version, Owner: p.Owner, OutcomeID: p.CurrentOutcome, ChangedBy: p.ChangedBy}
	at := p.UpdatedAt
	st.ChangedAt = &at
	if t != nil {
		st.TaskID, st.TaskStatus, st.TaskVersion = t.ID, t.Status, t.Version
	}
	raw, _ := json.Marshal(st)
	e := failure(409, code)
	e.Current = raw
	return e
}

// timeline 追加一条时间线（平台记录时间）。
func timeline(ctx context.Context, tx pgx.Tx, tenant, pursuit, kind, actor, detail string, at time.Time) error {
	var d *string
	if detail != "" {
		d = &detail
	}
	_, err := tx.Exec(ctx, `INSERT INTO pur_timeline (tenant_id, pursuit_id, kind, actor, detail, recorded_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		tenant, pursuit, kind, actor, d, at)
	return err
}

func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func versionDetail(format string, args ...any) string { return fmt.Sprintf(format, args...) }
