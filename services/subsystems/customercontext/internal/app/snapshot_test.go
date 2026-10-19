package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/domain"
)

// 接口返回的快照（生成类型的编码结果）满足上下文快照契约 contracts/schemas/intelligence/context-snapshot.schema.json：
// 未关联本企业时不出现 org_ids 与 entities，状态表为每个主体给出，未知不以 null 或空列表表达。
func TestSnapshotMatchesCanonicalSchema(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "..", "contracts", "schemas", "intelligence", "context-snapshot.schema.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat = true
	if err := c.AddResource("https://schemas.miaoqia.com/intelligence/context-snapshot.schema.json", bytesReader(raw)); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile("https://schemas.miaoqia.com/intelligence/context-snapshot.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	linked := domain.Empty()
	linked.Entities = []domain.Entity{{OrgID: "org_01JA8ZXXXXXXXXXXXXXXXXXXX1", QualificationsComplete: true}, {OrgID: "org_01JA8ZXXXXXXXXXXXXXXXXXXX2"}}
	linked.Categories, linked.Regions = []string{"CAT-PIPE-PE"}, []string{"330000"}
	linked.Qualifications = []domain.Qualification{{OrgID: "org_01JA8ZXXXXXXXXXXXXXXXXXXX1", Code: "Q-BM-ISO9001", CertificateNo: "1", ValidTo: "2020-01-01"}}
	for name, p := range map[string]domain.Profile{"未关联": domain.Empty(), "已关联": linked} {
		view, _ := domain.Compose(p, "2026-10-19")
		// 与数据库往返相同：先按存储格式编码再解码。
		stored, _ := json.Marshal(view)
		var decoded domain.View
		if err := json.Unmarshal(stored, &decoded); err != nil {
			t.Fatal(err)
		}
		out := SnapshotOut(&SnapshotResult{Tenant: "tnt_01JA8ZXXXXXXXXXXXXXXXXXXX1", Unit: "root", Version: 3, Chain: []string{"root"}, View: decoded})
		body, err := out.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(doc); err != nil {
			t.Errorf("%s：%s 不满足快照契约：%v", name, body, err)
		}
	}
}
