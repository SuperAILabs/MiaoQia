package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// 关闭与结果（PRD 第6段 F7；第13节第8–10、12项）。结果类型、原因代码与金额类型取自目标所属应用领域包的结果定义
// （packs/application_domains/*/outcomes/*.yaml，按目标模板，例如 APP-TENDER.A02）；没有结果定义的目标不能关闭（422 invalid_outcome）。

// OutcomeDef 是一个目标模板的结果定义。
type OutcomeDef struct {
	Results     map[string]ResultDef
	AmountTypes []string
}

// ResultDef 是一种结果类型：获知时间是否必填与原因代码（是否须写说明）。
type ResultDef struct {
	KnownAtRequired bool
	Reasons         map[string]bool // 原因代码 → 须写说明
}

// LoadOutcomes 读取 dir 下全部应用领域包的结果定义，键为目标模板（outcome.objective）。
func LoadOutcomes(dir string) (map[string]*OutcomeDef, error) {
	files, err := filepath.Glob(filepath.Join(dir, "application_domains", "*", "outcomes", "*.yaml"))
	if err != nil {
		return nil, err
	}
	out := map[string]*OutcomeDef{}
	for _, f := range files {
		var doc struct {
			Outcome struct {
				Objective string `yaml:"objective"`
				Results   []struct {
					Type            string `yaml:"type"`
					KnownAtRequired bool   `yaml:"known_at_required"`
					Reasons         []struct {
						Code         string `yaml:"code"`
						NoteRequired bool   `yaml:"note_required"`
					} `yaml:"reasons"`
				} `yaml:"results"`
				AmountTypes []struct {
					Code string `yaml:"code"`
				} `yaml:"amount_types"`
			} `yaml:"outcome"`
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("%s：%w", f, err)
		}
		if doc.Outcome.Objective == "" {
			continue
		}
		def := &OutcomeDef{Results: map[string]ResultDef{}}
		for _, r := range doc.Outcome.Results {
			rd := ResultDef{KnownAtRequired: r.KnownAtRequired, Reasons: map[string]bool{}}
			for _, reason := range r.Reasons {
				rd.Reasons[reason.Code] = reason.NoteRequired
			}
			def.Results[r.Type] = rd
		}
		for _, a := range doc.Outcome.AmountTypes {
			def.AmountTypes = append(def.AmountTypes, a.Code)
		}
		out[doc.Outcome.Objective] = def
	}
	return out, nil
}

// Amount 是契约 Amount（十进制文本，不换算、不相加）。
type Amount struct {
	Type      string `json:"type"`
	Currency  string `json:"currency"`
	Value     string `json:"value"`
	Nature    string `json:"nature"`
	Source    string `json:"source"`
	SourceRef string `json:"source_ref,omitempty"`
}

// EventAnchor 是结果对应的事件与时间。
type EventAnchor struct {
	Kind    string    `json:"kind"`
	At      LocalTime `json:"at"`
	FactRef string    `json:"fact_ref,omitempty"`
	Basis   string    `json:"basis,omitempty"`
}

// OutcomeInput 是契约 OutcomeInput。
type OutcomeInput struct {
	Result       string       `json:"result"`
	ReasonCode   string       `json:"reason_code"`
	Note         string       `json:"note,omitempty"`
	KnownAt      *LocalTime   `json:"known_at,omitempty"`
	KnownChannel string       `json:"known_channel,omitempty"`
	Amounts      []Amount     `json:"amounts,omitempty"`
	EventAnchor  *EventAnchor `json:"event_anchor,omitempty"`
	Contribution string       `json:"contribution,omitempty"`
}

// OutcomeView 是契约 Outcome 的 JSON。
type OutcomeView struct {
	ID             string       `json:"id"`
	Result         string       `json:"result"`
	ReasonCode     string       `json:"reason_code"`
	Note           string       `json:"note,omitempty"`
	KnownAt        *LocalTime   `json:"known_at,omitempty"`
	KnownChannel   string       `json:"known_channel,omitempty"`
	Amounts        []Amount     `json:"amounts"`
	EventAnchor    *EventAnchor `json:"event_anchor,omitempty"`
	Contribution   string       `json:"contribution"`
	Supersedes     string       `json:"supersedes,omitempty"`
	RevisionReason string       `json:"revision_reason,omitempty"`
	RecordedBy     string       `json:"recorded_by"`
	RecordedAt     time.Time    `json:"recorded_at"`
}

func loadOutcome(ctx context.Context, tx pgx.Tx, id string) (*OutcomeView, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT content FROM pur_outcomes WHERE id = $1`, id).Scan(&raw); err != nil {
		return nil, err
	}
	var o OutcomeView
	return &o, json.Unmarshal(raw, &o)
}

// validateOutcome 按目标的结果定义校验（422 invalid_outcome；获知时间格式 422 invalid_time）：结果与原因必填，原因要求说明时须写说明；
// 成功与失败须有获知时间（可只到日）；金额条目的类型取自结果定义，币种与取值性质必填（契约已约束格式）。
func (s *Service) validateOutcome(p *Pursuit, in *OutcomeInput) error {
	def := s.Outcomes[p.TemplateID]
	if def == nil {
		return failure(422, "invalid_outcome")
	}
	rd, ok := def.Results[in.Result]
	if !ok {
		return failure(422, "invalid_outcome")
	}
	noteRequired, ok := rd.Reasons[in.ReasonCode]
	if !ok || (noteRequired && in.Note == "") || (rd.KnownAtRequired && in.KnownAt == nil) {
		return failure(422, "invalid_outcome")
	}
	if in.KnownAt != nil {
		if _, err := in.KnownAt.Instant(); err != nil {
			return err
		}
	}
	if in.EventAnchor != nil {
		if _, err := in.EventAnchor.At.Instant(); err != nil {
			return err
		}
	}
	for _, a := range in.Amounts {
		if !slices.Contains(def.AmountTypes, a.Type) || a.Currency == "" || a.Nature == "" {
			return failure(422, "invalid_outcome")
		}
	}
	return nil
}

// contribution 判定贡献关系（F7 第4条、第13节第12项）：用户给出时以用户为准（更正追加记录）；从信号创建、且信号生成时间早于获知时间
// 的为首次提示；获知时间只到日时按所填时区比较日期，与信号同日不判定；其余不判定。不计算“妙洽带来的金额”。
func contribution(p *Pursuit, in *OutcomeInput) string {
	if in.Contribution != "" {
		return in.Contribution
	}
	if p.SourceKind != "signal" || p.SignalCreatedAt == nil || in.KnownAt == nil {
		return "undetermined"
	}
	loc, err := time.LoadLocation(in.KnownAt.Timezone)
	if err != nil {
		return "undetermined"
	}
	switch in.KnownAt.Precision {
	case "minute":
		at, err := time.ParseInLocation("2006-01-02T15:04", in.KnownAt.Value, loc)
		if err == nil && p.SignalCreatedAt.Before(at) {
			return "first_alert"
		}
	case "day":
		if p.SignalCreatedAt.In(loc).Format("2006-01-02") < in.KnownAt.Value {
			return "first_alert"
		}
	}
	return "undetermined"
}

// recordOutcome 写入一条结果记录并发布 pursuit.outcome.recorded.v1（learning 以结果记录标识幂等消费）。
func recordOutcome(ctx context.Context, tx pgx.Tx, rc reqctx.Context, p *Pursuit, in *OutcomeInput, supersedes, reason string, now time.Time) (*OutcomeView, error) {
	o := &OutcomeView{ID: ids.New("otc"), Result: in.Result, ReasonCode: in.ReasonCode, Note: in.Note, KnownAt: in.KnownAt, KnownChannel: in.KnownChannel,
		Amounts: in.Amounts, EventAnchor: in.EventAnchor, Contribution: contribution(p, in), Supersedes: supersedes, RevisionReason: reason,
		RecordedBy: rc.Principal.ID, RecordedAt: now}
	if o.Amounts == nil {
		o.Amounts = []Amount{}
	}
	content, _ := json.Marshal(o)
	if _, err := tx.Exec(ctx, `INSERT INTO pur_outcomes (id, tenant_id, pursuit_id, content, supersedes, recorded_by, recorded_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		o.ID, rc.Tenant, p.ID, content, optional(supersedes), rc.Principal.ID, now); err != nil {
		return nil, err
	}
	amounts := []map[string]any{}
	for _, a := range o.Amounts {
		amounts = append(amounts, map[string]any{"type": a.Type, "currency": a.Currency, "value": a.Value, "nature": a.Nature, "source": a.Source})
	}
	data := map[string]any{"outcome_id": o.ID, "pursuit_id": p.ID, "objective_id": p.ObjectiveID, "template_id": p.TemplateID, "result": o.Result,
		"reason_code": o.ReasonCode, "amounts": amounts, "contribution": o.Contribution, "recorded_at": ts(now)}
	if o.KnownAt != nil {
		at, _ := o.KnownAt.Instant()
		data["known_at"], data["known_at_utc"] = o.KnownAt, ts(at)
	}
	if o.KnownChannel != "" {
		data["known_channel"] = o.KnownChannel
	}
	if p.SourceSignal != "" {
		data["source_signal_id"] = p.SourceSignal
	}
	if supersedes != "" {
		data["supersedes"] = supersedes
	}
	if _, err := writer.Append(ctx, tx, outbox.Event{Type: OutcomeRecorded, Subject: p.ID, Data: data}); err != nil {
		return nil, err
	}
	return o, nil
}

// CloseRequest 是契约 CloseRequest。
type CloseRequest struct {
	OperationID string       `json:"operation_id"`
	Expected    int64        `json:"expected_version"`
	Outcome     OutcomeInput `json:"outcome"`
}

// ClosePursuit 关闭并记录结果（契约 closePursuit）：取消未完成任务的提醒，不改变信号；M2 不支持重新打开。授权同步失败的跟进
// 只允许决策者关闭（F12 第6条）。
func (s *Service) ClosePursuit(ctx context.Context, id, key string, req CloseRequest) (*CommandResult, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	fp := fingerprint("close", id, req)
	return s.run(ctx, rc, "close", key, fp, func() (*Fixed, error) {
		o := op{id: req.OperationID, fp: fp, action: "close", channel: "web", pursuit: id}
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
		if err := s.validateOutcome(p, &req.Outcome); err != nil {
			return s.fix(ctx, rc, o, err)
		}
		return s.commit(ctx, rc, o, func(ctx context.Context, tx pgx.Tx) (*Fixed, any, error) {
			now := s.businessNow()
			p, err := loadPursuit(ctx, tx, id, true)
			if err != nil {
				return nil, nil, err
			}
			if err := s.admitWrite(p, r, rc.Principal.ID); err != nil {
				return nil, nil, err
			}
			if p.State != "open" || p.Version != req.Expected {
				return nil, nil, conflict("conflict", p, nil)
			}
			out, err := recordOutcome(ctx, tx, rc, p, &req.Outcome, "", "", now)
			if err != nil {
				return nil, nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_pursuits SET state = 'closed', closed_at = $2, result = $3, current_outcome_id = $4, version = version + 1,
 changed_by = $5, updated_at = $2 WHERE id = $1`, id, now, out.Result, out.ID, rc.Principal.ID); err != nil {
				return nil, nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_reminders SET state = 'cancelled', cancelled_by = $2 WHERE pursuit_id = $1 AND state = 'planned'`, id, o.id); err != nil {
				return nil, nil, err
			}
			if _, err := writer.Append(ctx, tx, outbox.Event{Type: PursuitClosed, Subject: id, Data: map[string]any{"pursuit_id": id, "objective_id": p.ObjectiveID,
				"outcome_id": out.ID, "result": out.Result, "operation_id": o.id, "closed_at": ts(now)}}); err != nil {
				return nil, nil, err
			}
			if err := timeline(ctx, tx, rc.Tenant, id, "closed", rc.Principal.ID, out.Result+" · "+out.ReasonCode, now); err != nil {
				return nil, nil, err
			}
			if err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.pursuit.close", Resource: fmt.Sprintf("%s/op:%s/version:%d/outcome:%s", id, o.id, p.Version+1, out.ID),
				Result: "succeeded", Basis: relEdit}); err != nil {
				return nil, nil, err
			}
			return &Fixed{Result: "succeeded", PursuitID: id, OutcomeID: out.ID}, nil, nil
		})
	})
}

// ReviseRequest 是契约 ReviseOutcomeRequest。
type ReviseRequest struct {
	OperationID string       `json:"operation_id"`
	Supersedes  string       `json:"supersedes"`
	Outcome     OutcomeInput `json:"outcome"`
	Reason      string       `json:"reason"`
}

// ReviseOutcome 修订结果（契约 reviseOutcome；F7 第3条）：追加新的结果记录并引用被取代的当前记录，旧记录保留。
func (s *Service) ReviseOutcome(ctx context.Context, id, key string, req ReviseRequest) (*CommandResult, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	fp := fingerprint("revise", id, req)
	return s.run(ctx, rc, "revise", key, fp, func() (*Fixed, error) {
		o := op{id: req.OperationID, fp: fp, action: "revise", channel: "web", pursuit: id}
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
		if err := s.validateOutcome(p, &req.Outcome); err != nil {
			return s.fix(ctx, rc, o, err)
		}
		return s.commit(ctx, rc, o, func(ctx context.Context, tx pgx.Tx) (*Fixed, any, error) {
			now := s.businessNow()
			p, err := loadPursuit(ctx, tx, id, true)
			if err != nil {
				return nil, nil, err
			}
			if err := s.admitWrite(p, r, rc.Principal.ID); err != nil {
				return nil, nil, err
			}
			if p.State != "closed" || p.CurrentOutcome != req.Supersedes {
				return nil, nil, conflict("conflict", p, nil)
			}
			out, err := recordOutcome(ctx, tx, rc, p, &req.Outcome, req.Supersedes, req.Reason, now)
			if err != nil {
				return nil, nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE pur_pursuits SET result = $2, current_outcome_id = $3, version = version + 1, changed_by = $4, updated_at = $5 WHERE id = $1`,
				id, out.Result, out.ID, rc.Principal.ID, now); err != nil {
				return nil, nil, err
			}
			if err := timeline(ctx, tx, rc.Tenant, id, "outcome_revised", rc.Principal.ID, req.Supersedes+" → "+out.ID+"："+req.Reason, now); err != nil {
				return nil, nil, err
			}
			if err := recorder.Record(ctx, tx, audit.Entry{Action: "pursuit.outcome.revise", Resource: fmt.Sprintf("%s/op:%s/outcome:%s->%s", id, o.id, req.Supersedes, out.ID),
				Result: "succeeded", Basis: relEdit}); err != nil {
				return nil, nil, err
			}
			return &Fixed{Result: "succeeded", PursuitID: id, OutcomeID: out.ID}, nil, nil
		})
	})
}
