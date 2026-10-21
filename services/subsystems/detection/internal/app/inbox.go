package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 收件箱（PRD 第3段 F2、F3）：一次只看一个目标，信号随目标的 can_view_signals 授权（按 customer-context 的当前口径），不逐条授权。

const (
	pageSize     = 50  // 每页信号数（契约上限）
	batchLimit   = 200 // 批量上限（PRD 第3段第13节第3项）
	detailLimit  = 50  // 详情中的更新历史与处理记录条数
	feedbackShow = 100 // 详情中的成员反馈条数
)

// Feedback 是一名成员的反馈值（契约 Feedback）。
type Feedback struct {
	Result  string   `json:"result"`
	Reasons []string `json:"reasons"`
	Note    string   `json:"note,omitempty"`
	BasedOn int64    `json:"based_on_content_version"`
}

func (f *Feedback) equal(o *Feedback) bool {
	if f == nil || o == nil {
		return f == nil && o == nil
	}
	return f.Result == o.Result && strings.Join(f.Reasons, ",") == strings.Join(o.Reasons, ",") && f.Note == o.Note && f.BasedOn == o.BasedOn
}

// PolicyRef 是产生信号的策略版本（契约 PolicyRef）。
type PolicyRef struct {
	PolicyID        string   `json:"policy_id"`
	Version         int64    `json:"version"`
	Generation      int64    `json:"generation"`
	TemplateID      string   `json:"template_id"`
	TemplateVersion string   `json:"template_version"`
	EnabledDomains  []string `json:"enabled_domains"`
}

// SubjectRef 是求值单位（契约 SubjectRef）。
type SubjectRef struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Version int64  `json:"version"`
	PartID  string `json:"part_id,omitempty"`
}

// Winner 与 Amount 是行的展示字段。
type Winner struct {
	OrgID string `json:"org_id,omitempty"`
	Name  string `json:"name,omitempty"`
}

type Amount struct {
	Value    string `json:"value"`
	Currency string `json:"currency"`
}

// Row 是收件箱的一行（契约 SignalRow）。
type Row struct {
	ID              string     `json:"id"`
	ObjectiveID     string     `json:"objective_id"`
	FactType        string     `json:"fact_type"`
	Subject         SubjectRef `json:"subject"`
	Title           string     `json:"title"`
	Winner          *Winner    `json:"winner,omitempty"`
	Amount          *Amount    `json:"amount,omitempty"`
	Location        string     `json:"location,omitempty"`
	Categories      []string   `json:"categories"`
	SameLotCount    int        `json:"same_lot_count"`
	Disposition     string     `json:"disposition"`
	Manual          string     `json:"manual_disposition,omitempty"`
	Score           int        `json:"score"`
	NeedsReview     bool       `json:"needs_review"`
	ReasonCode      string     `json:"reason_code,omitempty"`
	Reasons         []string   `json:"reasons"`
	UnknownCount    int        `json:"unknown_count"`
	Status          string     `json:"status"`
	StateVersion    int64      `json:"state_version"`
	ContentVersion  int64      `json:"content_version"`
	MyFeedback      *Feedback  `json:"my_feedback,omitempty"`
	FeedbackVersion int64      `json:"feedback_version"`
	Backfill        bool       `json:"backfill"`
	UpdatedRecently bool       `json:"updated_recently"`
	Policy          PolicyRef  `json:"policy"`
	EvaluatedAt     time.Time  `json:"evaluated_at"`
	CreatedAt       time.Time  `json:"created_at"`

	// 不在契约行中的内部字段
	tenant, conclusion, manualReason, hash, orgUnit, trigger, objectiveCode string
	contextVersion                                                          int64
	trace, features                                                         []byte
}

// Page 是一页信号（契约 SignalPage）。
type Page struct {
	ObjectiveID       string `json:"objective_id"`
	Signals           []Row  `json:"signals"`
	NextCursor        string `json:"next_cursor,omitempty"`
	ViewerCanFeedback bool   `json:"viewer_can_feedback"`
}

// Filter 是收件箱的分栏与筛选。
type Filter struct {
	Tab         string
	Disposition string
	FactType    string
	Region      string
	Category    string
	AmountMin   *int64
	AmountMax   *int64
	From, To    *time.Time
	MyFeedback  string
	Marker      string
}

const rowColumns = `s.id, s.tenant_id, s.objective_id, s.objective_code, s.org_unit_id, s.fact_type, s.subject_id, s.subject_version, s.part_id, s.title,
 s.winner_org_id, s.winner_name, s.amount_value, s.amount_currency, s.location, s.categories, s.disposition, s.manual_disposition, s.manual_reason,
 s.score, s.needs_review, s.reason_code, s.reasons, s.unknown_count, s.status, s.state_version, s.content_version, s.backfill,
 s.material_updated_at, s.policy_id, s.policy_version, s.generation, s.template_id, s.template_version, s.enabled_domains,
 s.evaluated_at, s.created_at, s.conclusion, s.trace, s.features, s.view_snapshot_hash, s.context_version, s.trigger_event_id,
 CASE WHEN s.lot_key IS NULL THEN 1 ELSE (SELECT count(*) FROM det_signals l WHERE l.objective_id = s.objective_id AND l.lot_key = s.lot_key) END,
 f.result, f.reasons, f.note, f.based_on_content_version, f.version`

// rowFrom 是行查询的 FROM 子句：$1 为查看者（本人反馈）。
const rowFrom = ` FROM det_signals s LEFT JOIN det_feedback f ON f.signal_id = s.id AND f.member = $1`

func (s *Service) scanRow(row pgx.Row) (*Row, error) {
	var r Row
	var part, winnerID, winnerName, amountValue, amountCurrency, location, manual, manualReason, reasonCode *string
	var material *time.Time
	var fbResult, fbNote *string
	var fbReasons []string
	var fbBased, fbVersion *int64
	if err := row.Scan(&r.ID, &r.tenant, &r.ObjectiveID, &r.objectiveCode, &r.orgUnit, &r.FactType, &r.Subject.ID, &r.Subject.Version, &part, &r.Title,
		&winnerID, &winnerName, &amountValue, &amountCurrency, &location, &r.Categories, &r.Disposition, &manual, &manualReason,
		&r.Score, &r.NeedsReview, &reasonCode, &r.Reasons, &r.UnknownCount, &r.Status, &r.StateVersion, &r.ContentVersion, &r.Backfill,
		&material, &r.Policy.PolicyID, &r.Policy.Version, &r.Policy.Generation, &r.Policy.TemplateID, &r.Policy.TemplateVersion, &r.Policy.EnabledDomains,
		&r.EvaluatedAt, &r.CreatedAt, &r.conclusion, &r.trace, &r.features, &r.hash, &r.contextVersion, &r.trigger, &r.SameLotCount,
		&fbResult, &fbReasons, &fbNote, &fbBased, &fbVersion); err != nil {
		return nil, err
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	r.Subject.Type, r.Subject.PartID = "domain_fact", str(part)
	if winnerID != nil || winnerName != nil {
		r.Winner = &Winner{OrgID: str(winnerID), Name: str(winnerName)}
	}
	if amountValue != nil && amountCurrency != nil {
		r.Amount = &Amount{Value: *amountValue, Currency: *amountCurrency}
	}
	r.Location, r.Manual, r.manualReason, r.ReasonCode = str(location), str(manual), str(manualReason), str(reasonCode)
	r.EvaluatedAt, r.CreatedAt = r.EvaluatedAt.UTC(), r.CreatedAt.UTC()
	r.UpdatedRecently = material != nil && material.After(s.now().Add(-7*24*time.Hour))
	if fbVersion != nil {
		r.FeedbackVersion = *fbVersion
		if fbResult != nil {
			r.MyFeedback = &Feedback{Result: *fbResult, Reasons: fbReasons, Note: str(fbNote), BasedOn: *fbBased}
		}
	}
	if r.Categories == nil {
		r.Categories = []string{}
	}
	if r.Reasons == nil {
		r.Reasons = []string{}
	}
	if r.Policy.EnabledDomains == nil {
		r.Policy.EnabledDomains = []string{}
	}
	return &r, nil
}

// where 把分栏与筛选转换为条件（args 从 $2 起；$1 为查看者）。
func (s *Service) where(objective string, f Filter, args *[]any) string {
	add := func(v any) string {
		*args = append(*args, v)
		return fmt.Sprintf("$%d", len(*args))
	}
	conds := []string{"s.objective_id = " + add(objective)}
	switch f.Tab {
	case "handled":
		conds = append(conds, "s.status IN ('dismissed','converted') AND s.disposition <> 'excluded'")
	case "excluded":
		conds = append(conds, "s.disposition = 'excluded' AND s.status <> 'closed'")
	case "closed":
		conds = append(conds, "s.status = 'closed'")
	default:
		conds = append(conds, "s.status IN ('open','triaged') AND s.disposition <> 'excluded'")
	}
	if f.Disposition != "" {
		conds = append(conds, "COALESCE(s.manual_disposition, s.disposition) = "+add(f.Disposition))
	}
	if f.FactType != "" {
		conds = append(conds, "s.fact_type = "+add(f.FactType))
	}
	if f.Region != "" {
		prefix := f.Region
		switch {
		case strings.HasSuffix(prefix, "0000"):
			prefix = prefix[:2]
		case strings.HasSuffix(prefix, "00"):
			prefix = prefix[:4]
		}
		conds = append(conds, "s.location LIKE "+add(prefix+"%"))
	}
	if f.Category != "" {
		p := add(f.Category)
		conds = append(conds, fmt.Sprintf("EXISTS (SELECT 1 FROM unnest(s.categories) c WHERE c = %s OR c LIKE %s || '-%%')", p, p))
	}
	if f.AmountMin != nil {
		conds = append(conds, "s.amount_cny >= "+add(*f.AmountMin))
	}
	if f.AmountMax != nil {
		conds = append(conds, "s.amount_cny <= "+add(*f.AmountMax))
	}
	if f.From != nil {
		conds = append(conds, "s.created_at >= "+add(*f.From))
	}
	if f.To != nil {
		conds = append(conds, "s.created_at < "+add(*f.To))
	}
	switch f.MyFeedback {
	case "":
	case "none":
		conds = append(conds, "f.result IS NULL")
	default:
		conds = append(conds, "f.result = "+add(f.MyFeedback))
	}
	switch f.Marker {
	case "backfill":
		conds = append(conds, "s.backfill")
	case "updated":
		conds = append(conds, "s.material_updated_at > "+add(s.now().Add(-7*24*time.Hour)))
	case "unknown":
		conds = append(conds, "s.unknown_count > 0")
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// cursor 是分页位置，绑定目标、查看者、分栏、筛选与排序（binding 为摘要）。
type cursor struct {
	Binding string    `json:"b"`
	Key     int64     `json:"k"`
	Created time.Time `json:"c"`
	ID      string    `json:"i"`
}

func binding(parts ...any) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:12])
}

func encodeCursor(c cursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s, bind string) (*cursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	var c cursor
	if err != nil || json.Unmarshal(raw, &c) != nil || c.Binding != bind || c.ID == "" {
		return nil, failure(400, "invalid_cursor")
	}
	return &c, nil
}

// objectiveAccess 判断收件箱的访问：不能查看目标与目标不存在为 404 objective_not_found，能查看而没有 can_view_signals 为 403。
func (s *Service) objectiveAccess(ctx context.Context, objective string) (canFeedback bool, err error) {
	visible, granted, err := s.access(ctx, objective, relViewSignals, relFeedback)
	if err != nil {
		return false, err
	}
	if !visible {
		return false, failure(404, "objective_not_found")
	}
	if !has(granted, relViewSignals) {
		return false, failure(403, "forbidden")
	}
	return has(granted, relFeedback), nil
}

// ListSignals 列出一个目标的信号（契约 listSignals）。
func (s *Service) ListSignals(ctx context.Context, objective string, f Filter, sort string, limit int, after string) (*Page, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > pageSize {
		limit = pageSize
	}
	if sort == "" {
		sort = "score"
	}
	bind := binding(objective, rc.Principal.ID, f, sort)
	pos, err := decodeCursor(after, bind)
	if err != nil {
		return nil, err
	}
	canFeedback, err := s.objectiveAccess(ctx, objective)
	if err != nil {
		return nil, err
	}
	args := []any{rc.Principal.ID}
	where := s.where(objective, f, &args)
	add := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	var keyExpr, order string
	switch sort {
	case "discovered":
		keyExpr, order = "0", "s.created_at DESC, s.id DESC"
	case "amount":
		keyExpr, order = "COALESCE(s.amount_cny, -1)", "COALESCE(s.amount_cny, -1) DESC, s.created_at DESC, s.id DESC"
	default:
		keyExpr, order = "s.score", "s.score DESC, s.created_at DESC, s.id DESC"
	}
	if pos != nil {
		where += fmt.Sprintf(" AND (%s, s.created_at, s.id) < (%s, %s, %s)", keyExpr, add(pos.Key), add(pos.Created), add(pos.ID))
	}
	page := &Page{ObjectiveID: objective, Signals: []Row{}, ViewerCanFeedback: canFeedback}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+rowColumns+`, `+keyExpr+rowFrom+where+` ORDER BY `+order+fmt.Sprintf(` LIMIT %d`, limit+1), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		var keys []int64
		for rows.Next() {
			var key int64
			r, err := s.scanRow(scanAppend{rows, &key})
			if err != nil {
				return err
			}
			page.Signals = append(page.Signals, *r)
			keys = append(keys, key)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(page.Signals) > limit {
			last := page.Signals[limit-1]
			page.Signals = page.Signals[:limit]
			page.NextCursor = encodeCursor(cursor{Binding: bind, Key: keys[limit-1], Created: last.CreatedAt, ID: last.ID})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

// scanAppend 在行扫描的目标后追加一个排序键。
type scanAppend struct {
	row pgx.Row
	key *int64
}

func (s scanAppend) Scan(dest ...any) error { return s.row.Scan(append(dest, s.key)...) }

// BatchItemInput 是批量清单中的一项（契约 BatchItemInput）。
type BatchItemInput struct {
	SignalID        string `json:"signal_id"`
	SeenContent     int64  `json:"seen_content_version"`
	StateVersion    int64  `json:"state_version"`
	FeedbackVersion int64  `json:"feedback_version"`
}

// Selection 是“全部符合筛选”的固定清单（契约 SignalSelection）。
type Selection struct {
	Total        int              `json:"total"`
	ExceedsLimit bool             `json:"exceeds_limit"`
	Items        []BatchItemInput `json:"items"`
}

// SelectSignals 返回符合筛选的全部信号标识与选中时的版本（契约 selectSignals）：超过 200 条时不截断，items 为空并提示收窄筛选。
func (s *Service) SelectSignals(ctx context.Context, objective string, f Filter) (*Selection, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	canFeedback, err := s.objectiveAccess(ctx, objective)
	if err != nil {
		return nil, err
	}
	if !canFeedback {
		return nil, failure(403, "forbidden")
	}
	args := []any{rc.Principal.ID}
	where := s.where(objective, f, &args)
	out := &Selection{Items: []BatchItemInput{}}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*)`+rowFrom+where, args...).Scan(&out.Total); err != nil {
			return err
		}
		if out.Total > batchLimit {
			out.ExceedsLimit = true
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT s.id, s.content_version, s.state_version, COALESCE(f.version, 0)`+rowFrom+where+
			` ORDER BY s.score DESC, s.created_at DESC, s.id DESC`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it BatchItemInput
			if err := rows.Scan(&it.SignalID, &it.SeenContent, &it.StateVersion, &it.FeedbackVersion); err != nil {
				return err
			}
			out.Items = append(out.Items, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readSignal 读取信号并按目标的 can_view_signals 授权：不存在、其他租户（行级安全）与无权查看同为 404 signal_not_found。
func (s *Service) readSignal(ctx context.Context, id string) (*Row, bool, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, false, err
	}
	var r *Row
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = s.scanRow(tx.QueryRow(ctx, `SELECT `+rowColumns+rowFrom+` WHERE s.id = $2`, rc.Principal.ID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(404, "signal_not_found")
		}
		return err
	})
	if err != nil {
		return nil, false, err
	}
	visible, granted, err := s.access(ctx, r.ObjectiveID, relViewSignals, relFeedback)
	if err != nil {
		return nil, false, err
	}
	if !visible || !has(granted, relViewSignals) {
		return nil, false, failure(404, "signal_not_found")
	}
	return r, has(granted, relFeedback), nil
}

// ViewItem 是视图快照中的一项（契约 Explanation.inputs.view_items）。
type ViewItem struct {
	Name    string `json:"name"`
	ID      string `json:"id,omitempty"`
	Version int64  `json:"version,omitempty"`
	Domain  string `json:"domain,omitempty"`
}

// Inputs 是固定的求值输入（契约 Explanation.inputs）。
type Inputs struct {
	Subject           SubjectRef `json:"subject"`
	ViewSnapshotHash  string     `json:"view_snapshot_hash"`
	ViewItems         []ViewItem `json:"view_items"`
	ContextVersion    int64      `json:"context_snapshot_version"`
	OrgUnitID         string     `json:"org_unit_id"`
	Policy            PolicyRef  `json:"policy"`
	EvaluatedAt       time.Time  `json:"evaluated_at"`
	CreatedAt         time.Time  `json:"created_at"`
	TriggerEventID    string     `json:"trigger_event_id"`
	SnapshotAvailable bool       `json:"snapshot_available"`
}

// Explanation 是命中理由（契约 Explanation），基于信号记录，不重新求值。
type Explanation struct {
	SignalID    string          `json:"signal_id"`
	Disposition string          `json:"disposition"`
	NeedsReview bool            `json:"needs_review"`
	Score       int             `json:"score"`
	ReasonCode  string          `json:"reason_code,omitempty"`
	Conclusion  string          `json:"conclusion"`
	Trace       json.RawMessage `json:"trace"`
	Features    json.RawMessage `json:"features"`
	Inputs      Inputs          `json:"inputs"`
}

func (s *Service) explanation(ctx context.Context, tx pgx.Tx, r *Row) (*Explanation, error) {
	e := &Explanation{SignalID: r.ID, Disposition: r.Disposition, NeedsReview: r.NeedsReview, Score: r.Score, ReasonCode: r.ReasonCode,
		Conclusion: r.conclusion, Trace: r.trace, Features: r.features, Inputs: Inputs{Subject: r.Subject, ViewSnapshotHash: r.hash,
			ViewItems: []ViewItem{}, ContextVersion: r.contextVersion, OrgUnitID: r.orgUnit, Policy: r.Policy, EvaluatedAt: r.EvaluatedAt,
			CreatedAt: r.CreatedAt, TriggerEventID: r.trigger}}
	var content []byte
	err := tx.QueryRow(ctx, `SELECT content FROM det_view_snapshots WHERE hash = $1`, r.hash).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, nil // 快照已过保存期：调用方标“历史不可用”
	}
	if err != nil {
		return nil, err
	}
	var v view
	if err := json.Unmarshal(content, &v); err != nil {
		return nil, err
	}
	e.Inputs.SnapshotAvailable = true
	for _, it := range v.Items {
		e.Inputs.ViewItems = append(e.Inputs.ViewItems, ViewItem{Name: it.Name, ID: it.ID, Version: it.Version, Domain: it.Domain})
	}
	return e, nil
}

// ExplainSignal 返回命中理由与固定的求值输入（契约 explainSignal）。
func (s *Service) ExplainSignal(ctx context.Context, id string) (*Explanation, error) {
	r, _, err := s.readSignal(ctx, id)
	if err != nil {
		return nil, err
	}
	var out *Explanation
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.explanation(ctx, tx, r)
		return err
	})
	return out, err
}

// MemberFeedback 是一名成员的当前反馈（契约 MemberFeedback）。
type MemberFeedback struct {
	Member          string    `json:"member"`
	Feedback        Feedback  `json:"feedback"`
	FeedbackVersion int64     `json:"feedback_version"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Update 是一条更新记录（契约 SignalUpdate）。
type Update struct {
	ContentVersion    int64     `json:"content_version"`
	Trigger           string    `json:"trigger"`
	ChangedFields     []string  `json:"changed_fields"`
	DispositionBefore string    `json:"disposition_before"`
	DispositionAfter  string    `json:"disposition_after"`
	Material          bool      `json:"material"`
	RecordedAt        time.Time `json:"recorded_at"`
}

// Record 是一条处理与反馈记录（契约 TriageRecord）。
type Record struct {
	OperationID string          `json:"operation_id"`
	Actor       string          `json:"actor"`
	Action      string          `json:"action"`
	Parts       json.RawMessage `json:"parts"`
	Reason      string          `json:"reason,omitempty"`
	SeenContent int64           `json:"seen_content_version"`
	Channel     string          `json:"channel"`
	BatchID     string          `json:"batch_id,omitempty"`
	UndoOf      string          `json:"undo_of,omitempty"`
	OccurredAt  time.Time       `json:"occurred_at"`
	seq         int64
}

// Detail 是信号详情（契约 SignalDetail）。
type Detail struct {
	Signal            Row              `json:"signal"`
	Explanation       *Explanation     `json:"explanation"`
	ManualReason      string           `json:"manual_reason,omitempty"`
	Feedback          []MemberFeedback `json:"feedback"`
	FeedbackTruncated bool             `json:"feedback_truncated"`
	Updates           []Update         `json:"updates"`
	UpdatesTruncated  bool             `json:"updates_truncated"`
	Records           []Record         `json:"records"`
	RecordsTruncated  bool             `json:"records_truncated"`
	ViewerCanFeedback bool             `json:"viewer_can_feedback"`
}

func (s *Service) records(ctx context.Context, tx pgx.Tx, signal string, before int64, limit int) ([]Record, bool, error) {
	cond := ""
	args := []any{signal}
	if before > 0 {
		cond = " AND seq < $2"
		args = append(args, before)
	}
	rows, err := tx.Query(ctx, `SELECT seq, operation_id, actor, action, parts, reason, seen_content_version, channel, batch_id, undo_of, occurred_at
 FROM det_triage_ops WHERE signal_id = $1 AND outcome = 'succeeded'`+cond+fmt.Sprintf(` ORDER BY seq DESC LIMIT %d`, limit+1), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var r Record
		var reason, batch, undo *string
		if err := rows.Scan(&r.seq, &r.OperationID, &r.Actor, &r.Action, &r.Parts, &reason, &r.SeenContent, &r.Channel, &batch, &undo, &r.OccurredAt); err != nil {
			return nil, false, err
		}
		if reason != nil {
			r.Reason = *reason
		}
		if batch != nil {
			r.BatchID = *batch
		}
		if undo != nil {
			r.UndoOf = *undo
		}
		r.OccurredAt = r.OccurredAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// GetSignal 返回信号详情（契约 getSignal）。
func (s *Service) GetSignal(ctx context.Context, id string) (*Detail, error) {
	r, canFeedback, err := s.readSignal(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &Detail{Signal: *r, ManualReason: r.manualReason, Feedback: []MemberFeedback{}, Updates: []Update{}, ViewerCanFeedback: canFeedback}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if d.Explanation, err = s.explanation(ctx, tx, r); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT member, result, reasons, note, based_on_content_version, version, updated_at FROM det_feedback
 WHERE signal_id = $1 AND result IS NOT NULL ORDER BY updated_at DESC, member LIMIT %d`, feedbackShow+1), id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var m MemberFeedback
			var note *string
			if err := rows.Scan(&m.Member, &m.Feedback.Result, &m.Feedback.Reasons, &note, &m.Feedback.BasedOn, &m.FeedbackVersion, &m.UpdatedAt); err != nil {
				rows.Close()
				return err
			}
			if note != nil {
				m.Feedback.Note = *note
			}
			if m.Feedback.Reasons == nil {
				m.Feedback.Reasons = []string{}
			}
			m.UpdatedAt = m.UpdatedAt.UTC()
			d.Feedback = append(d.Feedback, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(d.Feedback) > feedbackShow {
			d.Feedback, d.FeedbackTruncated = d.Feedback[:feedbackShow], true
		}
		rows, err = tx.Query(ctx, fmt.Sprintf(`SELECT content_version, trigger, changed_fields, disposition_before, disposition_after, material, recorded_at
 FROM det_signal_updates WHERE signal_id = $1 ORDER BY content_version DESC LIMIT %d`, detailLimit+1), id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var u Update
			if err := rows.Scan(&u.ContentVersion, &u.Trigger, &u.ChangedFields, &u.DispositionBefore, &u.DispositionAfter, &u.Material, &u.RecordedAt); err != nil {
				rows.Close()
				return err
			}
			if u.ChangedFields == nil {
				u.ChangedFields = []string{}
			}
			u.RecordedAt = u.RecordedAt.UTC()
			d.Updates = append(d.Updates, u)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(d.Updates) > detailLimit {
			d.Updates, d.UpdatesTruncated = d.Updates[:detailLimit], true
		}
		d.Records, d.RecordsTruncated, err = s.records(ctx, tx, id, 0, detailLimit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// RecordPage 是一页处理记录（契约 TriageRecordPage）。
type RecordPage struct {
	Records    []Record `json:"records"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

// ListTriage 分页列出信号的处理与反馈记录（契约 listSignalTriage），游标绑定信号。
func (s *Service) ListTriage(ctx context.Context, id, after string) (*RecordPage, error) {
	bind := binding("triage", id)
	pos, err := decodeCursor(after, bind)
	if err != nil {
		return nil, err
	}
	if _, _, err := s.readSignal(ctx, id); err != nil {
		return nil, err
	}
	var before int64
	if pos != nil {
		before = pos.Key
	}
	out := &RecordPage{}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var more bool
		var err error
		out.Records, more, err = s.records(ctx, tx, id, before, detailLimit)
		if err == nil && more {
			last := out.Records[len(out.Records)-1]
			out.NextCursor = encodeCursor(cursor{Binding: bind, Key: last.seq, Created: last.OccurredAt, ID: last.OperationID})
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
