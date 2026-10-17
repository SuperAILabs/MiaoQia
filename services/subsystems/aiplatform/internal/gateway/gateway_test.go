package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/aiplatform/api"
)

const catalogYAML = `
endpoints:
  - id: gen-primary
    provider: stub
    model: m-large
    version: "2026-09"
    capabilities: [generate]
    filed: true
    region: cn
    tier: 0
    input_price_per_1k: 1000
    output_price_per_1k: 2000
    timeout: 50ms
    no_training_on_customer: true
  - id: gen-backup
    provider: stub
    model: m-small
    version: "2026-08"
    capabilities: [generate]
    filed: true
    region: cn
    tier: 1
    input_price_per_1k: 500
    output_price_per_1k: 500
    no_training_on_customer: true
  - id: gen-unfiled
    provider: stub
    model: m-x
    version: "1"
    capabilities: [generate, embed]
    filed: false
    region: cn
    no_training_on_customer: true
  - id: gen-abroad
    provider: stub
    model: m-y
    version: "1"
    capabilities: [generate, embed, rerank]
    filed: true
    region: us
    no_training_on_customer: true
  - id: emb
    provider: stub
    model: e-1
    version: "1"
    capabilities: [embed, rerank]
    filed: true
    region: cn
    max_input_tokens: 100
    no_training_on_customer: true
`

const schema = `{"type":"object","required":["category"],"properties":{"category":{"type":"string"}},"additionalProperties":false}`

func setup(t *testing.T, b map[string]StubBehavior) (*Service, *Stub, *MemoryMeter) {
	t.Helper()
	c, err := ParseCatalog([]byte(catalogYAML))
	if err != nil {
		t.Fatal(err)
	}
	stub, meter := NewStub(b), &MemoryMeter{}
	s, err := New(c, map[string]Provider{"stub": stub}, meter, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, stub, meter
}

func ctx() context.Context {
	return reqctx.With(context.Background(), reqctx.Context{
		Tenant: "tnt_1", Cell: "cell-01", Principal: reqctx.Principal{ID: "svc:evidence", Kind: "service"}, Purpose: "monitoring",
	})
}

func genReq() api.GenerateRequest {
	return api.GenerateRequest{
		Call:         api.Call{Task: "evidence.category_enrich", Classification: api.Public, Budget: api.Budget{MaxOutputTokens: 100, MaxCostMicroYuan: 1000}},
		PromptRef:    "evidence.category@1",
		Instructions: "判断品类",
		Inputs:       []api.Input{{Name: "notice", Content: "某市供水管网改造项目采购球墨铸铁管"}},
		OutputSchema: json.RawMessage(schema),
	}
}

func code(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func TestGenerateUsesPrimaryAndMeters(t *testing.T) {
	s, stub, meter := setup(t, map[string]StubBehavior{
		"gen-primary": {Output: json.RawMessage(`{"category":"管材"}`), Usage: ProviderUsage{InputTokens: 30, OutputTokens: 10}},
	})
	res, err := s.Generate(ctx(), genReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.Model.Endpoint != "gen-primary" || res.Model.Version != "2026-09" {
		t.Fatalf("model = %+v", res.Model)
	}
	if res.Usage.CostMicroYuan != 30+20 {
		t.Fatalf("cost = %d", res.Usage.CostMicroYuan)
	}
	if got := stub.Calls(); !slices.Equal(got, []string{"gen-primary"}) {
		t.Fatalf("calls = %v", got)
	}
	recs := meter.Records()
	if len(recs) != 1 || recs[0].Status != StatusOK || recs[0].ID != res.InvocationID || recs[0].Tenant != "tnt_1" || recs[0].Attempt != 1 {
		t.Fatalf("records = %+v", recs)
	}
	if strings.Contains(recs[0].InputHash, "球墨") || len(recs[0].InputHash) != 64 {
		t.Fatalf("计量只存哈希: %q", recs[0].InputHash)
	}
}

func TestFailoverToBackupOnUnavailableAndTimeout(t *testing.T) {
	for name, b := range map[string]StubBehavior{
		"unavailable": {Err: ErrUnavailable},
		"timeout":     {Block: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, stub, meter := setup(t, map[string]StubBehavior{"gen-primary": b, "gen-backup": {Output: json.RawMessage(`{"category":"管材"}`)}})
			res, err := s.Generate(ctx(), genReq())
			if err != nil {
				t.Fatal(err)
			}
			if res.Model.Endpoint != "gen-backup" || !slices.Equal(stub.Calls(), []string{"gen-primary", "gen-backup"}) {
				t.Fatalf("model=%s calls=%v", res.Model.Endpoint, stub.Calls())
			}
			recs := meter.Records()
			if len(recs) != 2 || recs[0].Status != StatusFailed || recs[0].ErrorCode != api.CodeProviderUnavailable || recs[1].Attempt != 2 {
				t.Fatalf("records = %+v", recs)
			}
		})
	}
}

func TestAllEndpointsDownFailsExplicitly(t *testing.T) {
	s, stub, _ := setup(t, map[string]StubBehavior{"gen-primary": {Err: ErrUnavailable}, "gen-backup": {Err: ErrUnavailable}})
	_, err := s.Generate(ctx(), genReq())
	if code(err) != api.CodeProviderUnavailable {
		t.Fatalf("err = %v", err)
	}
	// 未备案与境外端点即使声明了能力也不参与，不降级到不合规模型。
	if !slices.Equal(stub.Calls(), []string{"gen-primary", "gen-backup"}) {
		t.Fatalf("calls = %v", stub.Calls())
	}
}

func TestNonTransientProviderErrorDoesNotFailover(t *testing.T) {
	s, stub, _ := setup(t, map[string]StubBehavior{"gen-primary": {Err: errors.New("400 bad request")}})
	_, err := s.Generate(ctx(), genReq())
	if code(err) != api.CodeProviderResponseBad || len(stub.Calls()) != 1 {
		t.Fatalf("err=%v calls=%v", err, stub.Calls())
	}
}

func TestOutputSchemaValidation(t *testing.T) {
	for name, out := range map[string]string{"schema": `{"category":3}`, "extra": `{"category":"管材","x":1}`, "not json": `管材`} {
		t.Run(name, func(t *testing.T) {
			s, stub, meter := setup(t, map[string]StubBehavior{"gen-primary": {Output: json.RawMessage(out)}})
			_, err := s.Generate(ctx(), genReq())
			if code(err) != api.CodeOutputInvalid || len(stub.Calls()) != 1 {
				t.Fatalf("err=%v calls=%v", err, stub.Calls())
			}
			if r := meter.Records(); len(r) != 1 || r[0].ErrorCode != api.CodeOutputInvalid {
				t.Fatalf("records = %+v", r)
			}
		})
	}
}

func TestBudgetStopsBeforeCalling(t *testing.T) {
	cases := map[string]func(*api.GenerateRequest){
		"input tokens": func(r *api.GenerateRequest) { r.Budget.MaxInputTokens = 5 },
		"no cost":      func(r *api.GenerateRequest) { r.Budget.MaxCostMicroYuan = 0 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			s, stub, meter := setup(t, nil)
			r := genReq()
			mut(&r)
			_, err := s.Generate(ctx(), r)
			if code(err) != api.CodeBudgetExceeded || len(stub.Calls()) != 0 {
				t.Fatalf("err=%v calls=%v", err, stub.Calls())
			}
			if rec := meter.Records(); len(rec) != 1 || rec[0].Status != StatusRejected {
				t.Fatalf("records = %+v", rec)
			}
		})
	}
}

func TestBudgetSkipsTooExpensiveEndpoint(t *testing.T) {
	s, stub, _ := setup(t, map[string]StubBehavior{"gen-backup": {Output: json.RawMessage(`{"category":"管材"}`)}})
	r := genReq()
	r.Budget.MaxCostMicroYuan = 100 // 主选预计 27+200，备选 14+50
	res, err := s.Generate(ctx(), r)
	if err != nil || res.Model.Endpoint != "gen-backup" || !slices.Equal(stub.Calls(), []string{"gen-backup"}) {
		t.Fatalf("res=%+v err=%v calls=%v", res, err, stub.Calls())
	}
}

func TestSensitiveAndInvalidRequestsRejected(t *testing.T) {
	s, stub, _ := setup(t, nil)
	r := genReq()
	r.Classification = api.Sensitive
	if _, err := s.Generate(ctx(), r); code(err) != api.CodeSensitiveNotAllowed {
		t.Fatalf("sensitive: %v", err)
	}
	for name, mut := range map[string]func(*api.GenerateRequest){
		"task":       func(r *api.GenerateRequest) { r.Task = "" },
		"prompt":     func(r *api.GenerateRequest) { r.PromptRef = "" },
		"max output": func(r *api.GenerateRequest) { r.Budget.MaxOutputTokens = 0 },
		"schema":     func(r *api.GenerateRequest) { r.OutputSchema = json.RawMessage(`{"type":`) },
		"class":      func(r *api.GenerateRequest) { r.Classification = "secret" },
	} {
		r := genReq()
		mut(&r)
		if _, err := s.Generate(ctx(), r); code(err) != api.CodeInvalidRequest {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := s.Generate(context.Background(), genReq()); !errors.Is(err, reqctx.ErrMissing) {
		t.Fatalf("missing context: %v", err)
	}
	if len(stub.Calls()) != 0 {
		t.Fatalf("calls = %v", stub.Calls())
	}
}

func TestEmbedAndRerank(t *testing.T) {
	s, _, _ := setup(t, map[string]StubBehavior{"emb": {Usage: ProviderUsage{InputTokens: 6}}})
	call := api.Call{Task: "search.chunk_embed", Classification: api.Public}
	e, err := s.Embed(ctx(), api.EmbedRequest{Call: call, Purpose: "chunk", Texts: []string{"管材", "阀门"}})
	if err != nil || len(e.Vectors) != 2 || e.Model.Endpoint != "emb" {
		t.Fatalf("embed: %+v %v", e, err)
	}
	rr, err := s.Rerank(ctx(), api.RerankRequest{Call: call, Query: "管材", Candidates: []string{"a", "b", "c"}})
	if err != nil || len(rr.Scores) != 3 {
		t.Fatalf("rerank: %+v %v", rr, err)
	}
	// 超出端点上下文时没有可用端点。
	long := strings.Repeat("长", 101)
	if _, err := s.Embed(ctx(), api.EmbedRequest{Call: call, Purpose: "chunk", Texts: []string{long}}); code(err) != api.CodeBudgetExceeded {
		t.Fatalf("context limit: %v", err)
	}
	// 没有声明 OCR 能力的合规端点。
	c, _ := ParseCatalog([]byte(catalogYAML))
	if _, err := c.route(api.CapOCR, api.Public); code(err) != api.CodeNoEligibleModel {
		t.Fatalf("ocr: %v", err)
	}
}

func TestEmbedCountMismatchRejected(t *testing.T) {
	s, _, _ := setup(t, nil)
	s.providers["stub"] = shortEmbed{NewStub(nil)}
	_, err := s.Embed(ctx(), api.EmbedRequest{Call: api.Call{Task: "t", Classification: api.Public}, Purpose: "chunk", Texts: []string{"a", "b"}})
	if code(err) != api.CodeProviderResponseBad {
		t.Fatalf("err = %v", err)
	}
}

type shortEmbed struct{ *Stub }

func (s shortEmbed) Embed(ctx context.Context, ep Endpoint, texts []string) ([][]float32, ProviderUsage, error) {
	v, u, err := s.Stub.Embed(ctx, ep, texts)
	return v[:1], u, err
}

func TestCatalogValidation(t *testing.T) {
	for name, y := range map[string]string{
		"missing version": `endpoints: [{id: a, provider: stub, model: m, capabilities: [embed], region: cn, no_training_on_customer: true}]`,
		"duplicate":       `endpoints: [{id: a, provider: stub, model: m, version: "1", capabilities: [embed], region: cn, no_training_on_customer: true}, {id: a, provider: stub, model: m, version: "1", capabilities: [embed], region: cn, no_training_on_customer: true}]`,
		"no region":       `endpoints: [{id: a, provider: stub, model: m, version: "1", capabilities: [embed], no_training_on_customer: true}]`,
		"training":        `endpoints: [{id: a, provider: stub, model: m, version: "1", capabilities: [embed], region: cn}]`,
	} {
		if _, err := ParseCatalog([]byte(y)); err == nil {
			t.Fatalf("%s: 应拒绝", name)
		}
	}
	c, err := ParseCatalog([]byte(catalogYAML))
	if err != nil || !slices.Equal(c.AllowedRegions, []string{"cn"}) || c.Endpoints[0].Timeout != 50*time.Millisecond {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	if _, err := New(c, map[string]Provider{}, nil, nil); err == nil {
		t.Fatal("未注册适配器应拒绝")
	}
}

func TestLocalBindingRequiresContext(t *testing.T) {
	s, _, _ := setup(t, map[string]StubBehavior{"gen-primary": {Output: json.RawMessage(`{"category":"管材"}`)}})
	g := api.LocalGateway(s)
	if _, err := g.Generate(context.Background(), genReq()); !errors.Is(err, reqctx.ErrMissing) {
		t.Fatalf("err = %v", err)
	}
	if _, err := g.Generate(ctx(), genReq()); err != nil {
		t.Fatal(err)
	}
}
