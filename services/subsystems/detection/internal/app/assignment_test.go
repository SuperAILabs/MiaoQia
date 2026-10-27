package app

import (
	"testing"

	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
)

// 候选负责人的计算（C-14）：按顺序第一条命中的规则；区域与品类同时给出须都满足；被分派人已不是负责人或参与人的规则跳过；
// 提供方没有给出分派配置时为“不可用”。
func TestCandidateOwner(t *testing.T) {
	p := cc.ActivePolicy{Owner: cc.NewOptSubject("dm"), Participants: []cc.Subject{"bd", "lead"},
		Assignment: cc.NewOptActiveAssignment(cc.ActiveAssignment{Version: 3, Rules: []cc.AssignmentRule{
			{Assignee: "gone", Regions: []string{"330000"}},
			{Assignee: "lead", Regions: []string{"330100"}, Categories: []string{"CAT-PIPE-PE-GAS"}},
			{Assignee: "bd", Regions: []string{"330200"}},
			{Assignee: "dm", Categories: []string{"CAT-FIT"}},
		}})}
	for _, tc := range []struct {
		location   string
		categories []string
		want       string
	}{
		{"330106", []string{"CAT-PIPE-PE-GAS"}, "lead"},
		{"330106", []string{"CAT-PIPE-PE"}, ""},
		{"330206", []string{"CAT-PIPE-PE"}, "bd"},
		{"", []string{"CAT-FIT-PE"}, "dm"},
		{"", []string{"CAT-PIPE"}, ""},
	} {
		owner, version, ok := candidateOwner(p, tc.location, tc.categories)
		if !ok || version != 3 || owner != tc.want {
			t.Fatalf("%s %v：%q v%d %v，期望 %q", tc.location, tc.categories, owner, version, ok, tc.want)
		}
	}
	p.Assignment = cc.OptActiveAssignment{}
	if _, _, ok := candidateOwner(p, "330106", nil); ok {
		t.Fatal("没有分派配置时应为不可用")
	}
}
