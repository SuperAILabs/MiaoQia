package app

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/outbox"
)

// 反馈集与反馈汇总（PRD 第3段 F7；learning.md 第4、5节）。反馈只用于评估与改进建议，不创建草案或提案，不修改策略。

const (
	summaryWindow = 30 * 24 * time.Hour // 汇总窗口：发现时间在统计截至时间之前 30 天内
	minSample     = 20                  // 样本少于 20 时不给出比例（PRD 第3段第13节第9项）
)

// ReasonHint 是反馈原因标签可能相关的规则与参数（应用领域包 feedback/*.yaml 的 relates，只作提示）。
type ReasonHint struct {
	Rules  []string
	Params []string
}

// SignalDetected 是 detection.signal.detected.v1 的消费处理：登记信号的汇总口径字段。
func (s *Service) SignalDetected(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var d struct {
		SignalID      string    `json:"signal_id"`
		ObjectiveID   string    `json:"objective_id"`
		ObjectiveCode string    `json:"objective_code"`
		Disposition   string    `json:"disposition"`
		CreatedAt     time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO lrn_signals (tenant_id, signal_id, objective_id, objective_code, disposition, discovered_at) VALUES ($1,$2,$3,$4,$5,$6)
 ON CONFLICT (signal_id) DO UPDATE SET objective_code = EXCLUDED.objective_code`, env.Tenant, d.SignalID, d.ObjectiveID, d.ObjectiveCode, d.Disposition, d.CreatedAt)
	return err
}

type eventFeedback struct {
	Result  string   `json:"result"`
	Reasons []string `json:"reasons"`
	BasedOn int64    `json:"based_on_content_version"`
}

// SignalTriaged 是 detection.signal.triaged.v1 的消费处理：以操作标识幂等写入成员的当前反馈（只接受版本更高的变化，乱序不回退）。
func (s *Service) SignalTriaged(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var d struct {
		OperationID    string    `json:"operation_id"`
		SignalID       string    `json:"signal_id"`
		ObjectiveID    string    `json:"objective_id"`
		Actor          string    `json:"actor"`
		ContentVersion int64     `json:"content_version"`
		Disposition    string    `json:"disposition"`
		SignalCreated  time.Time `json:"signal_created_at"`
		OccurredAt     time.Time `json:"occurred_at"`
		Parts          struct {
			Feedback *struct {
				After   *eventFeedback `json:"after"`
				Version int64          `json:"version"`
			} `json:"feedback"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO lrn_feedback_ops (tenant_id, operation_id, signal_id, occurred_at) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		env.Tenant, d.OperationID, d.SignalID, d.OccurredAt)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO lrn_signals (tenant_id, signal_id, objective_id, disposition, discovered_at, content_version) VALUES ($1,$2,$3,$4,$5,$6)
 ON CONFLICT (signal_id) DO UPDATE SET content_version = GREATEST(lrn_signals.content_version, EXCLUDED.content_version)`,
		env.Tenant, d.SignalID, d.ObjectiveID, d.Disposition, d.SignalCreated, d.ContentVersion); err != nil {
		return err
	}
	f := d.Parts.Feedback
	if f == nil {
		return nil
	}
	var result *string
	reasons, based := []string{}, int64(0)
	if f.After != nil {
		result, based = &f.After.Result, f.After.BasedOn
		if f.After.Reasons != nil {
			reasons = f.After.Reasons
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO lrn_feedback (tenant_id, signal_id, member, result, reasons, based_on_content_version, version, recorded_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (signal_id, member) DO UPDATE SET result = EXCLUDED.result, reasons = EXCLUDED.reasons,
 based_on_content_version = EXCLUDED.based_on_content_version, version = EXCLUDED.version, recorded_at = EXCLUDED.recorded_at
 WHERE lrn_feedback.version < EXCLUDED.version`, env.Tenant, d.SignalID, d.Actor, result, reasons, based, f.Version, d.OccurredAt)
	return err
}

// ReasonCount 是一个原因标签的信号数（契约 FeedbackReasonCount）。
type ReasonCount struct {
	Result  string   `json:"result"`
	Code    string   `json:"code"`
	Signals int      `json:"signals"`
	Rules   []string `json:"rules"`
	Params  []string `json:"params"`
}

// SignalCounts、Relevance 与 Exclusions 是汇总的各部分。
type SignalCounts struct {
	Total           int      `json:"total"`
	WithFeedback    int      `json:"with_feedback"`
	WithoutFeedback int      `json:"without_feedback"`
	FeedbackRate    *float64 `json:"feedback_rate,omitempty"`
}

type Relevance struct {
	Relevant          int      `json:"relevant"`
	Irrelevant        int      `json:"irrelevant"`
	Disagreement      int      `json:"disagreement"`
	RelevantRatio     *float64 `json:"relevant_ratio,omitempty"`
	IrrelevantRatio   *float64 `json:"irrelevant_ratio,omitempty"`
	DisagreementRatio *float64 `json:"disagreement_ratio,omitempty"`
}

type Exclusions struct {
	Total        int `json:"total"`
	WithFeedback int `json:"with_feedback"`
	Correct      int `json:"correct"`
	Wrong        int `json:"wrong"`
	Disagreement int `json:"disagreement"`
}

// Summary 是反馈汇总（契约 FeedbackSummary）。
type Summary struct {
	ObjectiveID      string        `json:"objective_id"`
	AsOf             time.Time     `json:"as_of"`
	WindowStart      time.Time     `json:"window_start"`
	WindowEnd        time.Time     `json:"window_end"`
	LastFeedbackAt   *time.Time    `json:"last_feedback_at,omitempty"`
	MinSample        int           `json:"min_sample"`
	Sample           int           `json:"sample"`
	SampleSufficient bool          `json:"sample_sufficient"`
	Signals          SignalCounts  `json:"signals"`
	Relevance        Relevance     `json:"relevance"`
	StaleFeedback    int           `json:"stale_feedback"`
	Exclusions       Exclusions    `json:"exclusions"`
	Reasons          []ReasonCount `json:"reasons"`
}

func ratio(n, d int) *float64 {
	v := math.Round(float64(n)/float64(d)*10000) / 10000
	return &v
}

// GetFeedbackSummary 计算目标近 30 天的反馈汇总（契约 getFeedbackSummary；目标的 can_view_signals）。
func (s *Service) GetFeedbackSummary(ctx context.Context, objective string) (*Summary, error) {
	rc, err := tenantContext(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Principal.Kind != "user" {
		return nil, failure(403, "forbidden")
	}
	visible, ok, err := s.access(ctx, objective, "can_view_signals")
	if err != nil {
		return nil, err
	}
	if !visible {
		return nil, failure(404, "objective_not_found")
	}
	if !ok {
		return nil, failure(403, "forbidden")
	}
	asOf := s.businessNow()
	out := &Summary{ObjectiveID: objective, AsOf: asOf, WindowStart: asOf.Add(-summaryWindow), WindowEnd: asOf, MinSample: minSample, Reasons: []ReasonCount{}}
	type member struct {
		result  string
		reasons []string
		current bool
	}
	type signal struct {
		excluded bool
		members  []member
	}
	signals := map[string]*signal{}
	var order []string
	var code string
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT s.signal_id, s.disposition, COALESCE(s.objective_code, ''), f.result, f.reasons, f.based_on_content_version = s.content_version, f.recorded_at
 FROM lrn_signals s LEFT JOIN lrn_feedback f ON f.signal_id = s.signal_id AND f.result IS NOT NULL
 WHERE s.objective_id = $1 AND s.discovered_at >= $2 AND s.discovered_at < $3 ORDER BY s.signal_id`, objective, out.WindowStart, out.WindowEnd)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, disposition, c string
			var result *string
			var reasons []string
			var current *bool
			var recorded *time.Time
			if err := rows.Scan(&id, &disposition, &c, &result, &reasons, &current, &recorded); err != nil {
				return err
			}
			if c != "" {
				code = c
			}
			sg, ok := signals[id]
			if !ok {
				sg = &signal{excluded: disposition == "excluded"}
				signals[id] = sg
				order = append(order, id)
			}
			if result != nil {
				sg.members = append(sg.members, member{result: *result, reasons: reasons, current: current != nil && *current})
				if out.LastFeedbackAt == nil || recorded.After(*out.LastFeedbackAt) {
					t := recorded.UTC()
					out.LastFeedbackAt = &t
				}
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	reasonSignals := map[[2]string]int{}
	for _, id := range order {
		sg := signals[id]
		has := map[string]bool{}
		reasons := map[[2]string]bool{}
		for _, m := range sg.members {
			if !m.current {
				out.StaleFeedback++
				continue
			}
			has[m.result] = true
			if m.result == "irrelevant" || m.result == "exclusion_wrong" {
				for _, r := range m.reasons {
					reasons[[2]string{m.result, r}] = true
				}
			}
		}
		if sg.excluded {
			out.Exclusions.Total++
			switch {
			case has["exclusion_correct"] && has["exclusion_wrong"]:
				out.Exclusions.Disagreement++
			case has["exclusion_correct"]:
				out.Exclusions.Correct++
			case has["exclusion_wrong"]:
				out.Exclusions.Wrong++
			}
			if has["exclusion_correct"] || has["exclusion_wrong"] {
				out.Exclusions.WithFeedback++
			}
			if has["exclusion_wrong"] && !has["exclusion_correct"] {
				for k := range reasons {
					reasonSignals[k]++
				}
			}
			continue
		}
		out.Signals.Total++
		switch {
		case has["relevant"] && has["irrelevant"]:
			out.Relevance.Disagreement++
		case has["relevant"]:
			out.Relevance.Relevant++
		case has["irrelevant"]:
			out.Relevance.Irrelevant++
		}
		if has["relevant"] || has["irrelevant"] {
			out.Signals.WithFeedback++
		}
		if has["irrelevant"] && !has["relevant"] {
			for k := range reasons {
				reasonSignals[k]++
			}
		}
	}
	out.Signals.WithoutFeedback = out.Signals.Total - out.Signals.WithFeedback
	r := out.Relevance
	out.Sample = r.Relevant + r.Irrelevant + r.Disagreement
	out.SampleSufficient = out.Sample >= minSample
	if out.SampleSufficient {
		out.Signals.FeedbackRate = ratio(out.Signals.WithFeedback, out.Signals.Total)
		out.Relevance.RelevantRatio = ratio(r.Relevant, out.Sample)
		out.Relevance.IrrelevantRatio = ratio(r.Irrelevant, out.Sample)
		out.Relevance.DisagreementRatio = ratio(r.Disagreement, out.Sample)
	}
	for k, n := range reasonSignals {
		hint := s.Hints[code+"/"+k[0]+"/"+k[1]]
		rc := ReasonCount{Result: k[0], Code: k[1], Signals: n, Rules: []string{}, Params: []string{}}
		if hint != nil {
			rc.Rules, rc.Params = hint.Rules, hint.Params
		}
		out.Reasons = append(out.Reasons, rc)
	}
	slices.SortFunc(out.Reasons, func(a, b ReasonCount) int {
		if a.Signals != b.Signals {
			return b.Signals - a.Signals
		}
		return strings.Compare(a.Result+a.Code, b.Result+b.Code)
	})
	if len(out.Reasons) > 64 {
		out.Reasons = out.Reasons[:64]
	}
	return out, nil
}
