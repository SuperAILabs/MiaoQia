package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/outbox"
)

// OutcomeRecorded 是 pursuit.outcome.recorded.v1 的消费处理（PRD 第6段 F7 第5条）：以结果记录标识幂等写入本租户的结果集，
// 有获知时间的计入时点评估，缺失的只进入事后统计。
func (s *Service) OutcomeRecorded(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var d struct {
		OutcomeID    string          `json:"outcome_id"`
		PursuitID    string          `json:"pursuit_id"`
		ObjectiveID  string          `json:"objective_id"`
		TemplateID   string          `json:"template_id"`
		Result       string          `json:"result"`
		ReasonCode   string          `json:"reason_code"`
		KnownAtUTC   *time.Time      `json:"known_at_utc"`
		Contribution string          `json:"contribution"`
		Source       *string         `json:"source_signal_id"`
		Supersedes   *string         `json:"supersedes"`
		Amounts      json.RawMessage `json:"amounts"`
		RecordedAt   time.Time       `json:"recorded_at"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return err
	}
	if env.Tenant == "" {
		return fmt.Errorf("learning：结果事件 %s 缺少租户", env.ID)
	}
	basis := "post_hoc"
	if d.KnownAtUTC != nil {
		basis = "point_in_time"
	}
	_, err := tx.Exec(ctx, `INSERT INTO lrn_outcomes (outcome_id, tenant_id, pursuit_id, objective_id, template_id, result, reason_code, known_at_utc, basis,
 contribution, source_signal_id, supersedes, amounts, recorded_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT (outcome_id) DO NOTHING`,
		d.OutcomeID, env.Tenant, d.PursuitID, d.ObjectiveID, d.TemplateID, d.Result, d.ReasonCode, d.KnownAtUTC, basis, d.Contribution, d.Source, d.Supersedes,
		d.Amounts, d.RecordedAt)
	return err
}
