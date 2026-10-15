package policy

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// worstCase 构造按 contracts/policy/cel-environment.yaml 规模上界填满的输入：
// 列表取上界长度，清单与词表取上界，且让规则尽量走完整个列表（不命中）。
func worstCase(factType string) Input {
	ids := func(prefix string, n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%05d", prefix, i)
		}
		return out
	}
	items := make([]any, 500)
	for i := range items {
		cats := make([]any, 4)
		for j := range cats {
			cats[j] = map[string]any{"scheme": "industry:x", "code": fmt.Sprintf("CAT-OTHER-%d-%d", i, j)}
		}
		items[i] = map[string]any{"categories": cats, "brand_required": "brand-x"}
	}
	reqs := make([]any, 200)
	for i := range reqs {
		reqs[i] = map[string]any{"kind": "qualification", "code": fmt.Sprintf("Q%03d", i)}
	}
	flags := make([]any, 200)
	for i := range flags {
		flags[i] = map[string]any{"record_type": "penalty", "resolved": true}
	}
	winner := map[string]any{
		"org_id": "org_w", "credit_flags": flags,
		"history_categories": ids("CAT-H-", 500), "supplied_brand_owners": ids("org_b", 500),
		"past_buyer_ids": ids("org_pb", 2000), "trailing_award_total": map[string]any{"value": "100000000", "currency": "CNY"},
	}
	fact := map[string]any{
		"notice_type": "tender", "result_type": "awarded", "record_type": "penalty", "role": "defendant",
		"change_type": "status", "status_after": "active", "resolved": false,
		"title": strings.Repeat("工程", 1000), "location": "330106",
		"budget": map[string]any{"value": "1000000", "currency": "CNY"}, "amount": map[string]any{"value": "1000000", "currency": "CNY"},
		"items": items, "requirements": reqs, "bid_deadline": "2026-12-01T00:00:00+08:00",
		"buyer": map[string]any{"org_id": "org_buyer"}, "winner": winner, "subject": map[string]any{"org_id": "org_s"},
		"org_ids": ids("org_f", 200), "project_id": "pp_x", "single_source_supplier": map[string]any{"org_id": "org_ss"},
	}
	lists := map[string]any{}
	for _, k := range []string{"blocked", "customers", "competitors", "tracked", "partners", "counterparties"} {
		lists[k] = ids("org_"+k[:2], 20000)
	}
	quals := map[string]any{}
	for _, q := range ids("QQ", 100) {
		quals[q.(string)] = "valid"
	}
	entities := make([]any, 10)
	for i := range entities {
		entities[i] = map[string]any{"org_id": fmt.Sprintf("org_me%d", i), "qualifications": quals,
			"qualifications_pending": ids("QP", 20), "qualifications_complete": true}
	}
	profile := map[string]any{
		"org_ids": ids("org_me", 50), "offering_categories": ids("CAT-P-", 500), "service_regions": ids("44", 500),
		"entities": entities, "brands": ids("brand", 100),
	}
	vocab := map[string]any{"exclude_terms": ids("排除词", 1000)}
	return Input{FactType: factType, Fact: fact, Event: map[string]any{"type": "tendering.result.published.v1"},
		Profile: profile, Lists: lists, Vocab: vocab, Now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
}

type costRow struct {
	template           string
	maxStatic          uint64
	maxStaticRule      string
	exampleCost        uint64
	worstCost          uint64
	worstNS, exampleNS float64
}

func measureCosts(t testing.TB) []costRow { return measureWith(t, false) }

func measure(t testing.TB) []costRow { return measureWith(t, true) }

func measureWith(t testing.TB, timing bool) []costRow {
	pack := loadTendering(t)
	var rows []costRow
	for id, tpl := range pack.Templates {
		c, issues := Compile(tpl, pack.Features, Limits{})
		if issues != nil {
			t.Fatalf("%s：%v", id, issues)
		}
		row := costRow{template: id, maxStatic: c.MaxStaticCost}
		for _, r := range c.rules {
			if r.staticCost == c.MaxStaticCost {
				row.maxStaticRule = r.rule.ID
			}
		}
		for n, f := range c.features {
			if f.staticCost == c.MaxStaticCost {
				row.maxStaticRule = "features." + n
			}
		}
		for _, ex := range tpl.Examples {
			in, _ := tpl.ExampleInput(ex)
			r, _ := c.Evaluate(in)
			row.exampleCost = max(row.exampleCost, r.Cost)
		}
		for _, ft := range tpl.Scope.FactTypes {
			in := worstCase(ft)
			r, err := c.Evaluate(in)
			if err != nil {
				t.Fatal(err)
			}
			row.worstCost = max(row.worstCost, r.Cost)
		}
		if !timing {
			rows = append(rows, row)
			continue
		}
		in := worstCase(tpl.Scope.FactTypes[0])
		res := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				_, _ = c.Evaluate(in)
			}
		})
		row.worstNS = float64(res.NsPerOp())
		exIn, _ := tpl.ExampleInput(tpl.Examples[0])
		res = testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				_, _ = c.Evaluate(exIn)
			}
		})
		row.exampleNS = float64(res.NsPerOp())
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].template < rows[j].template })
	return rows
}

// TestCostReport 输出 R1 模板的成本与耗时（go test -run TestCostReport -v -report）。
func TestCostReport(t *testing.T) {
	if !*report {
		t.Skip("加 -report 输出成本报告")
	}
	fmt.Println("| 模板 | 最大静态成本（规则） | 样例运行时成本 | 最坏规模运行时成本 | 样例耗时 | 最坏规模耗时 |")
	fmt.Println("|---|---|---|---|---|---|")
	for _, r := range measure(t) {
		fmt.Printf("| %s | %d（%s） | %d | %d | %.1f µs | %.2f ms |\n", r.template, r.maxStatic, r.maxStaticRule,
			r.exampleCost, r.worstCost, r.exampleNS/1e3, r.worstNS/1e6)
	}
}
