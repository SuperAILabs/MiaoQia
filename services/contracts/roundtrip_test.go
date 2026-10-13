package contracts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
		"InvestmentRecord":   func() any { return &InvestmentRecord{} },
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
		if err := dec.Decode(mk()); err != nil {
			t.Errorf("%s：%v", filepath.Base(f), err)
		}
	}
}
