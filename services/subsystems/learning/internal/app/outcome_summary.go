package app

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	customercontext "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
)

// 跟进结果汇总（评估报告的结果部分，PRD 第6段 F7 第5条、用例42；learning.md 第8.1节；C-10）。只读计算，不持久化报告：
// 时点评估（有获知时间）与事后统计（获知时间缺失）分开给出，不合并；每个跟进取截至时间时记录链上的当前记录。

// relUnitPursuits 是 identity 的单元关系：能查看单元内全部跟进（决策者）。只凭参与关系查看个别跟进的成员不能读取汇总，
// 以免聚合泄露其看不到的跟进。
const relUnitPursuits = "can_view_unit_pursuits"

// OutcomeResultCounts 是契约 OutcomeResultCounts。
type OutcomeResultCounts struct {
	Success   int `json:"success"`
	Failure   int `json:"failure"`
	Abandoned int `json:"abandoned"`
}

// OutcomeReasonCount 是契约 OutcomeReasonCount。
type OutcomeReasonCount struct {
	Result     string `json:"result"`
	ReasonCode string `json:"reason_code"`
	Count      int    `json:"count"`
}

// OutcomeContributionCount 是契约 OutcomeContributionCount。
type OutcomeContributionCount struct {
	Contribution string `json:"contribution"`
	Count        int    `json:"count"`
}

// OutcomeAmountTotal 是契约 OutcomeAmountTotal。
type OutcomeAmountTotal struct {
	Type     string `json:"type"`
	Currency string `json:"currency"`
	Nature   string `json:"nature"`
	Total    string `json:"total"`
	Count    int    `json:"count"`
}

// OutcomeSection 是契约 OutcomeSection。
type OutcomeSection struct {
	Total         int                        `json:"total"`
	Results       OutcomeResultCounts        `json:"results"`
	Reasons       []OutcomeReasonCount       `json:"reasons"`
	Contributions []OutcomeContributionCount `json:"contributions"`
	Amounts       []OutcomeAmountTotal       `json:"amounts"`
}

// OutcomeSummary 是契约 OutcomeSummary。
type OutcomeSummary struct {
	ObjectiveID    string         `json:"objective_id"`
	From           *time.Time     `json:"from,omitempty"`
	AsOf           time.Time      `json:"as_of"`
	PointInTime    OutcomeSection `json:"point_in_time"`
	PostHoc        OutcomeSection `json:"post_hoc"`
	Superseded     int            `json:"superseded"`
	KnownAfterAsOf int            `json:"known_after_as_of"`
}

type outcomeRow struct {
	id, pursuit, result, reason, contribution string
	supersedes                                *string
	known                                     *time.Time
	recorded                                  time.Time
	amounts                                   []struct {
		Type     string `json:"type"`
		Currency string `json:"currency"`
		Value    string `json:"value"`
		Nature   string `json:"nature"`
	}
}

type sectionAcc struct {
	out      OutcomeSection
	reasons  map[[2]string]int
	contribs map[string]int
	amounts  map[[3]string]*amountAcc
}

type amountAcc struct {
	sum   *big.Rat
	count int
}

func newSection() *sectionAcc {
	return &sectionAcc{reasons: map[[2]string]int{}, contribs: map[string]int{}, amounts: map[[3]string]*amountAcc{}}
}

func (a *sectionAcc) add(r *outcomeRow) error {
	a.out.Total++
	switch r.result {
	case "success":
		a.out.Results.Success++
	case "failure":
		a.out.Results.Failure++
	case "abandoned":
		a.out.Results.Abandoned++
	}
	a.reasons[[2]string{r.result, r.reason}]++
	a.contribs[r.contribution]++
	for _, m := range r.amounts {
		v, ok := new(big.Rat).SetString(m.Value)
		if !ok {
			return errors.New("learning：结果金额不是十进制文本")
		}
		k := [3]string{m.Type, m.Currency, m.Nature}
		acc := a.amounts[k]
		if acc == nil {
			acc = &amountAcc{sum: new(big.Rat)}
			a.amounts[k] = acc
		}
		acc.sum.Add(acc.sum, v)
		acc.count++
	}
	return nil
}

func (a *sectionAcc) section() OutcomeSection {
	out := a.out
	out.Reasons, out.Contributions, out.Amounts = []OutcomeReasonCount{}, []OutcomeContributionCount{}, []OutcomeAmountTotal{}
	for k, n := range a.reasons {
		out.Reasons = append(out.Reasons, OutcomeReasonCount{Result: k[0], ReasonCode: k[1], Count: n})
	}
	sort.Slice(out.Reasons, func(i, j int) bool {
		x, y := out.Reasons[i], out.Reasons[j]
		if x.Count != y.Count {
			return x.Count > y.Count
		}
		return x.Result+"/"+x.ReasonCode < y.Result+"/"+y.ReasonCode
	})
	if len(out.Reasons) > 200 {
		out.Reasons = out.Reasons[:200]
	}
	for k, n := range a.contribs {
		out.Contributions = append(out.Contributions, OutcomeContributionCount{Contribution: k, Count: n})
	}
	sort.Slice(out.Contributions, func(i, j int) bool { return out.Contributions[i].Contribution < out.Contributions[j].Contribution })
	for k, acc := range a.amounts {
		out.Amounts = append(out.Amounts, OutcomeAmountTotal{Type: k[0], Currency: k[1], Nature: k[2], Total: decimalText(acc.sum), Count: acc.count})
	}
	sort.Slice(out.Amounts, func(i, j int) bool {
		x, y := out.Amounts[i], out.Amounts[j]
		return x.Type+"/"+x.Currency+"/"+x.Nature < y.Type+"/"+y.Currency+"/"+y.Nature
	})
	if len(out.Amounts) > 200 {
		out.Amounts = out.Amounts[:200]
	}
	return out
}

// decimalText 把合计写成十进制文本（输入至多 4 位小数，合计精确；去掉末尾的 0）。
func decimalText(v *big.Rat) string {
	s := v.FloatString(4)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" {
		s = "0"
	}
	return s
}

// GetOutcomeSummary 计算目标的跟进结果汇总（契约 getOutcomeSummary）。from 为空时不限开始；asOf 为空时为当前时间。
func (s *Service) GetOutcomeSummary(ctx context.Context, objective string, from, asOf *time.Time) (*OutcomeSummary, error) {
	rc, err := tenantContext(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Principal.Kind != "user" {
		return nil, failure(403, "forbidden")
	}
	if s.Scope == nil || s.Authz == nil {
		return nil, failure(503, "authorization_unavailable")
	}
	scope, err := s.Scope.Scope(ctx, objective, nil)
	if err != nil {
		var safe *customercontext.Error
		switch {
		case errors.As(err, &safe) && safe.Status == 404:
			return nil, failure(404, "objective_not_found")
		case errors.As(err, &safe) && safe.Status < 500:
			return nil, failure(safe.Status, safe.Code)
		}
		return nil, failure(503, "customer_context_unavailable")
	}
	if scope.OrgUnitID == "" {
		return nil, failure(503, "customer_context_unavailable")
	}
	ok, err := s.Authz.Check(ctx, authz.Request{Subject: authz.Subject(rc.Principal), Relation: relUnitPursuits, Object: "org_unit:" + scope.OrgUnitID})
	if err != nil {
		return nil, failure(503, "identity_unavailable")
	}
	if !ok {
		return nil, failure(403, "forbidden")
	}
	out := &OutcomeSummary{ObjectiveID: objective, AsOf: s.businessNow()}
	if asOf != nil {
		out.AsOf = asOf.UTC()
	}
	if from != nil {
		f := from.UTC()
		if !f.Before(out.AsOf) {
			return nil, failure(400, "invalid_window")
		}
		out.From = &f
	}
	var rows []*outcomeRow
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q, err := tx.Query(ctx, `SELECT outcome_id, pursuit_id, result, reason_code, contribution, supersedes, known_at_utc, recorded_at, amounts
 FROM lrn_outcomes WHERE objective_id = $1 AND recorded_at <= $2 ORDER BY recorded_at, outcome_id`, objective, out.AsOf)
		if err != nil {
			return err
		}
		defer q.Close()
		for q.Next() {
			r := &outcomeRow{}
			var raw []byte
			if err := q.Scan(&r.id, &r.pursuit, &r.result, &r.reason, &r.contribution, &r.supersedes, &r.known, &r.recorded, &raw); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &r.amounts); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return q.Err()
	})
	if err != nil {
		return nil, err
	}
	// 被同样不晚于截至时间的记录取代的不计
	replaced := map[string]bool{}
	for _, r := range rows {
		if r.supersedes != nil {
			replaced[*r.supersedes] = true
		}
	}
	pit, post := newSection(), newSection()
	inWindow := func(t time.Time) bool { return (out.From == nil || !t.Before(*out.From)) && t.Before(out.AsOf) }
	for _, r := range rows {
		if replaced[r.id] {
			out.Superseded++
			continue
		}
		switch {
		case r.known != nil && !r.known.Before(out.AsOf):
			out.KnownAfterAsOf++
		case r.known != nil && inWindow(*r.known):
			err = pit.add(r)
		case r.known == nil && inWindow(r.recorded):
			err = post.add(r)
		}
		if err != nil {
			s.logger().ErrorContext(ctx, "learning: 结果记录无法汇总", "outcome", r.id, "err", err)
			return nil, failure(503, "learning_unavailable")
		}
	}
	out.PointInTime, out.PostHoc = pit.section(), post.section()
	return out, nil
}
