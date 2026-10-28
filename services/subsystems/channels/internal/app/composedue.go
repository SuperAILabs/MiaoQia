package app

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
)

// ComposeDue 是 Briefings.ComposeDue（调度器入口）：为本租户已绑定企业微信的成员中、在 at 时已到计划发送时间且本期尚未组装的成员
// 组装简报。“到期”按成员自己的偏好与时区判断：at 时最近一个已到计划发送时间的周期，其终点与 at 在成员时区中是同一天（发送日只在
// 偏好的发送日；更早的、已错过的周期不补组装，由下一期覆盖——窗口不前移）。之后的规则都由 Compose 执行：成员不是有效成员时跳过；
// 当日 12:00 之后只记“迟到”、不推送；空简报按偏好决定是否推送；投递在安静时段内延后到结束。幂等：同一成员、同一期只组装一次
// （已组装的不计入 limit）。没有有效绑定的成员不在这里组装（只在 Web 与站内，由本人打开时读取）。每次至多为 limit 名成员组装；
// More 为还有到期未组装的成员（再次调用继续）。
func (s *Service) ComposeDue(ctx context.Context, at time.Time, limit int) (*api.ComposeDueResult, error) {
	rc, err := requireService(ctx)
	if err != nil {
		return nil, err
	}
	if s.Packs == nil || at.IsZero() {
		return nil, failure(400, "invalid_request")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	at = at.UTC().Truncate(time.Second)
	sctx := serviceContext(ctx, rc.Tenant, rc.Cell)
	out := &api.ComposeDueResult{}
	type dueMember struct {
		subject string
		end     time.Time
	}
	var due []dueMember
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT subject FROM chn_bindings WHERE tenant_id = $1 AND state = 'bound' ORDER BY subject`, rc.Tenant)
		if err != nil {
			return err
		}
		subjects, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, subject := range subjects {
			sched, err := s.schedule(ctx, tx, rc.Tenant, subject)
			if err != nil {
				return err
			}
			start, end, prefs, ok := sched.PeriodAt(at)
			if !ok || !sameLocalDay(end, at, prefs.Location()) {
				continue
			}
			var done int
			if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM chn_briefings WHERE tenant_id = $1 AND subject = $2 AND source = $3 AND period_start = $4 AND period_end = $5)
 + (SELECT count(*) FROM chn_compose_marks WHERE tenant_id = $1 AND subject = $2 AND source = $3 AND period_end = $5)`,
				rc.Tenant, subject, s.Packs.Source, start, end).Scan(&done); err != nil {
				return err
			}
			if done > 0 {
				out.Existing++
				continue
			}
			if len(due) == limit {
				out.More = true
				break
			}
			due = append(due, dueMember{subject, end})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, d := range due {
		subject := d.subject
		res, err := s.Compose(sctx, subject, at)
		if err != nil {
			out.Failed++
			s.logger().WarnContext(sctx, "到期简报组装失败", "subject", subject, "error", err.Error())
			continue
		}
		switch res.Outcome {
		case "composed":
			out.Composed++
		case "existing":
			out.Existing++
		case "late":
			out.Late++
		default:
			out.Skipped++
			// 没有生成简报：记下本期已处理，同一期不再组装
			err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO chn_compose_marks (tenant_id, subject, source, period_end, outcome, created_at) VALUES ($1, $2, $3, $4, 'skipped', $5)
 ON CONFLICT DO NOTHING`, rc.Tenant, subject, s.Packs.Source, d.end, s.businessNow())
				return err
			})
			if err != nil {
				s.logger().WarnContext(sctx, "到期组装标记写入失败", "subject", subject, "error", err.Error())
			}
		}
	}
	return out, nil
}

func sameLocalDay(a, b time.Time, loc *time.Location) bool {
	x, y := a.In(loc), b.In(loc)
	return x.Year() == y.Year() && x.YearDay() == y.YearDay()
}
