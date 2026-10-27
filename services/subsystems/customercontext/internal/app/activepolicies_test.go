package app

import (
	"encoding/json"
	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
	"strings"
	"testing"
	"time"
)

// 生效策略中的分派配置（C-14 评审 P1）：LEFT JOIN 没有分派配置记录（目标从未配置）时不出现 assignment，detection 记为
// rules_unavailable；已配置为空规则时出现（版本从 1 开始、规则为空），detection 记为 none。两者不得混同。
func TestActiveAssignmentScan(t *testing.T) {
	policy := func(a *ActiveAssignment) ActivePolicy {
		return ActivePolicy{ObjectiveID: "obj_01JA8ZXXXXXXXXXXXXXXXXXXX1", ObjectiveCode: "TPL-TENDER-A02-SUPPLY", ObjectiveState: "active",
			OrgUnitID: "root", PolicyID: "pol_01JA8ZXXXXXXXXXXXXXXXXXXX1", Generation: 1, Version: 1, TemplateID: "TPL-TENDER-A02-SUPPLY",
			TemplateVersion: "1.0.0", EnabledDomains: []string{}, Document: json.RawMessage(`{"id":"TPL-TENDER-A02-SUPPLY","version":"1.0.0"}`), EffectiveAt: time.Unix(0, 0).UTC(),
			Owner: "dm", Participants: []string{"bd"}, Assignment: a}
	}

	// 没有记录：两列都为空
	none, err := activeAssignment(nil, nil)
	if err != nil || none != nil {
		t.Fatalf("没有分派配置记录时应为 nil：%+v %v", none, err)
	}
	raw, err := json.Marshal(policy(none))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"assignment"`) {
		t.Fatalf("从未配置时不应出现 assignment：%s", raw)
	}
	list, err := convert[cc.ActivePolicyList](&ActivePolicyPage{Policies: []ActivePolicy{policy(none)}})
	if err != nil {
		t.Fatal(err)
	}
	if list.Policies[0].Assignment.Set {
		t.Fatalf("从未配置时契约类型不应带 assignment：%+v", list.Policies[0].Assignment)
	}

	// 已配置为空规则
	v := int64(1)
	empty, err := activeAssignment(&v, []byte(`[]`))
	if err != nil || empty == nil || empty.Version != 1 || empty.Rules == nil || len(empty.Rules) != 0 {
		t.Fatalf("已配置为空规则：%+v %v", empty, err)
	}
	list, err = convert[cc.ActivePolicyList](&ActivePolicyPage{Policies: []ActivePolicy{policy(empty)}})
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := list.Policies[0].Assignment.Get(); !ok || a.Version != 1 || len(a.Rules) != 0 {
		t.Fatalf("已配置为空规则时应带 assignment：%+v", list.Policies[0].Assignment)
	}

	// 版本小于 1 的记录不应存在：失败关闭，不当作从未配置也不当作已配置
	zero := int64(0)
	if _, err := activeAssignment(&zero, []byte(`[]`)); err == nil {
		t.Fatal("版本 0 的记录应失败")
	}
	if _, err := activeAssignment(&v, []byte(`{`)); err == nil {
		t.Fatal("损坏的规则应失败")
	}
}
