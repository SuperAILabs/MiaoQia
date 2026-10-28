package app

import (
	"math/big"
	"testing"
)

// C-10 用例42：金额按（类型、币种、性质）分别精确合计（十进制文本，不经浮点），不同组不相加；原因与贡献分别计数。
func TestOutcomeSectionTotals(t *testing.T) {
	row := func(result, reason, contribution string, amounts ...[4]string) *outcomeRow {
		r := &outcomeRow{result: result, reason: reason, contribution: contribution}
		for _, a := range amounts {
			r.amounts = append(r.amounts, struct {
				Type     string `json:"type"`
				Currency string `json:"currency"`
				Value    string `json:"value"`
				Nature   string `json:"nature"`
			}{a[0], a[1], a[2], a[3]})
		}
		return r
	}
	acc := newSection()
	for _, r := range []*outcomeRow{
		row("success", "PRICE_COMPETITIVE", "first_alert", [4]string{"own_supply_amount", "CNY", "0.1", "actual"}, [4]string{"own_supply_amount", "USD", "5", "actual"}),
		row("success", "PRICE_COMPETITIVE", "first_alert", [4]string{"own_supply_amount", "CNY", "0.2", "actual"}),
		row("failure", "PRICE_UNCOMPETITIVE", "self_found", [4]string{"own_supply_amount", "CNY", "100", "estimate"}),
		row("abandoned", "TOO_SMALL", "undetermined"),
	} {
		if err := acc.add(r); err != nil {
			t.Fatal(err)
		}
	}
	out := acc.section()
	if out.Total != 4 || out.Results != (OutcomeResultCounts{2, 1, 1}) || len(out.Reasons) != 3 || out.Reasons[0].Count != 2 || len(out.Contributions) != 3 {
		t.Fatalf("%+v", out)
	}
	want := map[string]string{"own_supply_amount/CNY/actual": "0.3", "own_supply_amount/CNY/estimate": "100", "own_supply_amount/USD/actual": "5"}
	if len(out.Amounts) != len(want) {
		t.Fatalf("%+v", out.Amounts)
	}
	for _, a := range out.Amounts {
		if want[a.Type+"/"+a.Currency+"/"+a.Nature] != a.Total {
			t.Fatalf("%+v", a)
		}
	}
	for in, want := range map[string]string{"1180000": "1180000", "-0.5000": "-0.5", "0": "0", "12.3400": "12.34"} {
		v, _ := new(big.Rat).SetString(in)
		if got := decimalText(v); got != want {
			t.Fatalf("%s → %s，期望 %s", in, got, want)
		}
	}
}
