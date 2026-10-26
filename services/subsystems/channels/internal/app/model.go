package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	purapi "github.com/superailabs/miaoqia/services/contracts/apis/pursuit"
	kbinding "github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	customercontext "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	detection "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	pursuit "github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// 简报快照的存储形状与契约 Briefing / BriefingSection / BriefingItem 一致（字段同名，经 JSON 转换为生成类型）。
// 条目只按最终展示级别逐字段白名单写入（displayField），不复制来源记录。

type displayField struct {
	Level     string `json:"level"`
	State     string `json:"state"`
	Value     string `json:"value,omitempty"`
	SourceURL string `json:"source_url,omitempty"`
}

type itemUpdate struct {
	RecordedAt        time.Time `json:"recorded_at"`
	ChangedFields     []string  `json:"changed_fields"`
	DispositionBefore string    `json:"disposition_before"`
	DispositionAfter  string    `json:"disposition_after"`
	ContentVersion    int64     `json:"content_version"`
}

type itemStatus struct {
	Status         string     `json:"status"`
	HandledBy      string     `json:"handled_by,omitempty"`
	HandledAt      *time.Time `json:"handled_at,omitempty"`
	ContentUpdated bool       `json:"content_updated"`
	StatusAsOf     time.Time  `json:"status_as_of"`
}

// SignalItem 是信号条目与引用行的字段。
type SignalItem struct {
	SignalID       string        `json:"signal_id"`
	ContentVersion int64         `json:"content_version"`
	Category       string        `json:"category"`
	Correction     bool          `json:"correction"`
	Score          int           `json:"score"`
	Disposition    string        `json:"disposition"`
	Project        *displayField `json:"project,omitempty"`
	Winner         *displayField `json:"winner,omitempty"`
	Amount         *displayField `json:"amount,omitempty"`
	Region         *displayField `json:"region,omitempty"`
	Categories     *displayField `json:"categories,omitempty"`
	Reasons        []string      `json:"reasons,omitempty"`
	UnknownCount   *int          `json:"unknown_count,omitempty"`
	SameLotCount   *int          `json:"same_lot_count,omitempty"`
	DiscoveredAt   *time.Time    `json:"discovered_at,omitempty"`
	Updates        []itemUpdate  `json:"updates,omitempty"`
	AlsoMatched    []string      `json:"also_matched,omitempty"`
	References     string        `json:"references,omitempty"`
}

// ApprovalItem 是“待我处理”中的审批。
type ApprovalItem struct {
	ApprovalID   string    `json:"approval_id"`
	ApprovalType string    `json:"approval_type"`
	SubmittedAt  time.Time `json:"submitted_at"`
}

// TaskItem 是“待我处理”中的跟进任务（PRD 第6段 F9 第1条）：分派给本人、未完成、截止在下一期简报之前或已逾期；Escalated 为无有效
// 负责人的跟进转给决策者的任务。
type TaskItem struct {
	TaskID      string   `json:"task_id"`
	PursuitID   string   `json:"pursuit_id"`
	ObjectiveID string   `json:"objective_id"`
	TaskTitle   string   `json:"task_title"`
	Due         *taskDue `json:"due,omitempty"`
	Escalated   bool     `json:"escalated"`
}

// taskDue 是契约 TaskDue。
type taskDue struct {
	Value     string `json:"value"`
	Precision string `json:"precision"`
	Timezone  string `json:"timezone"`
}

// taskStatus 是契约 TaskItemStatus（读取时叠加的当前状态）。
type taskStatus struct {
	Status     string    `json:"status"`
	StatusAsOf time.Time `json:"status_as_of"`
}

type item struct {
	Kind     string `json:"kind"`
	Position int    `json:"position"`
	*SignalItem
	*ApprovalItem
	*TaskItem
	Current     *itemStatus `json:"current,omitempty"`
	TaskCurrent *taskStatus `json:"task_current,omitempty"`
}

type dataAsOf struct {
	Domain string     `json:"domain"`
	State  string     `json:"state"`
	AsOf   *time.Time `json:"as_of,omitempty"`
}

// ObjectiveSection 是目标区的字段（“待我处理”区没有）。
type ObjectiveSection struct {
	ObjectiveID      string     `json:"objective_id"`
	ObjectiveCode    string     `json:"objective_code,omitempty"`
	Title            string     `json:"title,omitempty"`
	WindowStart      *time.Time `json:"window_start,omitempty"`
	WindowEnd        *time.Time `json:"window_end,omitempty"`
	IncludesPrevious bool       `json:"includes_previous"`
	NewCount         int        `json:"new_count"`
	UpdatedCount     int        `json:"updated_count"`
	HandledCount     int        `json:"handled_count"`
	BackfillCount    int        `json:"backfill_count"`
	OverflowCount    int        `json:"overflow_count"`
	Quota            int        `json:"quota"`
	DataAsOf         []dataAsOf `json:"data_as_of,omitempty"`
}

type section struct {
	Kind  string `json:"kind"`
	Order int    `json:"order"`
	State string `json:"state"`
	*ObjectiveSection
	Items []item `json:"items"`
}

// deliverySummary 是契约 DeliverySummary。
type deliverySummary struct {
	ID           string     `json:"id"`
	BriefingID   string     `json:"briefing_id"`
	Kind         string     `json:"kind"`
	State        string     `json:"state"`
	Attempts     int        `json:"attempts"`
	Reason       string     `json:"reason,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	SubmittedAt  *time.Time `json:"submitted_at,omitempty"`
	AcceptedAt   *time.Time `json:"accepted_at,omitempty"`
	ReadReceipts string     `json:"read_receipts"`
}

// period 是契约 Period。
type period struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Timezone string    `json:"timezone"`
}

// briefingDoc 是契约 Briefing。
type briefingDoc struct {
	ID                string            `json:"id"`
	Source            string            `json:"source"`
	Period            period            `json:"period"`
	AssembledAt       time.Time         `json:"assembled_at"`
	ScheduledAt       time.Time         `json:"scheduled_at"`
	Delayed           bool              `json:"delayed"`
	Empty             bool              `json:"empty"`
	Partial           bool              `json:"partial"`
	RuleVersion       string            `json:"rule_version"`
	PreferenceVersion int64             `json:"preference_version"`
	ContentAsOf       time.Time         `json:"content_as_of"`
	StatusAsOf        *time.Time        `json:"status_as_of,omitempty"`
	Sections          []section         `json:"sections"`
	Deliveries        []deliverySummary `json:"deliveries"`
}

// briefingSummary 是契约 BriefingSummary。
type briefingSummary struct {
	ID              string    `json:"id"`
	Period          period    `json:"period"`
	AssembledAt     time.Time `json:"assembled_at"`
	ScheduledAt     time.Time `json:"scheduled_at"`
	Delayed         bool      `json:"delayed"`
	Empty           bool      `json:"empty"`
	Partial         bool      `json:"partial"`
	ItemCount       int       `json:"item_count"`
	PushState       string    `json:"push_state"`
	NotPushedReason string    `json:"not_pushed_reason,omitempty"`
}

// convert 把用例类型转换为契约类型（字段同名，经 JSON 转换）。
func convert[T any](v any) (*T, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// 外部依赖的错误分类：transient 为暂时失败（未知错误、5xx、网络错误），按“不可用”处理，不当作“无权”或“不存在”。

func dependencyTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, kbinding.ErrTransient) {
		return true
	}
	var det *detection.Error
	if errors.As(err, &det) {
		return det.Status >= 500
	}
	var cc *customercontext.Error
	if errors.As(err, &cc) {
		return cc.Status >= 500
	}
	var id *identity.Error
	if errors.As(err, &id) {
		return id.Status >= 500
	}
	var pe *pursuit.Error
	if errors.As(err, &pe) {
		return pe.Status >= 500
	}
	var own *api.Error
	if errors.As(err, &own) {
		return own.Status >= 500
	}
	return true
}

// objectiveGrant 以成员本人判断目标访问：visible 为能查看信号（can_view_signals），feedback 为能反馈（can_feedback）。
// 不能查看与不存在同为不可见；判断不可用时返回错误（调用方失败关闭）。
func (s *Service) objectiveGrant(uctx context.Context, objective string) (visible, feedback bool, err error) {
	if s.Access == nil {
		return false, false, failure(503, "customer_context_unavailable")
	}
	granted, err := s.Access.Access(uctx, objective, []string{"can_view_signals", "can_feedback"})
	if err != nil {
		if !dependencyTransient(err) {
			return false, false, nil
		}
		return false, false, failure(503, "customer_context_unavailable")
	}
	return slices.Contains(granted, "can_view_signals"), slices.Contains(granted, "can_view_signals") && slices.Contains(granted, "can_feedback"), nil
}

// taskItem 把 pursuit listMyTasks 的一条转换为“待我处理”中的任务条目（标题按契约截断）。
func taskItem(t purapi.MyTask) *TaskItem {
	out := &TaskItem{TaskID: string(t.Task.ID), PursuitID: string(t.Task.PursuitID), ObjectiveID: string(t.ObjectiveID), Escalated: t.Escalated}
	title := []rune(t.Task.Title)
	if len(title) > 120 {
		title = title[:120]
	}
	out.TaskTitle = string(title)
	if d, ok := t.Task.Due.Get(); ok {
		out.Due = &taskDue{Value: d.Value, Precision: string(d.Precision), Timezone: d.Timezone}
	}
	return out
}
