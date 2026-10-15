package contracts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestRoundTrip 把契约正例（mqc fixtures --export 导出）严格反序列化为生成类型：
// 未知字段即失败，用来发现生成类型与 Schema 的偏差。未设置 MQC_FIXTURES 时跳过。
func TestRoundTrip(t *testing.T) {
	dir := os.Getenv("MQC_FIXTURES")
	if dir == "" {
		t.Skip("MQC_FIXTURES 未设置")
	}
	factories := map[string]func() any{
		"Notice":             func() any { return &Notice{} },
		"ProcurementProject": func() any { return &ProcurementProject{} },
		"OrgRegistration":    func() any { return &OrgRegistration{} },
		"OrgChange":          func() any { return &OrgChange{} },
		"OrgAbnormal":        func() any { return &OrgAbnormal{} },
		"CreditRecord":       func() any { return &CreditRecord{} },
		"Signal":             func() any { return &Signal{} },
		"ContextSnapshot":    func() any { return &ContextSnapshot{} },
		"InvestmentRecord":   func() any { return &InvestmentRecord{} },
		"NormalizedDocument": func() any { return &NormalizedDocument{} },
		"EvidenceRef":        func() any { return &EvidenceRef{} },
		"EventEnvelope":      func() any { return &EventEnvelope{} },
		"RequestContext":     func() any { return &RequestContext{} },
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("没有找到样例：%v", err)
	}
	for _, f := range files {
		name := strings.SplitN(filepath.Base(f), "__", 2)[0]
		mk, ok := factories[name]
		if !ok {
			t.Errorf("%s：类型 %s 没有登记在往返测试中", f, name)
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		model := mk()
		if err := dec.Decode(model); err != nil {
			t.Errorf("%s：%v", filepath.Base(f), err)
			continue
		}
		encoded, err := json.Marshal(model)
		if err != nil {
			t.Errorf("%s：重新编码失败 %v", filepath.Base(f), err)
			continue
		}
		var before, after any
		if err := json.Unmarshal(raw, &before); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &after); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Errorf("%s：往返改变了字段或值", filepath.Base(f))
		}
	}
}
