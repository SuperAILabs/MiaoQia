package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/superailabs/miaoqia/services/subsystems/aiplatform/api"
)

// TestVendorIntegration calls the real vendor APIs. It is skipped unless MIAOQIA_AI_INTEGRATION=1;
// each vendor then runs only when both of these are set (keys come from the environment, never the repo):
//
//	MIAOQIA_AI_PLATFORM_<VENDOR>_API_KEY     (and optionally ..._BASE_URL, ..._STRUCTURED)
//	MIAOQIA_AI_PLATFORM_<VENDOR>_TEST_MODEL  model ID to call
//
// Example: MIAOQIA_AI_INTEGRATION=1 go test ./subsystems/aiplatform/internal/gateway -run TestVendorIntegration -v
func TestVendorIntegration(t *testing.T) {
	if os.Getenv("MIAOQIA_AI_INTEGRATION") != "1" {
		t.Skip("set MIAOQIA_AI_INTEGRATION=1 and vendor keys to call real vendor APIs")
	}
	config := func(k string) string { return os.Getenv("MIAOQIA_AI_PLATFORM_" + k) }
	providers, err := VendorProviders(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range allVendors {
		t.Run(v, func(t *testing.T) {
			model := config(strings.ToUpper(v) + "_TEST_MODEL")
			if providers[v] == nil || model == "" {
				t.Skipf("MIAOQIA_AI_PLATFORM_%s_API_KEY or _TEST_MODEL not set", strings.ToUpper(v))
			}
			c, err := ParseCatalog(fmt.Appendf(nil, `
endpoints:
  - id: live
    provider: %s
    model: %s
    version: integration
    capabilities: [generate, classify]
    region: live
    billing: token
    input_price_per_1k: 1
    output_price_per_1k: 1
    request_overhead_tokens: 2048
    timeout: 90s
    no_training_on_customer: true
    decision_tasks:
      - {task: notice.doc_type, version: "2", capability: classify, score_meaning: none}
`, v, model))
			if err != nil {
				t.Fatal(err)
			}
			meter := &MemoryMeter{}
			s, err := New(c, map[string]Provider{v: providers[v]}, meter, nil)
			if err != nil {
				t.Fatal(err)
			}
			gen := genReq()
			gen.Instructions = "Return the product category of the notice in Chinese."
			gen.Budget = api.Budget{MaxOutputTokens: 2048, MaxCostMicroYuan: 1_000_000}
			res, err := s.Generate(ctx(), gen)
			if err != nil {
				t.Fatalf("generate: %v (records %+v)", err, meter.Records())
			}
			var out map[string]any
			if json.Unmarshal(res.Output, &out) != nil || res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
				t.Fatalf("generate: output=%s usage=%+v", res.Output, res.Usage)
			}
			t.Logf("generate: %s usage=%+v", res.Output, res.Usage)

			cls := classifyReq()
			cls.Budget = api.Budget{MaxOutputTokens: 2048, MaxCostMicroYuan: 1_000_000}
			d, err := s.Classify(ctx(), cls)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			t.Logf("classify: outcome=%s label=%s usage=%+v", d.Outcome, d.Label, d.Usage)
		})
	}
}
