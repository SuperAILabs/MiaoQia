package domain

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// 分派规则的形式校验（C-14）：上限、区域代码、已安装行业包的品类、同一规则内不重复。
func TestValidateAssignment(t *testing.T) {
	c := catalog()
	ok := []AssignmentRule{{Assignee: "bd", Regions: []string{"330100"}, Categories: []string{"CAT-PIPE-PE"}}, {Assignee: "dm"}}
	if err := ValidateAssignment(c, ok); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAssignment(c, nil); err != nil {
		t.Fatal(err)
	}
	many := make([]AssignmentRule, MaxAssignmentRules+1)
	for i := range many {
		many[i] = AssignmentRule{Assignee: fmt.Sprint("u", i)}
	}
	for _, tc := range []struct {
		name  string
		rules []AssignmentRule
		code  string
	}{
		{"条数上限", many, "too_many_rules"},
		{"空的区域条件", []AssignmentRule{{Assignee: "bd", Regions: []string{}}}, "invalid_rule"},
		{"无效区划", []AssignmentRule{{Assignee: "bd", Regions: []string{"001100"}}}, "invalid_region"},
		{"未安装的品类", []AssignmentRule{{Assignee: "bd", Categories: []string{"CAT-UNKNOWN"}}}, "unknown_category"},
		{"重复的区域", []AssignmentRule{{Assignee: "bd", Regions: []string{"330100", "330100"}}}, "duplicate_value"},
	} {
		var bad *Invalid
		if err := ValidateAssignment(c, tc.rules); !errors.As(err, &bad) || bad.Code != tc.code {
			t.Fatalf("%s：%v", tc.name, err)
		}
	}
	if got := Assignees([]AssignmentRule{{Assignee: "b"}, {Assignee: "a"}, {Assignee: "b"}}); !slices.Equal(got, []string{"b", "a"}) {
		t.Fatal(got)
	}
}
