package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// 持续跟踪（PRD 第6段 F10，C-10）：决策者或业务开拓者（目标上的 can_assign）为一组组织创建持续型跟进；生命周期为跟踪中 ⇄ 已暂停 → 已停止。
// 持续型跟进与交易型共用 pur_pursuits 的一行：标识、目标与所在单元、负责人、期望参与关系与授权同步、成员变化的重新判断与写入准入
// （admitWrite）都相同；交易型的接口对持续型的标识为 404，反之亦然。到达周期时点时由 Reminders.RunTracking 结束一期：记录实际期间、
// 期末成员快照与对比基准，发布 pursuit.tracking.period_closed.v1 请求 channels 生成该期变化简报（channels 侧尚未实现）。暂停期间不结束、
// 恢复后从恢复时起到下一个周期时点为一期，不补暂停期间的各期；停止不删除已结束的各期。

// 事件类型（0.3.0）。
const (
	TrackingChanged      = "pursuit.tracking.changed.v1"
	TrackingPeriodClosed = "pursuit.tracking.period_closed.v1"
)

// 持续跟踪的上限与缺省值。
const (
	MaxTrackingMembers = 200
	// trackingFactType 是持续型跟进在 pur_pursuits.fact_type 中的固定值（不是领域事实类型）。
	trackingFactType = "pursuit.tracking"
	// maxCatchUp 是一次结束一期时向后追赶的周期时点数上限（执行者长时间停止后，最多合并约十年的周期为一期）。
	maxCatchUp = 520
)

// DefaultCadence 是缺省周期：每周一 08:00，Asia/Shanghai（租户与成员时区尚无所有者，见 PRD 第6段附“已知设计缺口”第4项）。
var DefaultCadence = Cadence{Weekday: 1, Time: "08:00", Timezone: "Asia/Shanghai"}

var orgPattern = regexp.MustCompile(`^org_[0-9A-HJKMNP-TV-Z]{26}$`)

// Cadence 是契约 Cadence：每周 weekday（1 周一 … 7 周日）的 time（HH:MM），按 timezone 计算。
type Cadence struct {
	Weekday  int    `json:"weekday"`
	Time     string `json:"time"`
	Timezone string `json:"timezone"`
}

func (c Cadence) parse() (*time.Location, int, int, error) {
	if c.Weekday < 1 || c.Weekday > 7 || c.Timezone == "" || c.Timezone == "Local" {
		return nil, 0, 0, failure(422, "invalid_cadence")
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil, 0, 0, failure(422, "invalid_cadence")
	}
	at, err := time.Parse("15:04", c.Time)
	if err != nil || at.Format("15:04") != c.Time {
		return nil, 0, 0, failure(422, "invalid_cadence")
	}
	return loc, at.Hour(), at.Minute(), nil
}

// Next 返回严格晚于 after 的第一个周期时点（UTC）。按所填时区的本地日期与时刻计算（夏令时按该时区的规则）。
func (c Cadence) Next(after time.Time) (time.Time, error) {
	loc, hh, mm, err := c.parse()
	if err != nil {
		return time.Time{}, err
	}
	local := after.In(loc)
	diff := (c.Weekday%7 - int(local.Weekday()) + 7) % 7
	at := time.Date(local.Year(), local.Month(), local.Day()+diff, hh, mm, 0, 0, loc)
	if !at.After(after) {
		at = time.Date(local.Year(), local.Month(), local.Day()+diff+7, hh, mm, 0, 0, loc)
	}
	return at.UTC(), nil
}

// trackingRow 是 pur_tracking 的一行。
type trackingRow struct {
	Name, State    string
	Members        []string
	MembersVersion int64
	Cadence        Cadence
	PeriodStart    *time.Time
	NextRun        *time.Time
	LastSeq        int64
	StoppedAt      *time.Time
}

const trackingColumns = `name, state, members, members_version, weekday, run_time, timezone, period_start, next_run_at, last_seq, stopped_at`

func scanTracking(row pgx.Row) (*trackingRow, error) {
	var t trackingRow
	err := row.Scan(&t.Name, &t.State, &t.Members, &t.MembersVersion, &t.Cadence.Weekday, &t.Cadence.Time, &t.Cadence.Timezone, &t.PeriodStart, &t.NextRun,
		&t.LastSeq, &t.StoppedAt)
	if err != nil {
		return nil, err
	}
	if t.Members == nil {
		t.Members = []string{}
	}
	return &t, nil
}

func loadTracking(ctx context.Context, tx pgx.Tx, id string, lock bool) (*trackingRow, error) {
	q := `SELECT ` + trackingColumns + ` FROM pur_tracking WHERE pursuit_id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	t, err := scanTracking(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// TrackingSummary 是契约 TrackingSummary。
type TrackingSummary struct {
	ID             string        `json:"id"`
	ObjectiveID    string        `json:"objective_id"`
	Name           string        `json:"name"`
	State          string        `json:"state"`
	Owner          string        `json:"owner"`
	OwnerValid     bool          `json:"owner_valid"`
	MembersCount   int           `json:"members_count"`
	MembersVersion int64         `json:"members_version"`
	NextRunAt      *time.Time    `json:"next_run_at,omitempty"`
	Version        int64         `json:"version"`
	Authorization  Authorization `json:"authorization"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

func (s *Service) trackingSummary(p *Pursuit, t *trackingRow) TrackingSummary {
	return TrackingSummary{ID: p.ID, ObjectiveID: p.ObjectiveID, Name: t.Name, State: t.State, Owner: p.Owner, OwnerValid: p.OwnerValid,
		MembersCount: len(t.Members), MembersVersion: t.MembersVersion, NextRunAt: t.NextRun, Version: p.Version, Authorization: s.authorization(p),
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}

// TrackingResult 是契约 TrackingCommandResult。
type TrackingResult struct {
	OperationID   string           `json:"operation_id"`
	Result        string           `json:"result"`
	Tracking      *TrackingSummary `json:"tracking,omitempty"`
	Authorization string           `json:"authorization,omitempty"`
}

// projectTracking 按调用者当前的权限投影固定的结果（每次返回前，含重放）：能查看时给出摘要与返回时的授权状态，否则只给操作标识与结果。
func (s *Service) projectTracking(ctx context.Context, rc reqctx.Context, f *Fixed) (*TrackingResult, error) {
	out := &TrackingResult{OperationID: f.OperationID, Result: f.Result}
	if f.PursuitID == "" {
		return out, nil
	}
	var p *Pursuit
	var t *trackingRow
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if p, err = loadPursuit(ctx, tx, f.PursuitID, false); err != nil || p == nil {
			return err
		}
		t, err = loadTracking(ctx, tx, p.ID, false)
		return err
	})
	if err != nil || p == nil || t == nil || p.Kind != "continuous" {
		return out, err
	}
	r, err := s.rights(ctx, rc, p, unitRights{})
	if err != nil {
		return nil, err
	}
	if !r.View {
		return out, nil
	}
	sum := s.trackingSummary(p, t)
	out.Tracking = &sum
	out.Authorization = "effective"
	if p.Applied < p.Participation {
		out.Authorization = "syncing"
	}
	return out, nil
}

// runTracking 执行一个持续跟踪命令：记录尝试，执行，按当前权限投影。
func (s *Service) runTracking(ctx context.Context, rc reqctx.Context, scope, key, fp string, fn func() (*Fixed, error)) (*TrackingResult, error) {
	f, err := s.attempt(ctx, rc, scope, key, fp, fn)
	if err != nil {
		return nil, err
	}
	return s.projectTracking(ctx, rc, f)
}

// CreateTrackingRequest 是契约 CreateTrackingRequest。
type CreateTrackingRequest struct {
	OperationID string   `json:"operation_id"`
	ObjectiveID string   `json:"objective_id"`
	Name        string   `json:"name"`
	Members     []string `json:"members"`
	Owner       string   `json:"owner,omitempty"`
	Cadence     *Cadence `json:"cadence,omitempty"`
}

// members 校验并规范对象集合：组织标识、1–200 个、去重（保持首次出现的顺序）。
func trackingMembers(in []string) ([]string, error) {
	out := []string{}
	for _, m := range in {
		if !orgPattern.MatchString(m) {
			return nil, failure(400, "invalid_request")
		}
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	if len(out) == 0 || len(out) > MaxTrackingMembers {
		return nil, failure(400, "invalid_request")
	}
	return out, nil
}

// trackingEvent 发布 pursuit.tracking.changed.v1（与变化在同一事务中）。
func trackingEvent(ctx context.Context, tx pgx.Tx, p *Pursuit, t *trackingRow, version int64, reason string, now time.Time) error {
	_, err := writer.Append(ctx, tx, outbox.Event{Type: TrackingChanged, Subject: p.ID, PartitionKey: p.ID, Data: map[string]any{"pursuit_id": p.ID,
		"objective_id": p.ObjectiveID, "org_unit_id": p.OrgUnitID, "state": t.State, "members": t.Members, "members_version": t.MembersVersion,
		"version": version, "reason": reason, "changed_at": ts(now)}})
	return err
}

// CreateTracking 创建持续跟踪（契约 createTracking；F10 第1条）。授权同创建交易型跟进：目标上的 can_assign（customer-context 的当前
// 口径），负责人以其本人判断是租户有效成员且能查看该目标的信号（否则 422 assignee_no_access，不写入、不自动授予权限）。
func (s *Service) CreateTracking(ctx context.Context, key string, req CreateTrackingRequest) (*TrackingResult, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	req.Name = strings.TrimSpace(req.Name)
	members, err := trackingMembers(req.Members)
	if err != nil {
		return nil, err
	}
	if req.Name == "" || len([]rune(req.Name)) > 120 {
		return nil, failure(400, "invalid_request")
	}
	req.Members = members
	if req.Cadence == nil {
		c := DefaultCadence
		req.Cadence = &c
	}
	fp := fingerprint("create_tracking", req)
	return s.runTracking(ctx, rc, "create_tracking", key, fp, func() (*Fixed, error) {
		o := op{id: req.OperationID, fp: fp, action: "create_tracking", channel: "web"}
		if f, found, err := s.lookup(ctx, rc, o); found || err != nil {
			return f, err
		}
		if _, err := req.Cadence.Next(s.now()); err != nil {
			return s.fix(ctx, rc, o, err)
		}
		scope, err := s.scopeOf(ctx, req.ObjectiveID, relAssign, relViewSignal)
		if err != nil {
			return s.fix(ctx, rc, o, err)
		}
		if !scope.Has(relAssign) {
			return s.fix(ctx, rc, o, failure(403, "forbidden"))
		}
		if scope.OrgUnitID == "" || scope.TemplateID == "" {
			return nil, failure(503, "customer_context_unavailable")
		}
		owner := req.Owner
		if owner == "" {
			owner = rc.Principal.ID
		}
		if owner == rc.Principal.ID && !scope.Has(relViewSignal) {
			return s.fix(ctx, rc, o, failure(422, "assignee_no_access"))
		}
		versions, err := s.assignees(ctx, rc, req.ObjectiveID, []string{owner}, nil)
		if err != nil {
			return s.fix(ctx, rc, o, err)
		}
		var created string
		f, err := s.commit(ctx, rc, o, func(ctx context.Context, tx pgx.Tx) (*Fixed, any, error) {
			now := s.businessNow()
			next, err := req.Cadence.Next(now)
			if err != nil {
				return nil, nil, err
			}
			p := &Pursuit{ID: ids.New("pur"), TenantID: rc.Tenant, ObjectiveID: req.ObjectiveID, OrgUnitID: scope.OrgUnitID, TemplateID: scope.TemplateID,
				Kind: "continuous", State: "open", Owner: owner, OwnerValid: true, Collaborators: []string{}, CreatedBy: rc.Principal.ID, Version: 1, Participation: 1,
				CreatedAt: now, UpdatedAt: now}
			grants, err := json.Marshal(regrant(nil, func(string) string { return "" }, owner, nil, 1, versions))
			if err != nil {
				return nil, nil, err
			}
			var notifyVersion *int64
			var notifyAssignee, notifyBy, notifyKind *string
			if owner != rc.Principal.ID {
				one, kind := int64(1), "created"
				notifyVersion, notifyAssignee, notifyBy, notifyKind = &one, &owner, &rc.Principal.ID, &kind
			}
			_, err = tx.Exec(ctx, `INSERT INTO pur_pursuits (id, tenant_id, objective_id, org_unit_id, template_id, kind, fact_type, subject_id, target_key,
 source_kind, channel, state, owner, owner_valid, collaborators, created_by, create_operation_id, version, participation_version, applied_version,
 sync_pending_since, notify_version, notify_assignee, notify_by, notify_kind, changed_by, created_at, updated_at, grants, team_changed_by)
 VALUES ($1,$2,$3,$4,$5,'continuous',$6,$1,$7,'manual','web','open',$8,true,'{}',$9,$10,1,1,0,$11,$12,$13,$14,$15,$9,$11,$11,$16,$9)`,
				p.ID, rc.Tenant, p.ObjectiveID, p.OrgUnitID, p.TemplateID, trackingFactType, "continuous|"+p.ID, owner, rc.Principal.ID, o.id, now,
				notifyVersion, notifyAssignee, notifyBy, notifyKind, grants)
			if err != nil {
				return nil, nil, err
			}
			t := &trackingRow{Name: req.Name, State: "tracking", Members: req.Members, MembersVersion: 1, Cadence: *req.Cadence, PeriodStart: &now, NextRun: &next}
			if _, err := tx.Exec(ctx, `INSERT INTO pur_tracking (pursuit_id, tenant_id, name, state, members, members_version, weekday, run_time, timezone,
 period_start, next_run_at) VALUES ($1,$2,$3,'tracking',$4,1,$5,$6,$7,$8,$9)`, p.ID, rc.Tenant, t.Name, t.Members, t.Cadence.Weekday, t.Cadence.Time,
				t.Cadence.Timezone, now, next); err != nil {
				return nil, nil, err
			}
			if err := trackingEvent(ctx, tx, p, t, 1, "created", now); err != nil {
				return nil, nil, err
			}
			if err := teamChanged(ctx, tx, p.ID, 1, "created"); err != nil {
				return nil, nil, err
			}
			if err := timeline(ctx, tx, rc.Tenant, p.ID, "created", rc.Principal.ID, versionDetail("持续跟踪；负责人 %s；对象 %d 个", owner, len(t.Members)), now); err != nil {
				return nil, nil, err
			}
			if err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.tracking.create", Resource: fmt.Sprintf("%s/op:%s/version:1/owner:%s/members:%d",
				p.ID, o.id, owner, len(t.Members)), Result: "succeeded", Basis: relAssign}); err != nil {
				return nil, nil, err
			}
			created = p.ID
			return &Fixed{Result: "created", PursuitID: p.ID}, nil, nil
		})
		if err == nil && created != "" {
			s.syncAfterCommit(ctx, rc.Tenant, rc.Cell, created)
		}
		return f, err
	})
}

// TrackingMembersRequest 是契约 TrackingMembersRequest。
type TrackingMembersRequest struct {
	OperationID string   `json:"operation_id"`
	Expected    int64    `json:"expected_version"`
	Members     []string `json:"members"`
}

// trackingChange 是一次持续跟踪状态或对象集合的变化：apply 在加锁重读后按当前值决定结果（no_change、确定失败或修改 t），返回事件
// 原因与时间线种类。
type trackingChange struct {
	action string
	apply  func(p *Pursuit, t *trackingRow, now time.Time) (reason, kind, detail string, err error)
}

// errNoChange 表示请求的效果已成立（无变化）。
var errNoChange = errors.New("no_change")

// PauseTracking 暂停（契约 pauseTracking）：跟踪中 → 已暂停，进行中的一期不再结束。已暂停为无变化，已停止 409 invalid_transition。
func (s *Service) PauseTracking(ctx context.Context, id, key string, req VersionRequest) (*TrackingResult, error) {
	return s.changeTracking(ctx, id, key, req.OperationID, req.Expected, req, trackingChange{action: "pause_tracking",
		apply: func(p *Pursuit, t *trackingRow, now time.Time) (string, string, string, error) {
			switch t.State {
			case "paused":
				return "", "", "", errNoChange
			case "stopped":
				return "", "", "", conflict("invalid_transition", p, nil)
			}
			t.State, t.PeriodStart, t.NextRun = "paused", nil, nil
			return "paused", "tracking_paused", "", nil
		}})
}

// ResumeTracking 恢复（契约 resumeTracking）：已暂停 → 跟踪中，下一期从恢复时起到下一个周期时点，不补暂停期间的各期。
func (s *Service) ResumeTracking(ctx context.Context, id, key string, req VersionRequest) (*TrackingResult, error) {
	return s.changeTracking(ctx, id, key, req.OperationID, req.Expected, req, trackingChange{action: "resume_tracking",
		apply: func(p *Pursuit, t *trackingRow, now time.Time) (string, string, string, error) {
			switch t.State {
			case "tracking":
				return "", "", "", errNoChange
			case "stopped":
				return "", "", "", conflict("invalid_transition", p, nil)
			}
			next, err := t.Cadence.Next(now)
			if err != nil {
				return "", "", "", err
			}
			start := now
			t.State, t.PeriodStart, t.NextRun = "tracking", &start, &next
			return "resumed", "tracking_resumed", "下一期结束 " + ts(next), nil
		}})
}

// StopTracking 停止（契约 stopTracking）：不再结束新的一期，已结束的各期保留。已停止为无变化。
func (s *Service) StopTracking(ctx context.Context, id, key string, req VersionRequest) (*TrackingResult, error) {
	return s.changeTracking(ctx, id, key, req.OperationID, req.Expected, req, trackingChange{action: "stop_tracking",
		apply: func(p *Pursuit, t *trackingRow, now time.Time) (string, string, string, error) {
			if t.State == "stopped" {
				return "", "", "", errNoChange
			}
			stopped := now
			t.State, t.PeriodStart, t.NextRun, t.StoppedAt = "stopped", nil, nil, &stopped
			return "stopped", "tracking_stopped", "", nil
		}})
}

// UpdateTrackingMembers 以完整集合替换对象集合（契约 updateTrackingMembers；F10 第3条）：成员版本加一，已结束的各期保留其当期快照。
func (s *Service) UpdateTrackingMembers(ctx context.Context, id, key string, req TrackingMembersRequest) (*TrackingResult, error) {
	members, err := trackingMembers(req.Members)
	if err != nil {
		return nil, err
	}
	req.Members = members
	return s.changeTracking(ctx, id, key, req.OperationID, req.Expected, req, trackingChange{action: "tracking_members",
		apply: func(p *Pursuit, t *trackingRow, now time.Time) (string, string, string, error) {
			if t.State == "stopped" {
				return "", "", "", conflict("invalid_transition", p, nil)
			}
			a, b := slices.Clone(t.Members), slices.Clone(members)
			slices.Sort(a)
			slices.Sort(b)
			if slices.Equal(a, b) {
				return "", "", "", errNoChange
			}
			added, removed := 0, 0
			for _, m := range members {
				if !slices.Contains(t.Members, m) {
					added++
				}
			}
			for _, m := range t.Members {
				if !slices.Contains(members, m) {
					removed++
				}
			}
			t.Members, t.MembersVersion = members, t.MembersVersion+1
			return "members_changed", "members_changed", fmt.Sprintf("对象 +%d −%d；成员版本 %d", added, removed, t.MembersVersion), nil
		}})
}

// changeTracking 是持续跟踪写命令的公共流程：操作标识与尝试、实时的读取权限（形态为持续型，否则 404）、统一写入准入（admitWrite，
// 加锁重读后再判断一次）、按当前值判断无变化与状态转换、版本并发控制，变化在一个事务中写入状态、版本、时间线、审计与事件。
func (s *Service) changeTracking(ctx context.Context, id, key, opID string, expected int64, req any, c trackingChange) (*TrackingResult, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	fp := fingerprint(c.action, id, req)
	return s.runTracking(ctx, rc, c.action, key, fp, func() (*Fixed, error) {
		o := op{id: opID, fp: fp, action: c.action, channel: "web", pursuit: id}
		if f, found, err := s.lookup(ctx, rc, o); found || err != nil {
			return f, err
		}
		p, r, err := s.accessKind(ctx, rc, id, "continuous")
		if err != nil {
			return s.fix(ctx, rc, o, err)
		}
		if err := s.admitWrite(p, r, rc.Principal.ID); err != nil {
			return s.fix(ctx, rc, o, err)
		}
		return s.commit(ctx, rc, o, func(ctx context.Context, tx pgx.Tx) (*Fixed, any, error) {
			now := s.businessNow()
			p, err := loadPursuit(ctx, tx, id, true)
			if err != nil {
				return nil, nil, err
			}
			if p == nil || p.Kind != "continuous" {
				return nil, nil, failure(404, "pursuit_not_found")
			}
			if err := s.admitWrite(p, r, rc.Principal.ID); err != nil {
				return nil, nil, err
			}
			t, err := loadTracking(ctx, tx, id, true)
			if err != nil {
				return nil, nil, err
			}
			if t == nil || p.State != "open" {
				return nil, nil, conflict("conflict", p, nil)
			}
			reason, kind, detail, err := c.apply(p, t, now)
			if errors.Is(err, errNoChange) {
				return &Fixed{Result: "no_change", PursuitID: id}, nil, nil
			}
			if err != nil {
				return nil, nil, err
			}
			if p.Version != expected {
				return nil, nil, conflict("conflict", p, nil)
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_tracking SET state = $2, members = $3, members_version = $4, period_start = $5, next_run_at = $6, stopped_at = $7
 WHERE pursuit_id = $1`, id, t.State, t.Members, t.MembersVersion, t.PeriodStart, t.NextRun, t.StoppedAt); err != nil {
				return nil, nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_pursuits SET version = version + 1, changed_by = $2, updated_at = $3 WHERE id = $1`, id, rc.Principal.ID, now); err != nil {
				return nil, nil, err
			}
			if err := trackingEvent(ctx, tx, p, t, p.Version+1, reason, now); err != nil {
				return nil, nil, err
			}
			if err := timeline(ctx, tx, rc.Tenant, id, kind, rc.Principal.ID, detail, now); err != nil {
				return nil, nil, err
			}
			if err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.tracking." + reason, Resource: fmt.Sprintf("%s/op:%s/version:%d/members_version:%d",
				id, o.id, p.Version+1, t.MembersVersion), Result: "succeeded", Basis: relEdit}); err != nil {
				return nil, nil, err
			}
			return &Fixed{Result: "succeeded", PursuitID: id}, nil, nil
		})
	})
}

// TrackingDetail 是契约 Tracking。
type TrackingDetail struct {
	ID             string          `json:"id"`
	ObjectiveID    string          `json:"objective_id"`
	OrgUnitID      string          `json:"org_unit_id"`
	Name           string          `json:"name"`
	State          string          `json:"state"`
	Owner          string          `json:"owner"`
	OwnerValid     bool            `json:"owner_valid"`
	Members        []string        `json:"members"`
	MembersVersion int64           `json:"members_version"`
	Cadence        Cadence         `json:"cadence"`
	PeriodStart    *time.Time      `json:"period_start,omitempty"`
	NextRunAt      *time.Time      `json:"next_run_at,omitempty"`
	LastPeriodSeq  int64           `json:"last_period_seq,omitempty"`
	Version        int64           `json:"version"`
	Authorization  Authorization   `json:"authorization"`
	CreatedBy      string          `json:"created_by"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	StoppedAt      *time.Time      `json:"stopped_at,omitempty"`
	Timeline       []TimelineEntry `json:"timeline"`
	ViewerCanEdit  bool            `json:"viewer_can_edit"`
}

// GetTracking 读取持续跟踪（契约 getTracking）：每次按当前权限判断；交易型的标识为 404。
func (s *Service) GetTracking(ctx context.Context, id string) (*TrackingDetail, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	p, r, err := s.accessKind(ctx, rc, id, "continuous")
	if err != nil {
		return nil, err
	}
	var t *trackingRow
	entries := []TimelineEntry{}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if t, err = loadTracking(ctx, tx, id, false); err != nil || t == nil {
			return err
		}
		entries, err = timelineOf(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, failure(404, "pursuit_not_found")
	}
	return &TrackingDetail{ID: p.ID, ObjectiveID: p.ObjectiveID, OrgUnitID: p.OrgUnitID, Name: t.Name, State: t.State, Owner: p.Owner, OwnerValid: p.OwnerValid,
		Members: t.Members, MembersVersion: t.MembersVersion, Cadence: t.Cadence, PeriodStart: t.PeriodStart, NextRunAt: t.NextRun, LastPeriodSeq: t.LastSeq,
		Version: p.Version, Authorization: s.authorization(p), CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, StoppedAt: t.StoppedAt,
		Timeline: entries, ViewerCanEdit: t.State != "stopped" && s.admitWrite(p, r, rc.Principal.ID) == nil}, nil
}

// timelineOf 返回最近 200 条时间线（按平台记录时间排列）。
func timelineOf(ctx context.Context, tx pgx.Tx, id string) ([]TimelineEntry, error) {
	rows, err := tx.Query(ctx, `SELECT kind, actor, COALESCE(detail, ''), recorded_at FROM (SELECT * FROM pur_timeline WHERE pursuit_id = $1 ORDER BY seq DESC LIMIT 200) t
 ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TimelineEntry{}
	for rows.Next() {
		var e TimelineEntry
		if err := rows.Scan(&e.Kind, &e.Actor, &e.Detail, &e.RecordedAt); err != nil {
			return nil, err
		}
		e.Detail = truncate(e.Detail, 500)
		out = append(out, e)
	}
	return out, rows.Err()
}

// TrackingList 是契约 TrackingList。
type TrackingList struct {
	Items      []TrackingSummary `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// trackingCursor 是持续跟踪列表的翻页位置（整个载荷签名，绑定租户、调用者与目标筛选）。
type trackingCursor struct {
	Tenant    string    `json:"t"`
	Subject   string    `json:"u"`
	Objective string    `json:"o,omitempty"`
	Created   time.Time `json:"c"`
	ID        string    `json:"i"`
}

// ListTracking 列出调用者可查看的持续跟踪（契约 listTracking）：按创建时间倒序，每次读取按当前权限过滤（无权的不出现）。
func (s *Service) ListTracking(ctx context.Context, objective string, limit int, cursor string) (*TrackingList, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	c := trackingCursor{Tenant: rc.Tenant, Subject: rc.Principal.ID, Objective: objective, Created: infinity, ID: "~"}
	if cursor != "" {
		var got trackingCursor
		if !s.unseal(purposeTrackingCursor, cursor, &got) || got.Tenant != rc.Tenant || got.Subject != rc.Principal.ID || got.Objective != objective {
			return nil, failure(400, "invalid_cursor")
		}
		c = got
	}
	out := &TrackingList{Items: []TrackingSummary{}}
	cache := unitRights{}
	scanned := 0
	for {
		type item struct {
			p *Pursuit
			t *trackingRow
		}
		var batch []item
		err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT `+pursuitColumns+` FROM pur_pursuits WHERE kind = 'continuous' AND state = 'open' AND ($1 = '' OR objective_id = $1)
 AND (created_at, id) < ($2, $3) ORDER BY created_at DESC, id DESC LIMIT 100`, objective, c.Created, c.ID)
			if err != nil {
				return err
			}
			var list []*Pursuit
			for rows.Next() {
				p, err := scanPursuit(rows)
				if err != nil {
					rows.Close()
					return err
				}
				list = append(list, p)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, p := range list {
				t, err := loadTracking(ctx, tx, p.ID, false)
				if err != nil {
					return err
				}
				batch = append(batch, item{p, t})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		for _, it := range batch {
			scanned++
			r, err := s.rights(ctx, rc, it.p, cache)
			if err != nil {
				return nil, err
			}
			if r.View && it.t != nil {
				if len(out.Items) == limit {
					return s.trackingPage(out, c)
				}
				out.Items = append(out.Items, s.trackingSummary(it.p, it.t))
			}
			c.Created, c.ID = it.p.CreatedAt, it.p.ID
			if scanned >= listScanCap {
				return s.trackingPage(out, c)
			}
		}
		if len(batch) < 100 {
			return out, nil
		}
	}
}

func (s *Service) trackingPage(out *TrackingList, c trackingCursor) (*TrackingList, error) {
	next, err := s.seal(purposeTrackingCursor, c)
	if err != nil {
		return nil, err
	}
	out.NextCursor = next
	return out, nil
}

// Period 是契约 TrackingPeriod。
type Period struct {
	PursuitID      string    `json:"pursuit_id"`
	Seq            int64     `json:"seq"`
	PeriodStart    time.Time `json:"period_start"`
	PeriodEnd      time.Time `json:"period_end"`
	Timezone       string    `json:"timezone"`
	Members        []string  `json:"members"`
	MembersVersion int64     `json:"members_version"`
	BaselineSeq    *int64    `json:"baseline_seq,omitempty"`
	Owner          string    `json:"owner"`
	OwnerValid     bool      `json:"owner_valid"`
	BriefingState  string    `json:"briefing_state"`
	RecordedAt     time.Time `json:"recorded_at"`
}

const periodColumns = `pursuit_id, seq, period_start, period_end, timezone, members, members_version, baseline_seq, owner, owner_valid, briefing_state, recorded_at`

func scanPeriod(row pgx.Row) (*Period, error) {
	var p Period
	if err := row.Scan(&p.PursuitID, &p.Seq, &p.PeriodStart, &p.PeriodEnd, &p.Timezone, &p.Members, &p.MembersVersion, &p.BaselineSeq, &p.Owner, &p.OwnerValid,
		&p.BriefingState, &p.RecordedAt); err != nil {
		return nil, err
	}
	if p.Members == nil {
		p.Members = []string{}
	}
	return &p, nil
}

// PeriodList 是契约 TrackingPeriodList。
type PeriodList struct {
	Items      []Period `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

// periodsCursor 是各期列表的翻页位置（整个载荷签名，绑定租户、调用者与该跟踪）。
type periodsCursor struct {
	Tenant  string `json:"t"`
	Subject string `json:"u"`
	Pursuit string `json:"p"`
	Before  int64  `json:"s"`
}

// ListTrackingPeriods 列出持续跟踪已结束的各期（契约 listTrackingPeriods）：按序号倒序，读取权限每次实时判断。
func (s *Service) ListTrackingPeriods(ctx context.Context, id string, limit int, cursor string) (*PeriodList, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	before := int64(1<<62 - 1)
	if cursor != "" {
		var got periodsCursor
		if !s.unseal(purposePeriodsCursor, cursor, &got) || got.Tenant != rc.Tenant || got.Subject != rc.Principal.ID || got.Pursuit != id || got.Before < 1 {
			return nil, failure(400, "invalid_cursor")
		}
		before = got.Before
	}
	if _, _, err := s.accessKind(ctx, rc, id, "continuous"); err != nil {
		return nil, err
	}
	out := &PeriodList{Items: []Period{}}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+periodColumns+` FROM pur_tracking_periods WHERE pursuit_id = $1 AND seq < $2 ORDER BY seq DESC LIMIT $3`, id, before, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPeriod(rows)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, *p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		next, err := s.seal(purposePeriodsCursor, periodsCursor{Tenant: rc.Tenant, Subject: rc.Principal.ID, Pursuit: id, Before: out.Items[limit-1].Seq})
		if err != nil {
			return nil, err
		}
		out.NextCursor = next
	}
	return out, nil
}

// GetTrackingPeriod 读取一期（契约 getTrackingPeriod；F10 第3条、用例47）：返回该期结束时固定的成员快照，之后修改对象集合不影响它；
// 读取权限按查看者当前的权限判断，不以该期结束时的负责人为准。
func (s *Service) GetTrackingPeriod(ctx context.Context, id string, seq int64) (*Period, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	if _, _, err := s.accessKind(ctx, rc, id, "continuous"); err != nil {
		return nil, err
	}
	var out *Period
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = scanPeriod(tx.QueryRow(ctx, `SELECT `+periodColumns+` FROM pur_tracking_periods WHERE pursuit_id = $1 AND seq = $2`, id, seq))
		if errors.Is(err, pgx.ErrNoRows) {
			out, err = nil, nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, failure(404, "period_not_found")
	}
	return out, nil
}

// RunTracking 是 Reminders.RunTracking（F10 第2–3条）：结束到达周期时点的跟踪中各期。
func (s *Service) RunTracking(ctx context.Context, now time.Time, limit int) (int, error) {
	rc, err := serviceContext(ctx)
	if err != nil {
		return 0, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	now = now.UTC().Truncate(time.Millisecond)
	var due []string
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT pursuit_id FROM pur_tracking WHERE state = 'tracking' AND next_run_at <= $1 ORDER BY next_run_at, pursuit_id LIMIT $2`, now, limit)
		if err != nil {
			return err
		}
		due, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, id := range due {
		var done bool
		err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			done, err = s.closePeriod(ctx, tx, rc.Tenant, id, now)
			return err
		})
		if err != nil {
			s.logger().WarnContext(ctx, "持续跟踪的一期暂未结束，稍后再试", "pursuit", id, "error", err.Error())
			continue
		}
		if done {
			closed++
		}
	}
	return closed, nil
}

// closePeriod 结束一个跟踪的进行中的一期（加行锁并跳过已被并发执行者锁住的；按当前值再检查仍为跟踪中、时点仍已到）。实际期间的结束取
// 不晚于 now 的最近一个周期时点（执行者停止过时，错过的周期时点合并为一期，实际期间如实记录）。返回是否结束了一期。
func (s *Service) closePeriod(ctx context.Context, tx pgx.Tx, tenant, id string, now time.Time) (bool, error) {
	t, err := scanTracking(tx.QueryRow(ctx, `SELECT `+trackingColumns+` FROM pur_tracking WHERE pursuit_id = $1 AND state = 'tracking' AND next_run_at <= $2
 FOR UPDATE SKIP LOCKED`, id, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	p, err := loadPursuit(ctx, tx, id, false)
	if err != nil {
		return false, err
	}
	if p == nil || p.Kind != "continuous" || p.State != "open" || t.PeriodStart == nil || t.NextRun == nil {
		return false, nil
	}
	end := *t.NextRun
	for i := 0; i < maxCatchUp; i++ {
		n, err := t.Cadence.Next(end)
		if err != nil {
			return false, err
		}
		if n.After(now) {
			break
		}
		end = n
	}
	next, err := t.Cadence.Next(end)
	if err != nil {
		return false, err
	}
	seq := t.LastSeq + 1
	var baseline *int64
	if t.LastSeq > 0 {
		b := t.LastSeq
		baseline = &b
	}
	recorded := s.businessNow()
	if _, err := tx.Exec(ctx, `INSERT INTO pur_tracking_periods (tenant_id, pursuit_id, seq, period_start, period_end, timezone, members, members_version,
 baseline_seq, owner, owner_valid, recorded_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, tenant, id, seq, *t.PeriodStart, end, t.Cadence.Timezone,
		t.Members, t.MembersVersion, baseline, p.Owner, p.OwnerValid, recorded); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE pur_tracking SET last_seq = $2, period_start = $3, next_run_at = $4 WHERE pursuit_id = $1`, id, seq, end, next); err != nil {
		return false, err
	}
	data := map[string]any{"pursuit_id": id, "objective_id": p.ObjectiveID, "org_unit_id": p.OrgUnitID, "seq": seq, "period_start": ts(*t.PeriodStart),
		"period_end": ts(end), "timezone": t.Cadence.Timezone, "members": t.Members, "members_version": t.MembersVersion, "owner": p.Owner,
		"owner_valid": p.OwnerValid, "recorded_at": ts(recorded)}
	if baseline != nil {
		data["baseline_seq"] = *baseline
	}
	_, err = writer.Append(ctx, tx, outbox.Event{Type: TrackingPeriodClosed, Subject: id, PartitionKey: id, Data: data})
	return err == nil, err
}
