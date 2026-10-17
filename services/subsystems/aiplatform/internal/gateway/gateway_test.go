package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"math"
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
    billing: token
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
    billing: token
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
    billing: token
    input_price_per_1k: 1
    no_training_on_customer: true
  - id: gen-abroad
    provider: stub
    model: m-y
    version: "1"
    capabilities: [generate, embed, rerank]
    filed: true
    region: us
    billing: token
    input_price_per_1k: 1
    no_training_on_customer: true
  - id: emb
    provider: stub
    model: e-1
    version: "1"
    capabilities: [embed, rerank]
    filed: true
    region: cn
    max_input_tokens: 100
    billing: token
    input_price_per_1k: 100
    no_training_on_customer: true
  - id: dec-primary
    provider: stub
    model: d-1
    version: "1.0"
    capabilities: [classify, score]
    filed: true
    region: cn
    tier: 0
    billing: request
    request_price: 300
    timeout: 50ms
    no_training_on_customer: true
    decision_tasks:
      - {task: notice.doc_type, version: "2", capability: classify, score_meaning: probability}
      - {task: notice.relevance, version: "1", capability: score, score_meaning: scale}
  - id: dec-backup
    provider: stub
    model: d-llm
    version: "3"
    capabilities: [classify]
    filed: true
    region: cn
    tier: 1
    billing: token
    input_price_per_1k: 100
    output_price_per_1k: 100
    no_training_on_customer: true
    decision_tasks:
      - {task: notice.doc_type, version: "2", capability: classify, score_meaning: probability}
  - id: dec-intent
    provider: stub
    model: d-2
    version: "1"
    capabilities: [classify]
    filed: true
    region: cn
    billing: request
    request_price: 200
    no_training_on_customer: true
    decision_tasks:
      - {task: message.intent, version: "1", capability: classify, score_meaning: none}
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
	if res.Usage != (api.Usage{Unit: api.BillToken, InputTokens: 30, OutputTokens: 10, CostMicroYuan: 30 + 20}) {
		t.Fatalf("usage = %+v", res.Usage)
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
	r.Budget.MaxCostMicroYuan = 300 // 输入上界 187（UTF-8 字节，含 Schema）：主选预计 187+200，备选 94+50
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
	call := api.Call{Task: "search.chunk_embed", Classification: api.Public, Budget: api.Budget{MaxCostMicroYuan: 100}}
	e, err := s.Embed(ctx(), api.EmbedRequest{Call: call, Purpose: "chunk", Texts: []string{"管材", "阀门"}})
	if err != nil || len(e.Vectors) != 2 || e.Model.Endpoint != "emb" {
		t.Fatalf("embed: %+v %v", e, err)
	}
	if e.Usage != (api.Usage{Unit: api.BillToken, InputTokens: 6, CostMicroYuan: 1}) {
		t.Fatalf("embed usage: %+v", e.Usage)
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
	if _, err := c.route(api.CapOCR, api.Public, ""); code(err) != api.CodeNoEligibleModel {
		t.Fatalf("ocr: %v", err)
	}
}

func TestEmbedCountMismatchRejected(t *testing.T) {
	s, _, _ := setup(t, nil)
	s.providers["stub"] = shortEmbed{NewStub(nil)}
	_, err := s.Embed(ctx(), api.EmbedRequest{Call: api.Call{Task: "t", Classification: api.Public, Budget: api.Budget{MaxCostMicroYuan: 100}}, Purpose: "chunk", Texts: []string{"a", "b"}})
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
	const ep = `{id: a, provider: stub, model: m, version: "1", region: cn, no_training_on_customer: true, `
	for name, y := range map[string]string{
		"no billing":            `endpoints: [` + ep + `capabilities: [embed], input_price_per_1k: 1}]`,
		"token without price":   `endpoints: [` + ep + `capabilities: [embed], billing: token}]`,
		"request without price": `endpoints: [` + ep + `capabilities: [classify], billing: request, decision_tasks: [{task: t, version: "1", capability: classify, score_meaning: none}]}]`,
		"request with token":    `endpoints: [` + ep + `capabilities: [embed], billing: request, request_price: 1, input_price_per_1k: 1}]`,
		"decision without task": `endpoints: [` + ep + `capabilities: [classify], billing: request, request_price: 1}]`,
		"task without cap":      `endpoints: [` + ep + `capabilities: [embed], billing: request, request_price: 1, decision_tasks: [{task: t, version: "1", capability: classify, score_meaning: none}]}]`,
		"score as probability":  `endpoints: [` + ep + `capabilities: [score], billing: request, request_price: 1, decision_tasks: [{task: t, version: "1", capability: score, score_meaning: probability}]}]`,
		"classify as scale":     `endpoints: [` + ep + `capabilities: [classify], billing: request, request_price: 1, decision_tasks: [{task: t, version: "1", capability: classify, score_meaning: scale}]}]`,
		"meaning mismatch": `endpoints: [` + ep + `capabilities: [classify], billing: request, request_price: 1, decision_tasks: [{task: t, version: "1", capability: classify, score_meaning: probability}]},` +
			`{id: b, provider: stub, model: m, version: "1", region: cn, no_training_on_customer: true, capabilities: [classify], billing: request, request_price: 1, decision_tasks: [{task: t, version: "1", capability: classify, score_meaning: confidence}]}]`,
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

func f(v float64) *float64 { return &v }

func classifyReq() api.ClassifyRequest {
	return api.ClassifyRequest{
		Call:        api.Call{Task: "notice.doc_type", Classification: api.Public, Budget: api.Budget{MaxOutputTokens: 20, MaxCostMicroYuan: 1000}},
		TaskVersion: "2",
		Inputs:      []api.Input{{Name: "notice", Content: "某市供水管网改造项目中标结果公示"}},
		Labels: []api.Label{
			{ID: "tender", Definition: "招标公告"}, {ID: "award", Definition: "中标结果"},
			{ID: "correction", Definition: "更正公告"}, {ID: "other", Definition: "其他"},
		},
	}
}

func scoreReq() api.ScoreRequest {
	return api.ScoreRequest{
		Call:        api.Call{Task: "notice.relevance", Classification: api.Public, Budget: api.Budget{MaxCostMicroYuan: 1000}},
		TaskVersion: "1",
		Inputs:      []api.Input{{Name: "notice", Content: "球墨铸铁管采购"}},
		Scale:       api.Scale{Min: 1, Max: 5, Definition: "1 无关，5 与目标品类直接相关"},
	}
}

func TestClassifyDecidedPerRequestBilling(t *testing.T) {
	s, stub, meter := setup(t, map[string]StubBehavior{
		"dec-primary": {Decision: &ProviderDecision{Outcome: api.OutcomeDecided, Label: "award", Score: f(0.8)}},
	})
	d, err := s.Classify(ctx(), classifyReq())
	if err != nil {
		t.Fatal(err)
	}
	if d.Outcome != api.OutcomeDecided || d.Label != "award" || *d.Score != 0.8 || d.ScoreMeaning != api.MeaningProbability || d.TaskVersion != "2" || d.Model.Endpoint != "dec-primary" {
		t.Fatalf("decision = %+v", d)
	}
	// 按请求计费：没有 token 用量也不是免费。
	if d.Usage != (api.Usage{Unit: api.BillRequest, Requests: 1, CostMicroYuan: 300}) {
		t.Fatalf("usage = %+v", d.Usage)
	}
	recs := meter.Records()
	if len(recs) != 1 || recs[0].Outcome != api.OutcomeDecided || recs[0].PromptRef != "notice.doc_type@2" || recs[0].Capability != api.CapClassify || recs[0].ID != d.InvocationID {
		t.Fatalf("records = %+v", recs)
	}
	if !slices.Equal(stub.Calls(), []string{"dec-primary"}) {
		t.Fatalf("calls = %v", stub.Calls())
	}
}

func TestClassifyUnknownAndRefusedAreResultsNotFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		in     ProviderDecision
		reason api.UnknownReason
	}{
		"insufficient evidence": {ProviderDecision{Outcome: api.OutcomeUnknown, UnknownReason: api.UnknownInsufficientEvidence}, api.UnknownInsufficientEvidence},
		"out of scope":          {ProviderDecision{Outcome: api.OutcomeUnknown, UnknownReason: api.UnknownOutOfScope}, api.UnknownOutOfScope},
		"no reason given":       {ProviderDecision{Outcome: api.OutcomeUnknown}, api.UnknownUnspecified},
		"refused":               {ProviderDecision{Outcome: api.OutcomeRefused}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			s, stub, meter := setup(t, map[string]StubBehavior{"dec-primary": {Decision: &tc.in}})
			d, err := s.Classify(ctx(), classifyReq())
			if err != nil {
				t.Fatal(err)
			}
			if d.Outcome != tc.in.Outcome || d.Label != "" || d.Score != nil || d.UnknownReason != tc.reason || d.Usage.CostMicroYuan != 300 {
				t.Fatalf("decision = %+v", d)
			}
			// 未知不是故障：不切换到备选端点，计量为成功并记录结果状态。
			if !slices.Equal(stub.Calls(), []string{"dec-primary"}) {
				t.Fatalf("calls = %v", stub.Calls())
			}
			if r := meter.Records(); len(r) != 1 || r[0].Status != StatusOK || r[0].Outcome != tc.in.Outcome {
				t.Fatalf("records = %+v", r)
			}
		})
	}
}

func TestClassifyInvalidOutputs(t *testing.T) {
	for name, tc := range map[string]struct {
		req func(*api.ClassifyRequest)
		out ProviderDecision
	}{
		"label not offered":   {nil, ProviderDecision{Outcome: api.OutcomeDecided, Label: "contract"}},
		"no label":            {nil, ProviderDecision{Outcome: api.OutcomeDecided}},
		"probability > 1":     {nil, ProviderDecision{Outcome: api.OutcomeDecided, Label: "award", Score: f(1.2)}},
		"probability NaN":     {nil, ProviderDecision{Outcome: api.OutcomeDecided, Label: "award", Score: f(math.NaN())}},
		"unknown with label":  {nil, ProviderDecision{Outcome: api.OutcomeUnknown, Label: "other"}},
		"refused with score":  {nil, ProviderDecision{Outcome: api.OutcomeRefused, Score: f(0.1)}},
		"bad reason":          {nil, ProviderDecision{Outcome: api.OutcomeUnknown, UnknownReason: "guess"}},
		"decided with reason": {nil, ProviderDecision{Outcome: api.OutcomeDecided, Label: "award", UnknownReason: api.UnknownAmbiguous}},
		"bad outcome":         {nil, ProviderDecision{Outcome: "maybe"}},
		// 目录声明该任务只给标签，模型却返回数值：不当作置信度使用。
		"score not declared": {func(r *api.ClassifyRequest) { r.Task, r.TaskVersion = "message.intent", "1" }, ProviderDecision{Outcome: api.OutcomeDecided, Label: "award", Score: f(0.9)}},
	} {
		t.Run(name, func(t *testing.T) {
			s, stub, meter := setup(t, map[string]StubBehavior{"dec-primary": {Decision: &tc.out}, "dec-intent": {Decision: &tc.out}})
			r := classifyReq()
			if tc.req != nil {
				tc.req(&r)
			}
			_, err := s.Classify(ctx(), r)
			if code(err) != api.CodeOutputInvalid || len(stub.Calls()) != 1 {
				t.Fatalf("err=%v calls=%v", err, stub.Calls())
			}
			if rec := meter.Records(); len(rec) != 1 || rec[0].Status != StatusFailed || rec[0].Usage.Requests != 1 {
				t.Fatalf("records = %+v", rec)
			}
		})
	}
}

func TestClassifyLabelOnlyTask(t *testing.T) {
	s, _, _ := setup(t, map[string]StubBehavior{"dec-intent": {Decision: &ProviderDecision{Outcome: api.OutcomeDecided, Label: "other"}}})
	r := classifyReq()
	r.Task, r.TaskVersion = "message.intent", "1"
	d, err := s.Classify(ctx(), r)
	if err != nil || d.Score != nil || d.ScoreMeaning != api.MeaningNone || d.Model.Endpoint != "dec-intent" {
		t.Fatalf("d=%+v err=%v", d, err)
	}
}

func TestDecisionRoutingByTaskVersion(t *testing.T) {
	s, stub, _ := setup(t, nil)
	r := classifyReq()
	r.TaskVersion = "3" // 没有端点经评测准入该版本
	if _, err := s.Classify(ctx(), r); code(err) != api.CodeNoEligibleModel {
		t.Fatalf("version: %v", err)
	}
	r = classifyReq()
	r.Task = "notice.relevance" // 该任务在目录中是评分任务
	if _, err := s.Classify(ctx(), r); code(err) != api.CodeNoEligibleModel {
		t.Fatalf("capability: %v", err)
	}
	r = classifyReq()
	r.Classification = api.Sensitive
	if _, err := s.Classify(ctx(), r); code(err) != api.CodeSensitiveNotAllowed {
		t.Fatalf("sensitive: %v", err)
	}
	if len(stub.Calls()) != 0 {
		t.Fatalf("calls = %v", stub.Calls())
	}
}

func TestClassifyFailoverKeepsSemantics(t *testing.T) {
	for name, b := range map[string]StubBehavior{
		"unavailable": {Err: ErrUnavailable},
		"timeout":     {Block: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, stub, meter := setup(t, map[string]StubBehavior{
				"dec-primary": b,
				"dec-backup":  {Decision: &ProviderDecision{Outcome: api.OutcomeDecided, Label: "award", Score: f(0.7)}, Usage: ProviderUsage{InputTokens: 40, OutputTokens: 5}},
			})
			d, err := s.Classify(ctx(), classifyReq())
			if err != nil {
				t.Fatal(err)
			}
			// 备选端点对同一任务版本声明相同的数值含义；按 token 计费。
			if d.Model.Endpoint != "dec-backup" || d.ScoreMeaning != api.MeaningProbability || d.Usage != (api.Usage{Unit: api.BillToken, InputTokens: 40, OutputTokens: 5, CostMicroYuan: 4 + 1}) {
				t.Fatalf("decision = %+v", d)
			}
			if !slices.Equal(stub.Calls(), []string{"dec-primary", "dec-backup"}) {
				t.Fatalf("calls = %v", stub.Calls())
			}
			// 失败的按请求计费尝试没有供应商计费记录时不计费。
			recs := meter.Records()
			if len(recs) != 2 || recs[0].ErrorCode != api.CodeProviderUnavailable || recs[0].Usage != (api.Usage{Unit: api.BillRequest}) || recs[1].Attempt != 2 {
				t.Fatalf("records = %+v", recs)
			}
		})
	}
}

func TestDecisionBudget(t *testing.T) {
	t.Run("no cost allowed", func(t *testing.T) {
		s, stub, meter := setup(t, nil)
		r := classifyReq()
		r.Budget.MaxCostMicroYuan = 0
		if _, err := s.Classify(ctx(), r); code(err) != api.CodeBudgetExceeded || len(stub.Calls()) != 0 {
			t.Fatalf("err=%v calls=%v", err, stub.Calls())
		}
		if rec := meter.Records(); len(rec) != 1 || rec[0].Status != StatusRejected {
			t.Fatalf("records = %+v", rec)
		}
	})
	t.Run("request price over cap", func(t *testing.T) {
		s, stub, _ := setup(t, nil)
		r := classifyReq()
		r.Budget.MaxCostMicroYuan = 299 // 主选每次 300；备选预计 ceil(46*0.1)+2
		d, err := s.Classify(ctx(), r)
		if err != nil || d.Model.Endpoint != "dec-backup" || !slices.Equal(stub.Calls(), []string{"dec-backup"}) {
			t.Fatalf("d=%+v err=%v calls=%v", d, err, stub.Calls())
		}
	})
	t.Run("token output unbounded", func(t *testing.T) {
		// 没有输出上限时按 token 计费的备选无法限定费用，不参与；主选故障即明确失败。
		s, stub, _ := setup(t, map[string]StubBehavior{"dec-primary": {Err: ErrUnavailable}})
		r := classifyReq()
		r.Budget.MaxOutputTokens = 0
		if _, err := s.Classify(ctx(), r); code(err) != api.CodeProviderUnavailable || !slices.Equal(stub.Calls(), []string{"dec-primary"}) {
			t.Fatalf("err=%v calls=%v", err, stub.Calls())
		}
	})
}

func TestScore(t *testing.T) {
	s, _, meter := setup(t, map[string]StubBehavior{"dec-primary": {Decision: &ProviderDecision{Outcome: api.OutcomeDecided, Score: f(4)}}})
	d, err := s.Score(ctx(), scoreReq())
	if err != nil || *d.Score != 4 || d.ScoreMeaning != api.MeaningScale || d.Label != "" || d.Usage.CostMicroYuan != 300 {
		t.Fatalf("d=%+v err=%v", d, err)
	}
	if r := meter.Records(); r[0].Capability != api.CapScore || r[0].PromptRef != "notice.relevance@1" {
		t.Fatalf("records = %+v", r)
	}
	for name, out := range map[string]ProviderDecision{
		"above max":  {Outcome: api.OutcomeDecided, Score: f(6)},
		"below min":  {Outcome: api.OutcomeDecided, Score: f(0)},
		"no score":   {Outcome: api.OutcomeDecided},
		"with label": {Outcome: api.OutcomeDecided, Label: "x", Score: f(3)},
	} {
		s, _, _ := setup(t, map[string]StubBehavior{"dec-primary": {Decision: &out}})
		if _, err := s.Score(ctx(), scoreReq()); code(err) != api.CodeOutputInvalid {
			t.Fatalf("%s: %v", name, err)
		}
	}
	s, _, _ = setup(t, map[string]StubBehavior{"dec-primary": {Decision: &ProviderDecision{Outcome: api.OutcomeUnknown, UnknownReason: api.UnknownAmbiguous}}})
	if d, err := s.Score(ctx(), scoreReq()); err != nil || d.Outcome != api.OutcomeUnknown || d.Score != nil {
		t.Fatalf("unknown: %+v %v", d, err)
	}
}

func TestDecisionInvalidRequests(t *testing.T) {
	s, stub, _ := setup(t, nil)
	for name, mut := range map[string]func(*api.ClassifyRequest){
		"task":       func(r *api.ClassifyRequest) { r.Task = "" },
		"version":    func(r *api.ClassifyRequest) { r.TaskVersion = "" },
		"inputs":     func(r *api.ClassifyRequest) { r.Inputs = nil },
		"one label":  func(r *api.ClassifyRequest) { r.Labels = r.Labels[:1] },
		"duplicate":  func(r *api.ClassifyRequest) { r.Labels[1].ID = "tender" },
		"definition": func(r *api.ClassifyRequest) { r.Labels[2].Definition = "" },
	} {
		r := classifyReq()
		mut(&r)
		if _, err := s.Classify(ctx(), r); code(err) != api.CodeInvalidRequest {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, mut := range map[string]func(*api.ScoreRequest){
		"range":      func(r *api.ScoreRequest) { r.Scale.Min = 5 },
		"definition": func(r *api.ScoreRequest) { r.Scale.Definition = "" },
	} {
		r := scoreReq()
		mut(&r)
		if _, err := s.Score(ctx(), r); code(err) != api.CodeInvalidRequest {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(stub.Calls()) != 0 {
		t.Fatalf("calls = %v", stub.Calls())
	}
}

func TestLocalBindingDecision(t *testing.T) {
	s, _, _ := setup(t, nil)
	g := api.LocalGateway(s)
	if _, err := g.Classify(context.Background(), classifyReq()); !errors.Is(err, reqctx.ErrMissing) {
		t.Fatalf("err = %v", err)
	}
	if d, err := g.Classify(ctx(), classifyReq()); err != nil || d.Label != "tender" {
		t.Fatalf("classify: %+v %v", d, err)
	}
	if d, err := g.Score(ctx(), scoreReq()); err != nil || *d.Score != 1 {
		t.Fatalf("score: %+v %v", d, err)
	}
}

const failoverCatalog = `
endpoints:
  - {id: p, provider: stub, model: m, version: "1", capabilities: [classify], filed: true, region: cn, tier: 0, billing: request, request_price: 300, no_training_on_customer: true,
     decision_tasks: [{task: notice.doc_type, version: "2", capability: classify, score_meaning: probability}]}
  - {id: b, provider: stub, model: m, version: "2", capabilities: [classify], filed: true, region: cn, tier: 1, billing: request, request_price: 300, no_training_on_customer: true,
     decision_tasks: [{task: notice.doc_type, version: "2", capability: classify, score_meaning: probability}]}
`

func setupCatalog(t *testing.T, y string, b map[string]StubBehavior) (*Service, *Stub, *MemoryMeter) {
	t.Helper()
	c, err := ParseCatalog([]byte(y))
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

func TestFailoverChargesRemainingBudget(t *testing.T) {
	billedFailure := StubBehavior{Err: ErrUnavailable, Usage: ProviderUsage{Requests: 1, Reported: true}}
	silentFailure := StubBehavior{Err: ErrUnavailable}
	for name, tc := range map[string]struct {
		primary StubBehavior
		cap     int64
		ok      bool
		known   int64
		unknown bool
	}{
		// 主选失败但已计费：上限只够一次请求时不再调用备选。
		"billed failure, cap one call":  {billedFailure, 300, false, 300, false},
		"billed failure, cap two calls": {billedFailure, 600, true, 600, false},
		// 主选失败且未报告用量：按估算上界扣减，费用标为未知，不当作免费。
		"silent failure, cap below two": {silentFailure, 599, false, 0, true},
		"silent failure, cap two calls": {silentFailure, 600, true, 300, true},
	} {
		t.Run(name, func(t *testing.T) {
			s, stub, meter := setupCatalog(t, failoverCatalog, map[string]StubBehavior{"p": tc.primary})
			r := classifyReq()
			r.Budget.MaxCostMicroYuan = tc.cap
			d, err := s.Classify(ctx(), r)
			recs := meter.Records()
			var total int64
			for _, rec := range recs {
				total += rec.Usage.CostMicroYuan
				if rec.CallID == "" || rec.CallID != recs[0].CallID {
					t.Fatalf("同一逻辑调用的记录应共享 CallID: %+v", recs)
				}
			}
			if recs[0].CostUnknown != tc.unknown || total != tc.known {
				t.Fatalf("records = %+v", recs)
			}
			if !tc.ok {
				if code(err) != api.CodeBudgetExceeded || !slices.Equal(stub.Calls(), []string{"p"}) {
					t.Fatalf("err=%v calls=%v", err, stub.Calls())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := api.CallCost{CallID: recs[0].CallID, Attempts: 2, CostMicroYuan: tc.known, CostUnknown: tc.unknown}
			if d.Model.Endpoint != "b" || d.Usage.CostMicroYuan != 300 || d.Call != want {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
}

func TestNegativeBudgetRejected(t *testing.T) {
	s, stub, _ := setup(t, nil)
	negs := []func(*api.Budget){
		func(b *api.Budget) { b.MaxOutputTokens = -1000 },
		func(b *api.Budget) { b.MaxInputTokens = -1 },
		func(b *api.Budget) { b.MaxCostMicroYuan = -1 },
	}
	for i, neg := range negs {
		c := classifyReq()
		c.Budget.MaxCostMicroYuan = 0
		neg(&c.Budget)
		if _, err := s.Classify(ctx(), c); code(err) != api.CodeInvalidRequest {
			t.Fatalf("classify %d: %v", i, err)
		}
		sc := scoreReq()
		neg(&sc.Budget)
		if _, err := s.Score(ctx(), sc); code(err) != api.CodeInvalidRequest {
			t.Fatalf("score %d: %v", i, err)
		}
		g := genReq()
		neg(&g.Budget)
		if _, err := s.Generate(ctx(), g); code(err) != api.CodeInvalidRequest {
			t.Fatalf("generate %d: %v", i, err)
		}
		e := api.EmbedRequest{Call: api.Call{Task: "t", Classification: api.Public}, Purpose: "chunk", Texts: []string{"a"}}
		neg(&e.Budget)
		if _, err := s.Embed(ctx(), e); code(err) != api.CodeInvalidRequest {
			t.Fatalf("embed %d: %v", i, err)
		}
	}
	if len(stub.Calls()) != 0 {
		t.Fatalf("calls = %v", stub.Calls())
	}
	// 估算边界本身也不产生负费用。
	if _, ok := (Endpoint{Billing: api.BillToken, InputPricePer1K: 1, OutputPricePer1K: 1}).estimate(10, -1); ok {
		t.Fatal("负输出上限不能估算")
	}
}

func TestInputEstimateCoversFullPayload(t *testing.T) {
	// 输出 Schema 也发送给供应商，计入输入上界。
	s, stub, _ := setup(t, nil)
	r := genReq()
	r.Instructions, r.Inputs = "", nil
	r.Budget.MaxInputTokens = 1
	r.OutputSchema = json.RawMessage(`{"type":"object","description":"` + strings.Repeat("x", 10000) + `"}`)
	if _, err := s.Generate(ctx(), r); code(err) != api.CodeBudgetExceeded || len(stub.Calls()) != 0 {
		t.Fatalf("schema: err=%v calls=%v", err, stub.Calls())
	}
	// 非 ASCII 按 UTF-8 字节计：33 个汉字为 99+1，在 100 的上下文内；34 个为 102+1，超出。
	call := api.Call{Task: "search.chunk_embed", Classification: api.Public, Budget: api.Budget{MaxCostMicroYuan: 100}}
	if _, err := s.Embed(ctx(), api.EmbedRequest{Call: call, Purpose: "chunk", Texts: []string{strings.Repeat("长", 33)}}); err != nil {
		t.Fatalf("33: %v", err)
	}
	if _, err := s.Embed(ctx(), api.EmbedRequest{Call: call, Purpose: "chunk", Texts: []string{strings.Repeat("长", 34)}}); code(err) != api.CodeBudgetExceeded {
		t.Fatalf("34: %v", err)
	}
	// 适配器附加量计入端点的输入上界。
	ep := Endpoint{RequestOverheadTokens: 50}
	if got := ep.inputUpper(inputTokensUpper("长", "ab")); got != 4+3+50 {
		t.Fatalf("inputUpper = %d", got)
	}
}
