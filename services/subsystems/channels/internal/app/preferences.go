package app

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	chn "github.com/superailabs/miaoqia/services/contracts/apis/channels"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/domain"
)

// 简报偏好（PRD 第5段 F9）：每次修改追加一个版本，新版本从“修改时所在周期”的下一期生效（F1 第2条），简报记录所用的偏好版本。
// 例外：即时通知开关 instant_notify（C-08）按最新版本立即生效，即时通知不按周期发送。

// defaults 返回缺省偏好，各目标区条数取应用领域包的缺省值。
func (s *Service) defaults() domain.Preferences {
	p := domain.Defaults()
	if s.Packs != nil {
		for code, sec := range s.Packs.Sections {
			p.SectionLimits[code] = sec.Quota.Default
		}
	}
	return p
}

// schedule 读取成员的偏好版本序列（版本 0 为缺省值，自始生效）。
func (s *Service) schedule(ctx context.Context, tx pgx.Tx, tenant, subject string) (domain.Schedule, error) {
	out := domain.Schedule{s.defaults()}
	rows, err := tx.Query(ctx, `SELECT version, send_time, timezone, send_days, quiet_start, quiet_end, push_empty, section_limits, effective_from, instant_notify
 FROM chn_briefing_preferences WHERE tenant_id = $1 AND subject = $2 ORDER BY version`, tenant, subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p := s.defaults()
		var limits []byte
		var eff time.Time
		var days []int32
		if err := rows.Scan(&p.Version, &p.SendTime, &p.Timezone, &days, &p.QuietStart, &p.QuietEnd, &p.PushEmpty, &limits, &eff, &p.InstantNotify); err != nil {
			return nil, err
		}
		p.SendDays = nil
		for _, d := range days {
			p.SendDays = append(p.SendDays, int(d))
		}
		custom := map[string]int{}
		if err := json.Unmarshal(limits, &custom); err != nil {
			return nil, err
		}
		for k, v := range custom {
			p.SectionLimits[k] = v
		}
		eff = eff.UTC()
		p.EffectiveFrom = &eff
		out = append(out, p)
	}
	return out, rows.Err()
}

func prefsOut(p domain.Preferences, updated *time.Time) *chn.BriefingPreferences {
	out := &chn.BriefingPreferences{Version: p.Version, SendTime: chn.ClockTime(p.SendTime), Timezone: p.Timezone, QuietStart: chn.ClockTime(p.QuietStart),
		QuietEnd: chn.ClockTime(p.QuietEnd), PushEmpty: p.PushEmpty, SendDays: []int{}, SectionLimits: []chn.SectionLimit{},
		InstantNotify: chn.NewOptBool(p.InstantNotify)}
	out.SendDays = append(out.SendDays, p.SendDays...)
	codes := make([]string, 0, len(p.SectionLimits))
	for c := range p.SectionLimits {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		out.SectionLimits = append(out.SectionLimits, chn.SectionLimit{ObjectiveCode: c, Limit: p.SectionLimits[c]})
	}
	if p.EffectiveFrom != nil {
		out.EffectiveFrom = chn.NewOptDateTime(*p.EffectiveFrom)
	}
	if updated != nil {
		out.UpdatedAt = chn.NewOptDateTime(*updated)
	}
	return out
}

// GetPreferences 返回本人的当前偏好（最新版本；从未保存时为缺省值）。
func (s *Service) GetPreferences(ctx context.Context) (*chn.BriefingPreferences, error) {
	rc, err := s.member(ctx)
	if err != nil {
		return nil, err
	}
	var out *chn.BriefingPreferences
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		sched, err := s.schedule(ctx, tx, rc.Tenant, rc.Principal.ID)
		if err != nil {
			return err
		}
		latest := sched[len(sched)-1]
		var updated *time.Time
		if latest.Version > 0 {
			var at time.Time
			if err := tx.QueryRow(ctx, `SELECT created_at FROM chn_briefing_preferences WHERE tenant_id = $1 AND subject = $2 AND version = $3`,
				rc.Tenant, rc.Principal.ID, latest.Version).Scan(&at); err != nil {
				return err
			}
			at = at.UTC()
			updated = &at
		}
		out = prefsOut(latest, updated)
		return nil
	})
	return out, err
}

// UpdatePreferences 修改本人的偏好（按 expected_version 并发控制，按 Idempotency-Key 幂等）。新版本的生效起点为旧偏好下修改时刻之后的
// 第一个计划发送时间（新旧周期首尾相接，F1 第2条）。写入偏好版本与审计。
func (s *Service) UpdatePreferences(ctx context.Context, req *chn.UpdateBriefingPreferencesRequest, key string) (*chn.BriefingPreferences, error) {
	if req == nil || req.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, err := s.member(ctx)
	if err != nil {
		return nil, err
	}
	p := s.defaults()
	p.SendTime, p.Timezone, p.QuietStart, p.QuietEnd, p.PushEmpty = string(req.SendTime), req.Timezone, string(req.QuietStart), string(req.QuietEnd), req.PushEmpty
	p.SendDays = append([]int(nil), req.SendDays...)
	sort.Ints(p.SendDays)
	custom := map[string]int{}
	for _, l := range req.SectionLimits {
		if _, dup := custom[l.ObjectiveCode]; dup {
			return nil, failure(422, "invalid_preferences")
		}
		custom[l.ObjectiveCode] = l.Limit
	}
	check := p
	check.SectionLimits = custom
	limits := map[string][2]int{}
	if s.Packs != nil {
		limits = s.Packs.Limits()
	}
	if domain.ValidatePreferences(check, limits) != nil {
		return nil, failure(422, "invalid_preferences")
	}
	for k, v := range custom {
		p.SectionLimits[k] = v
	}
	limitsJSON, _ := json.Marshal(custom)
	out := &chn.BriefingPreferences{}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// 同一成员的偏好修改串行化
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "chn-prefs/"+rc.Tenant+"/"+rc.Principal.ID); err != nil {
			return err
		}
		_, err := command(ctx, tx, key, "updateBriefingPreferences", req, out, func() error {
			sched, err := s.schedule(ctx, tx, rc.Tenant, rc.Principal.ID)
			if err != nil {
				return err
			}
			current := sched[len(sched)-1].Version
			if req.ExpectedVersion != current {
				return failure(409, "version_conflict")
			}
			// 即时通知开关（C-08）：不提供时保持当前值
			p.InstantNotify = req.InstantNotify.Or(sched[len(sched)-1].InstantNotify)
			now := s.businessNow()
			eff := sched.NextBoundary(now)
			if eff.IsZero() {
				return failure(422, "invalid_preferences")
			}
			eff = eff.UTC()
			p.Version, p.EffectiveFrom = current+1, &eff
			days := make([]int32, len(p.SendDays))
			for i, d := range p.SendDays {
				days[i] = int32(d)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO chn_briefing_preferences (tenant_id, subject, version, send_time, timezone, send_days, quiet_start, quiet_end,
 push_empty, section_limits, effective_from, created_at, instant_notify) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
				rc.Tenant, rc.Principal.ID, p.Version, p.SendTime, p.Timezone, days, p.QuietStart, p.QuietEnd, p.PushEmpty, limitsJSON, eff, now, p.InstantNotify); err != nil {
				return err
			}
			*out = *prefsOut(p, &now)
			return record(ctx, tx, "channels.briefing_preferences.updated", rc.Principal.ID, "member.self")
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
