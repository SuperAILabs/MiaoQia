package policy

import (
	"fmt"
	"time"
)

// ExampleInput 把样例的内联视图（given）转为求值输入。
func (p *Policy) ExampleInput(ex Example) (Input, error) {
	ft := ex.FactType
	if ft == "" {
		if len(p.Scope.FactTypes) > 0 {
			ft = p.Scope.FactTypes[0]
		} else if len(p.FactTypes) > 0 {
			ft = p.FactTypes[0]
		}
	}
	in := Input{FactType: ft}
	get := func(k string) map[string]any {
		m, _ := ex.Given[k].(map[string]any)
		return m
	}
	in.Fact, in.Event, in.Profile, in.Lists, in.Vocab = get("fact"), get("event"), get("profile"), get("lists"), get("vocab")
	switch v := ex.Given["now"].(type) {
	case nil:
		in.Now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	case string:
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return in, fmt.Errorf("样例「%s」的 now：%w", ex.Name, err)
		}
		in.Now = t
	case time.Time:
		in.Now = v
	}
	return in, nil
}
