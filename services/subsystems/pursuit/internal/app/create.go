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

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	detection "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// TaskInput 是契约 TaskInput。
type TaskInput struct {
	Title    string     `json:"title"`
	Assignee string     `json:"assignee,omitempty"`
	Due      *LocalTime `json:"due,omitempty"`
	Note     string     `json:"note,omitempty"`
}

// CreateRequest 是契约 CreatePursuitRequest。
type CreateRequest struct {
	OperationID   string     `json:"operation_id"`
	ObjectiveID   string     `json:"objective_id"`
	SignalID      string     `json:"signal_id,omitempty"`
	SeenContent   *int64     `json:"seen_content_version,omitempty"`
	Target        *Target    `json:"target,omitempty"`
	Owner         string     `json:"owner,omitempty"`
	Collaborators []string   `json:"collaborators,omitempty"`
	FirstTask     *TaskInput `json:"first_task,omitempty"`
	Note          string     `json:"note,omitempty"`
	Channel       string     `json:"channel,omitempty"`
}

// CreatePursuit 从信号或手动创建交易型跟进（契约 createPursuit；PRD 第6段 F1、F2）。
func (s *Service) CreatePursuit(ctx context.Context, key string, req CreateRequest) (*CommandResult, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	if req.Channel == "" {
		req.Channel = "web"
	}
	// 请求形状（契约中以说明给出、由代码判断）：信号与目标对象二选一；渠道创建只能由内部端口发起，负责人为点击者本人、没有任务
	switch {
	case req.Channel == "wecom" && !isInternal(ctx):
		return nil, failure(403, "forbidden")
	case (req.SignalID == "") == (req.Target == nil), (req.SignalID != "") != (req.SeenContent != nil):
		return nil, failure(400, "invalid_request")
	case req.Channel == "wecom" && ((req.Owner != "" && req.Owner != rc.Principal.ID) || len(req.Collaborators) > 0 || req.FirstTask != nil || req.Note != ""):
		return nil, failure(400, "invalid_request")
	}
	req.Collaborators = dedupe(req.Collaborators, "")
	fp := fingerprint("create", req)
	return s.run(ctx, rc, "create", key, fp, func() (*Fixed, error) { return s.create(ctx, rc, req, fp) })
}

// signalVersion 是 signal_updated 时信号的当前版本（契约 SignalVersion）。
type signalVersion struct {
	ContentVersion int64  `json:"content_version"`
	Status         string `json:"status"`
}

// signal 以调用者的权限读取信号并做创建与关联的检查（F1 第5条）：不可见、他租户与不属于所选目标同为 404 signal_not_found；
// 排除信号 422 not_applicable；已关闭 409 signal_closed；内容版本与所见不一致 409 signal_updated（带当前版本）。
func (s *Service) signal(ctx context.Context, objective, id string, seen int64) (*detection.SignalRow, error) {
	if s.Signals == nil {
		return nil, failure(503, "detection_unavailable")
	}
	list, err := s.Signals.SignalCards(ctx, []string{id})
	if err != nil {
		var safe *detection.Error
		if errors.As(err, &safe) && safe.Status < 500 && safe.Status != 404 {
			return nil, failure(safe.Status, safe.Code)
		}
		if errors.As(err, &safe) && safe.Status == 404 {
			return nil, failure(404, "signal_not_found")
		}
		return nil, failure(503, "detection_unavailable")
	}
	if len(list.Cards) == 0 || list.Cards[0].Signal.ID != id || list.Cards[0].Signal.ObjectiveID != objective {
		return nil, failure(404, "signal_not_found")
	}
	row := list.Cards[0].Signal
	switch {
	case row.Disposition == "excluded":
		return nil, failure(422, "not_applicable")
	case row.Status == "closed":
		return nil, failure(409, "signal_closed")
	case row.ContentVersion != seen:
		raw, _ := json.Marshal(signalVersion{ContentVersion: row.ContentVersion, Status: string(row.Status)})
		e := failure(409, "signal_updated")
		e.SignalCurrent = raw
		return nil, e
	}
	return &row, nil
}

// assignees 检查被分派的成员（F1 第2条、F4 第2条）：每人以其本人判断是否是租户有效成员且能查看该目标的信号；任一不满足即
// 422 assignee_no_access（整个命令不写入，不自动授予权限）。skip 中的成员（已在跟进中且有效）不再判断。返回被判断成员此刻在
// identity 的成员版本（记入授予记录）。
func (s *Service) assignees(ctx context.Context, rc reqctx.Context, objective string, members []string, skip func(string) bool) (map[string]int64, error) {
	versions := map[string]int64{}
	for _, m := range members {
		if skip != nil && skip(m) {
			continue
		}
		ok, mv, err := s.memberAccess(ctx, rc.Tenant, rc.Cell, m, objective)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &api.Error{Status: 422, Code: "assignee_no_access", Detail: m}
		}
		versions[m] = mv
	}
	return versions, nil
}

func (s *Service) create(ctx context.Context, rc reqctx.Context, req CreateRequest, fp string) (*Fixed, error) {
	o := op{id: req.OperationID, fp: fp, action: "create", channel: req.Channel}
	if f, found, err := s.lookup(ctx, rc, o); found || err != nil {
		return f, err
	}
	// 目标上的 can_assign（customer-context 的当前口径）：不可见 404 objective_not_found，无权 403 forbidden
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
	var target Target
	var row *detection.SignalRow
	if req.SignalID != "" {
		if row, err = s.signal(ctx, req.ObjectiveID, req.SignalID, *req.SeenContent); err != nil {
			return s.fix(ctx, rc, o, err)
		}
		target = Target{FactType: row.FactType, SubjectID: row.Subject.ID, PartID: row.Subject.PartID.Or("")}
		if w, ok := row.Winner.Get(); ok {
			target.WinnerOrgID = w.OrgID.Or("")
		}
	} else {
		target = *req.Target
	}
	owner := req.Owner
	if owner == "" {
		owner = rc.Principal.ID
	}
	collaborators := dedupe(req.Collaborators, owner)
	// 调用者本人作为负责人时同样须能查看该目标的信号；其他被分派人以其本人判断
	if owner == rc.Principal.ID && !scope.Has(relViewSignal) {
		return s.fix(ctx, rc, o, &api.Error{Status: 422, Code: "assignee_no_access", Detail: owner})
	}
	// 调用者本人同样以其本人判断一次，取得成员版本
	versions, err := s.assignees(ctx, rc, req.ObjectiveID, append([]string{owner}, collaborators...), nil)
	if err != nil {
		return s.fix(ctx, rc, o, err)
	}
	if t := req.FirstTask; t != nil {
		if t.Assignee != "" && t.Assignee != owner && !slices.Contains(collaborators, t.Assignee) {
			return s.fix(ctx, rc, o, failure(422, "invalid_assignee"))
		}
		if t.Due != nil {
			if _, err := t.Due.Instant(); err != nil {
				return s.fix(ctx, rc, o, err)
			}
		}
	}
	var created string
	f, err := s.commit(ctx, rc, o, func(ctx context.Context, tx pgx.Tx) (*Fixed, any, error) {
		now := s.businessNow()
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "pur-target/"+rc.Tenant+"/"+req.ObjectiveID+"/"+target.key()); err != nil {
			return nil, nil, err
		}
		existing, err := scanPursuit(tx.QueryRow(ctx, `SELECT `+pursuitColumns+` FROM pur_pursuits WHERE objective_id = $1 AND target_key = $2 AND state = 'open' FOR UPDATE`,
			req.ObjectiveID, target.key()))
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, err
		}
		if existing != nil {
			return s.existing(ctx, tx, rc, o, req, existing, now)
		}
		var previous *string
		var prev string
		err = tx.QueryRow(ctx, `SELECT id FROM pur_pursuits WHERE objective_id = $1 AND target_key = $2 AND state = 'closed' ORDER BY closed_at DESC, id DESC LIMIT 1`,
			req.ObjectiveID, target.key()).Scan(&prev)
		if err == nil {
			previous = &prev
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, err
		}
		p := &Pursuit{ID: ids.New("pur"), TenantID: rc.Tenant, ObjectiveID: req.ObjectiveID, OrgUnitID: scope.OrgUnitID, TemplateID: scope.TemplateID, Target: target,
			SourceKind: "manual", Channel: req.Channel, State: "open", Owner: owner, OwnerValid: true, Collaborators: collaborators, Note: req.Note,
			CreatedBy: rc.Principal.ID, CreateOp: o.id, ChangedBy: rc.Principal.ID, Version: 1, Participation: 1, CreatedAt: now, UpdatedAt: now, PendingSince: &now}
		if previous != nil {
			p.Previous = prev
		}
		grants, err := json.Marshal(regrant(nil, func(string) string { return "" }, owner, collaborators, 1, versions))
		if err != nil {
			return nil, nil, err
		}
		var signalCreated *time.Time
		if row != nil {
			p.SourceKind, p.SourceSignal, p.SourceContent = "signal", row.ID, row.ContentVersion
			c := row.CreatedAt.UTC()
			signalCreated = &c
		}
		var notifyVersion *int64
		var notifyAssignee, notifyBy, notifyKind *string
		if owner != rc.Principal.ID {
			one := int64(1)
			kind := "created"
			notifyVersion, notifyAssignee, notifyBy, notifyKind = &one, &owner, &rc.Principal.ID, &kind
		}
		var content *int64
		if row != nil {
			content = &p.SourceContent
		}
		_, err = tx.Exec(ctx, `INSERT INTO pur_pursuits (id, tenant_id, objective_id, org_unit_id, template_id, fact_type, subject_id, part_id, winner_org_id, target_key,
 source_kind, source_signal_id, source_content_version, source_signal_created_at, channel, previous_pursuit_id, state, owner, owner_valid, collaborators, note,
 created_by, create_operation_id, version, participation_version, applied_version, sync_pending_since, notify_version, notify_assignee, notify_by, notify_kind,
 changed_by, created_at, updated_at, grants)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'open',$17,true,$18,$19,$20,$21,1,1,0,$22,$23,$24,$25,$26,$20,$22,$22,$27)`,
			p.ID, rc.Tenant, p.ObjectiveID, p.OrgUnitID, p.TemplateID, target.FactType, target.SubjectID, optional(target.PartID), optional(target.WinnerOrgID), target.key(),
			p.SourceKind, optional(p.SourceSignal), content, signalCreated, p.Channel, previous, owner, collaborators, optional(req.Note),
			rc.Principal.ID, o.id, now, notifyVersion, notifyAssignee, notifyBy, notifyKind, grants)
		if err != nil {
			return nil, nil, err
		}
		data := map[string]any{"pursuit_id": p.ID, "operation_id": o.id, "objective_id": p.ObjectiveID, "org_unit_id": p.OrgUnitID, "target": target,
			"source": p.SourceKind, "owner": owner, "created_by": rc.Principal.ID, "channel": p.Channel, "created_at": ts(now)}
		if row != nil {
			data["signal_id"], data["seen_content_version"] = row.ID, row.ContentVersion
		}
		if previous != nil {
			data["previous_pursuit_id"] = prev
		}
		if _, err := writer.Append(ctx, tx, outbox.Event{Type: PursuitCreated, Subject: p.ID, Data: data}); err != nil {
			return nil, nil, err
		}
		if row != nil {
			if err := link(ctx, tx, rc, p, row.ID, row.ContentVersion, "create", o.id, now); err != nil {
				return nil, nil, err
			}
		}
		if req.FirstTask != nil {
			if _, err := s.insertTask(ctx, tx, rc, p, *req.FirstTask, now); err != nil {
				return nil, nil, err
			}
		}
		if err := teamChanged(ctx, tx, p.ID, 1, "created"); err != nil {
			return nil, nil, err
		}
		if err := timeline(ctx, tx, rc.Tenant, p.ID, "created", rc.Principal.ID, versionDetail("负责人 %s；来源 %s；渠道 %s", owner, p.SourceKind, p.Channel), now); err != nil {
			return nil, nil, err
		}
		if err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.pursuit.create", Resource: fmt.Sprintf("%s/op:%s/version:1/owner:%s", p.ID, o.id, owner),
			Result: "succeeded", Basis: relAssign}); err != nil {
			return nil, nil, err
		}
		created = p.ID
		return &Fixed{Result: "created", PursuitID: p.ID}, nil, nil
	})
	if err == nil && created != "" {
		s.syncAfterCommit(ctx, rc.Tenant, rc.Cell, created)
	}
	return f, err
}

// existing 处理同一目标、同一目标对象已有进行中的跟进（F2 第2–5条）：请求带信号且该信号尚未关联到它时新增关联（已存在 · 已关联，
// 一次改变，跟进版本加一），否则为已存在（无改变）；请求中的负责人、协作者、任务与说明都不写入，在 not_written 中列出。
func (s *Service) existing(ctx context.Context, tx pgx.Tx, rc reqctx.Context, o op, req CreateRequest, p *Pursuit, now time.Time) (*Fixed, any, error) {
	f := &Fixed{Result: "existing", PursuitID: p.ID}
	if req.Owner != "" && req.Channel == "web" {
		f.NotWritten = append(f.NotWritten, "owner")
	}
	if len(req.Collaborators) > 0 {
		f.NotWritten = append(f.NotWritten, "collaborators")
	}
	if req.FirstTask != nil {
		f.NotWritten = append(f.NotWritten, "first_task")
	}
	if req.Note != "" {
		f.NotWritten = append(f.NotWritten, "note")
	}
	if req.SignalID != "" {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pur_signal_links WHERE pursuit_id = $1 AND signal_id = $2 AND state = 'active'`, p.ID, req.SignalID).Scan(&n); err != nil {
			return nil, nil, err
		}
		if n == 0 {
			if err := link(ctx, tx, rc, p, req.SignalID, *req.SeenContent, "link_existing", o.id, now); err != nil {
				return nil, nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_pursuits SET version = version + 1, changed_by = $2, updated_at = $3 WHERE id = $1`, p.ID, rc.Principal.ID, now); err != nil {
				return nil, nil, err
			}
			if err := timeline(ctx, tx, rc.Tenant, p.ID, "signal_linked", rc.Principal.ID, req.SignalID, now); err != nil {
				return nil, nil, err
			}
			f.Result = "existing_linked"
		}
	}
	err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.pursuit." + f.Result, Resource: fmt.Sprintf("%s/op:%s/not_written:%s", p.ID, o.id,
		strings.Join(f.NotWritten, ",")), Result: "succeeded", Basis: relAssign})
	return f, nil, err
}

// link 建立一条信号关联并发布 pursuit.pursuit.signal_linked.v1（detection 以关联标识幂等维护有效关联集合）。
func link(ctx context.Context, tx pgx.Tx, rc reqctx.Context, p *Pursuit, signal string, seen int64, method, opID string, now time.Time) error {
	id := ids.New("psl")
	if _, err := tx.Exec(ctx, `INSERT INTO pur_signal_links (id, tenant_id, pursuit_id, signal_id, seen_content_version, method, operation_id, state, linked_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,'active',$8)`, id, rc.Tenant, p.ID, signal, seen, method, opID, now); err != nil {
		return err
	}
	_, err := writer.Append(ctx, tx, outbox.Event{Type: SignalLinked, Subject: p.ID, PartitionKey: signal, Data: map[string]any{"link_id": id, "pursuit_id": p.ID,
		"signal_id": signal, "seen_content_version": seen, "method": method, "operation_id": opID, "linked_at": ts(now)}})
	return err
}

// teamChanged 发布授权同步请求（与期望参与关系在同一事务中）。
func teamChanged(ctx context.Context, tx pgx.Tx, pursuit string, version int64, reason string) error {
	_, err := writer.Append(ctx, tx, outbox.Event{Type: TeamChanged, Subject: pursuit, Data: map[string]any{"pursuit_id": pursuit,
		"participation_version": version, "reason": reason}})
	return err
}

// VersionRequest 是契约 VersionRequest。
type VersionRequest struct {
	OperationID string `json:"operation_id"`
	Expected    int64  `json:"expected_version"`
}

// VoidPursuit 撤销创建（契约 voidPursuit；F3 第4条、F12 第5条）。
func (s *Service) VoidPursuit(ctx context.Context, id, key string, req VersionRequest) (*CommandResult, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	fp := fingerprint("void", id, req)
	return s.run(ctx, rc, "void", key, fp, func() (*Fixed, error) {
		o := op{id: req.OperationID, fp: fp, action: "void", channel: "web", pursuit: id}
		if f, found, err := s.lookup(ctx, rc, o); found || err != nil {
			return f, err
		}
		p, r, err := s.access(ctx, rc, id)
		if err != nil {
			return s.fix(ctx, rc, o, err)
		}
		if !r.View {
			return s.fix(ctx, rc, o, failure(404, "pursuit_not_found"))
		}
		if p.CreatedBy != rc.Principal.ID {
			return s.fix(ctx, rc, o, failure(403, "forbidden"))
		}
		restoration := s.restorationHint(ctx, id)
		var voided bool
		f, err := s.commit(ctx, rc, o, func(ctx context.Context, tx pgx.Tx) (*Fixed, any, error) {
			now := s.businessNow()
			p, err := loadPursuit(ctx, tx, id, true)
			if err != nil {
				return nil, nil, err
			}
			switch {
			case p.State != "open":
				return nil, nil, conflict("conflict", p, nil)
			case now.Sub(p.CreatedAt) > VoidWindow:
				return nil, nil, conflict("void_window_expired", p, nil)
			case p.Version != req.Expected || p.Version != 1:
				return nil, nil, conflict("conflict", p, nil)
			}
			pv := p.Participation + 1
			if _, err := tx.Exec(ctx, `UPDATE pur_pursuits SET state = 'voided', voided_at = $2, version = version + 1, participation_version = $3,
 sync_pending_since = COALESCE(sync_pending_since, $2), notify_version = NULL, notify_assignee = NULL, notify_by = NULL, notify_kind = NULL, changed_by = $4, updated_at = $2
 WHERE id = $1`, id, now, pv, rc.Principal.ID); err != nil {
				return nil, nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_tasks SET status = 'cancelled', version = version + 1 WHERE pursuit_id = $1 AND status = 'todo'`, id); err != nil {
				return nil, nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_reminders SET state = 'cancelled', cancelled_by = $2 WHERE pursuit_id = $1 AND state = 'planned'`, id, o.id); err != nil {
				return nil, nil, err
			}
			rows, err := tx.Query(ctx, `UPDATE pur_signal_links SET state = 'removed', removed_at = $2 WHERE pursuit_id = $1 AND state = 'active' RETURNING id, signal_id`, id, now)
			if err != nil {
				return nil, nil, err
			}
			type removed struct{ id, signal string }
			var links []removed
			for rows.Next() {
				var l removed
				if err := rows.Scan(&l.id, &l.signal); err != nil {
					rows.Close()
					return nil, nil, err
				}
				links = append(links, l)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, nil, err
			}
			for _, l := range links {
				if _, err := writer.Append(ctx, tx, outbox.Event{Type: SignalUnlinked, Subject: id, PartitionKey: l.signal, Data: map[string]any{"link_id": l.id,
					"pursuit_id": id, "signal_id": l.signal, "reason": "voided", "operation_id": o.id, "unlinked_at": ts(now)}}); err != nil {
					return nil, nil, err
				}
			}
			if _, err := writer.Append(ctx, tx, outbox.Event{Type: PursuitVoided, Subject: id, Data: map[string]any{"pursuit_id": id, "operation_id": o.id,
				"voided_by": rc.Principal.ID, "voided_at": ts(now), "unlinked": len(links)}}); err != nil {
				return nil, nil, err
			}
			if err := teamChanged(ctx, tx, id, pv, "voided"); err != nil {
				return nil, nil, err
			}
			if err := timeline(ctx, tx, rc.Tenant, id, "voided", rc.Principal.ID, fmt.Sprintf("解除关联 %d 条", len(links)), now); err != nil {
				return nil, nil, err
			}
			if err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.pursuit.void", Resource: fmt.Sprintf("%s/op:%s/version:%d", id, o.id, p.Version+1),
				Result: "succeeded", Basis: "creator"}); err != nil {
				return nil, nil, err
			}
			voided = true
			return &Fixed{Result: "succeeded", PursuitID: id, SignalRestoration: restoration}, nil, nil
		})
		if err == nil && voided {
			s.syncAfterCommit(ctx, rc.Tenant, rc.Cell, id)
		}
		return f, err
	})
}

// restorationHint 返回撤销创建对来源信号的说明（F3 第4条）：信号已关闭时状态不恢复。读取失败不影响撤销。
func (s *Service) restorationHint(ctx context.Context, id string) string {
	var signal string
	_ = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(source_signal_id, '') FROM pur_pursuits WHERE id = $1`, id).Scan(&signal)
	})
	if signal == "" || s.Signals == nil {
		return ""
	}
	list, err := s.Signals.SignalCards(ctx, []string{signal})
	if err != nil || len(list.Cards) == 0 {
		return ""
	}
	if list.Cards[0].Signal.Status == "closed" {
		return "信号已关闭，状态不恢复"
	}
	return "信号在没有其他有效关联且状态未变时回到转跟进前的状态"
}

// access 读取跟进并判断调用者的当前权限；不存在与他租户返回 404 pursuit_not_found。
func (s *Service) access(ctx context.Context, rc reqctx.Context, id string) (*Pursuit, Rights, error) {
	var p *Pursuit
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = loadPursuit(ctx, tx, id, false)
		return err
	})
	if err != nil {
		return nil, Rights{}, err
	}
	if p == nil {
		return nil, Rights{}, failure(404, "pursuit_not_found")
	}
	r, err := s.rights(ctx, rc, p, unitRights{})
	if err != nil {
		return nil, Rights{}, err
	}
	if !r.View || p.State == "voided" && p.CreatedBy != rc.Principal.ID && !r.UnitEdit {
		return nil, Rights{}, failure(404, "pursuit_not_found")
	}
	return p, r, nil
}

// AssignRequest 是契约 AssignRequest。
type AssignRequest struct {
	OperationID   string   `json:"operation_id"`
	Expected      int64    `json:"expected_version"`
	Owner         string   `json:"owner"`
	Collaborators []string `json:"collaborators"`
	Reason        string   `json:"reason,omitempty"`
}

// AssignPursuit 改派负责人与设置协作者（契约 assignPursuit；F4、F12）。
func (s *Service) AssignPursuit(ctx context.Context, id, key string, req AssignRequest) (*CommandResult, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	req.Collaborators = dedupe(req.Collaborators, req.Owner)
	fp := fingerprint("assign", id, req)
	return s.run(ctx, rc, "assign", key, fp, func() (*Fixed, error) {
		o := op{id: req.OperationID, fp: fp, action: "assign", channel: "web", pursuit: id}
		if f, found, err := s.lookup(ctx, rc, o); found || err != nil {
			return f, err
		}
		p, r, err := s.access(ctx, rc, id)
		if err != nil {
			return s.fix(ctx, rc, o, err)
		}
		if err := s.admitWrite(p, r, rc.Principal.ID); err != nil {
			return s.fix(ctx, rc, o, err)
		}
		// 角色不变且已有授予记录的成员沿用原授予（不再判断）；新加入、角色改变（负责人↔协作者）或失效后重新分派的成员以其本人
		// 重新判断成员状态与目标访问，并记下此刻的成员版本（不沿用授予前的成员版本，否则迟到的旧退出事件会撤销这次分派）
		pre := p
		roleIn := func(m string) string {
			if m == req.Owner {
				return "owner"
			}
			return "collaborator"
		}
		unchanged := func(q *Pursuit, m string) bool {
			g, ok := q.Grants[m]
			return ok && g.Role == roleIn(m) && q.role(m) == roleIn(m)
		}
		versions, err := s.assignees(ctx, rc, p.ObjectiveID, append([]string{req.Owner}, req.Collaborators...), func(m string) bool { return unchanged(pre, m) })
		if err != nil {
			return s.fix(ctx, rc, o, err)
		}
		var changed bool
		f, err := s.commit(ctx, rc, o, func(ctx context.Context, tx pgx.Tx) (*Fixed, any, error) {
			now := s.businessNow()
			p, err := loadPursuit(ctx, tx, id, true)
			if err != nil {
				return nil, nil, err
			}
			if p.State != "open" {
				return nil, nil, conflict("conflict", p, nil)
			}
			if err := s.admitWrite(p, r, rc.Principal.ID); err != nil {
				return nil, nil, err
			}
			if p.Owner == req.Owner && p.OwnerValid && slices.Equal(dedupe(p.Collaborators, ""), req.Collaborators) {
				return &Fixed{Result: "no_change", PursuitID: id}, nil, nil
			}
			if p.Version != req.Expected {
				return nil, nil, conflict("conflict", p, nil)
			}
			// 沿用原授予的成员须在加锁后仍是同一授予：并发改动使原判断失效时按冲突处理
			for _, m := range append([]string{req.Owner}, req.Collaborators...) {
				if unchanged(pre, m) && (!unchanged(p, m) || p.Grants[m] != pre.Grants[m]) {
					return nil, nil, conflict("conflict", p, nil)
				}
			}
			pv := p.Participation + 1
			ownerChanged := p.Owner != req.Owner || !p.OwnerValid
			var nv *int64
			var na, nb, nk *string
			if ownerChanged && req.Owner != rc.Principal.ID {
				kind := "reassigned"
				nv, na, nb, nk = &pv, &req.Owner, &rc.Principal.ID, &kind
			} else if !ownerChanged {
				nv, na, nb, nk = p.NotifyVersion, optional(p.NotifyAssignee), optional(p.NotifyBy), optional(p.NotifyKind)
			}
			grants, err := json.Marshal(regrant(p.Grants, p.role, req.Owner, req.Collaborators, pv, versions))
			if err != nil {
				return nil, nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_pursuits SET owner = $2, owner_valid = true, collaborators = $3, version = version + 1, participation_version = $4,
 sync_pending_since = COALESCE(sync_pending_since, $5), notify_version = $6, notify_assignee = $7, notify_by = $8, notify_kind = $9, changed_by = $10, updated_at = $5,
 grants = $11 WHERE id = $1`, id, req.Owner, req.Collaborators, pv, now, nv, na, nb, nk, rc.Principal.ID, grants); err != nil {
				return nil, nil, err
			}
			// 不再在团队中的成员的待办任务转给新负责人（任务负责人须为负责人或协作者）
			team := append([]string{req.Owner}, req.Collaborators...)
			if _, err := tx.Exec(ctx, `UPDATE pur_tasks SET assignee = $2, version = version + 1 WHERE pursuit_id = $1 AND status = 'todo' AND NOT (assignee = ANY($3))`,
				id, req.Owner, team); err != nil {
				return nil, nil, err
			}
			reason := "team_changed"
			if ownerChanged {
				reason = "reassigned"
			}
			if err := teamChanged(ctx, tx, id, pv, reason); err != nil {
				return nil, nil, err
			}
			detail := fmt.Sprintf("负责人 %s → %s；协作者 %s", p.Owner, req.Owner, strings.Join(req.Collaborators, "、"))
			if req.Reason != "" {
				detail += "；原因：" + req.Reason
			}
			if err := timeline(ctx, tx, rc.Tenant, id, reason, rc.Principal.ID, detail, now); err != nil {
				return nil, nil, err
			}
			if err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.pursuit.assign", Resource: fmt.Sprintf("%s/op:%s/version:%d/owner:%s->%s/participation:%d",
				id, o.id, p.Version+1, p.Owner, req.Owner, pv), Result: "succeeded", Basis: "can_manage_team"}); err != nil {
				return nil, nil, err
			}
			changed = true
			return &Fixed{Result: "succeeded", PursuitID: id}, nil, nil
		})
		if err == nil && changed {
			s.syncAfterCommit(ctx, rc.Tenant, rc.Cell, id)
		}
		return f, err
	})
}
