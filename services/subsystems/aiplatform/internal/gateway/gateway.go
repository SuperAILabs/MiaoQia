package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/aiplatform/api"
)

const defaultEndpointTimeout = 30 * time.Second

// Service 是模型网关的本地实现。
type Service struct {
	catalog   *Catalog
	providers map[string]Provider
	meter     Meter
	log       *slog.Logger
}

var _ api.Gateway = (*Service)(nil)

// New 组装网关；目录中每个端点的适配器都必须已注册。
func New(c *Catalog, providers map[string]Provider, meter Meter, log *slog.Logger) (*Service, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	for _, e := range c.Endpoints {
		if providers[e.Provider] == nil {
			return nil, fmt.Errorf("端点 %q 的适配器 %q 未注册", e.ID, e.Provider)
		}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{catalog: c, providers: providers, meter: meter, log: log}, nil
}

// plan 是一次调用在路由前已确定的内容。
type plan struct {
	capability api.Capability
	call       api.Call
	promptRef  string
	inputHash  string
	payload    int // 送给供应商的全部内容的输入 token 上界，见 inputTokensUpper
}

func (s *Service) Generate(ctx context.Context, req api.GenerateRequest) (*api.GenerateResult, error) {
	var schema *jsonschema.Schema
	var bad string
	switch {
	case req.PromptRef == "":
		bad = "缺少 prompt_ref"
	case req.Budget.MaxOutputTokens <= 0:
		bad = "生成必须设置 max_output_tokens"
	case len(req.OutputSchema) == 0:
		bad = "缺少 output_schema"
	default:
		c := jsonschema.NewCompiler()
		if err := c.AddResource("output.json", bytes.NewReader(req.OutputSchema)); err != nil {
			bad = "output_schema 无法解析"
		} else if schema, err = c.Compile("output.json"); err != nil {
			bad = "output_schema 无法编译"
		}
	}
	parts := append(appendInputs([]string{req.Instructions}, req.Inputs), string(req.OutputSchema))
	p := plan{capability: api.CapGenerate, call: req.Call, promptRef: req.PromptRef, inputHash: hash(parts...), payload: inputTokensUpper(parts...)}
	preq := ProviderRequest{Instructions: req.Instructions, Inputs: req.Inputs, OutputSchema: req.OutputSchema, MaxOutputTokens: req.Budget.MaxOutputTokens}
	out, r, err := attempt(ctx, s, p, bad,
		func(ctx context.Context, pr Provider, ep Endpoint) (json.RawMessage, ProviderUsage, error) {
			return pr.Generate(ctx, ep, preq)
		},
		func(out json.RawMessage, _ Endpoint) (api.Outcome, error) {
			var v any
			if err := json.Unmarshal(out, &v); err != nil {
				return "", &api.Error{Code: api.CodeOutputInvalid, Detail: "输出不是 JSON"}
			}
			if err := schema.Validate(v); err != nil {
				return "", &api.Error{Code: api.CodeOutputInvalid, Detail: err.Error()}
			}
			return "", nil
		})
	if err != nil {
		return nil, err
	}
	return &api.GenerateResult{Output: out, Model: r.ref, Usage: r.usage, Call: r.call, InvocationID: r.id}, nil
}

func (s *Service) Embed(ctx context.Context, req api.EmbedRequest) (*api.EmbedResult, error) {
	bad := ""
	if len(req.Texts) == 0 || req.Purpose == "" {
		bad = "缺少 texts 或 purpose"
	}
	p := plan{capability: api.CapEmbed, call: req.Call, inputHash: hash(req.Texts...), payload: inputTokensUpper(req.Texts...)}
	out, r, err := attempt(ctx, s, p, bad,
		func(ctx context.Context, pr Provider, ep Endpoint) ([][]float32, ProviderUsage, error) {
			return pr.Embed(ctx, ep, req.Texts)
		},
		func(out [][]float32, _ Endpoint) (api.Outcome, error) {
			if len(out) != len(req.Texts) {
				return "", &api.Error{Code: api.CodeProviderResponseBad, Detail: "向量数量与输入不符"}
			}
			return "", nil
		})
	if err != nil {
		return nil, err
	}
	return &api.EmbedResult{Vectors: out, Model: r.ref, Usage: r.usage, Call: r.call, InvocationID: r.id}, nil
}

func (s *Service) Rerank(ctx context.Context, req api.RerankRequest) (*api.RerankResult, error) {
	bad := ""
	if req.Query == "" || len(req.Candidates) == 0 {
		bad = "缺少 query 或 candidates"
	}
	parts := append([]string{req.Query}, req.Candidates...)
	p := plan{capability: api.CapRerank, call: req.Call, inputHash: hash(parts...), payload: inputTokensUpper(parts...)}
	out, r, err := attempt(ctx, s, p, bad,
		func(ctx context.Context, pr Provider, ep Endpoint) ([]float64, ProviderUsage, error) {
			return pr.Rerank(ctx, ep, req.Query, req.Candidates)
		},
		func(out []float64, _ Endpoint) (api.Outcome, error) {
			if len(out) != len(req.Candidates) {
				return "", &api.Error{Code: api.CodeProviderResponseBad, Detail: "分数数量与候选不符"}
			}
			return "", nil
		})
	if err != nil {
		return nil, err
	}
	return &api.RerankResult{Scores: out, Model: r.ref, Usage: r.usage, Call: r.call, InvocationID: r.id}, nil
}

// Classify 在任务给定的候选标签中选择一个。结果只接受供应商的类型化字段；标签不在候选中、数值越界或与目录声明的含义不符时
// 返回 output_invalid，不改选标签、不切换端点。未知与拒答作为成功结果返回。
func (s *Service) Classify(ctx context.Context, req api.ClassifyRequest) (*api.Decision, error) {
	bad := decisionBad(req.TaskVersion, req.Inputs)
	labels := map[string]bool{}
	parts := []string{req.Task, req.TaskVersion}
	for _, l := range req.Labels {
		if bad == "" && (l.ID == "" || l.Definition == "") {
			bad = "候选标签缺少 id 或定义"
		}
		if bad == "" && labels[l.ID] {
			bad = "候选标签重复"
		}
		labels[l.ID] = true
		parts = append(parts, l.ID, l.Definition)
	}
	if bad == "" && len(req.Labels) < 2 {
		bad = "候选标签至少两个"
	}
	parts = appendInputs(parts, req.Inputs)
	key := req.Task + "@" + req.TaskVersion
	p := plan{capability: api.CapClassify, call: req.Call, promptRef: key, inputHash: hash(parts...), payload: inputTokensUpper(parts...)}
	preq := ProviderClassifyRequest{Task: key, Inputs: req.Inputs, Labels: req.Labels, MaxOutputTokens: req.Budget.MaxOutputTokens}
	var meaning api.ScoreMeaning
	d, r, err := attempt(ctx, s, p, bad,
		func(ctx context.Context, pr Provider, ep Endpoint) (ProviderDecision, ProviderUsage, error) {
			return pr.Classify(ctx, ep, preq)
		},
		func(d ProviderDecision, ep Endpoint) (api.Outcome, error) {
			decl, _ := ep.decisionTask(key)
			meaning = decl.ScoreMeaning
			if d.Outcome == api.OutcomeDecided {
				switch {
				case !labels[d.Label]:
					return "", &api.Error{Code: api.CodeOutputInvalid, Detail: "标签不在候选中"}
				case d.Score != nil && meaning == api.MeaningNone:
					return "", &api.Error{Code: api.CodeOutputInvalid, Detail: "目录声明该任务不返回数值"}
				case d.Score != nil && !inRange(*d.Score, 0, 1):
					return "", &api.Error{Code: api.CodeOutputInvalid, Detail: "数值不在 [0,1]"}
				}
			}
			return checkOutcome(d)
		})
	if err != nil {
		return nil, err
	}
	return decision(req.TaskVersion, d, meaning, r), nil
}

// Score 在任务给定的量表上评分，结果约束同 Classify；分数必须落在量表范围内。
func (s *Service) Score(ctx context.Context, req api.ScoreRequest) (*api.Decision, error) {
	bad := decisionBad(req.TaskVersion, req.Inputs)
	sc := req.Scale
	if bad == "" && (!(sc.Min < sc.Max) || sc.Definition == "") {
		bad = "量表须满足 min < max 并说明含义"
	}
	parts := appendInputs([]string{req.Task, req.TaskVersion, fmt.Sprint(sc.Min, sc.Max), sc.Definition}, req.Inputs)
	key := req.Task + "@" + req.TaskVersion
	p := plan{capability: api.CapScore, call: req.Call, promptRef: key, inputHash: hash(parts...), payload: inputTokensUpper(parts...)}
	preq := ProviderScoreRequest{Task: key, Inputs: req.Inputs, Scale: sc, MaxOutputTokens: req.Budget.MaxOutputTokens}
	d, r, err := attempt(ctx, s, p, bad,
		func(ctx context.Context, pr Provider, ep Endpoint) (ProviderDecision, ProviderUsage, error) {
			return pr.Score(ctx, ep, preq)
		},
		func(d ProviderDecision, _ Endpoint) (api.Outcome, error) {
			if d.Outcome == api.OutcomeDecided {
				switch {
				case d.Label != "":
					return "", &api.Error{Code: api.CodeOutputInvalid, Detail: "评分结果不应带标签"}
				case d.Score == nil:
					return "", &api.Error{Code: api.CodeOutputInvalid, Detail: "缺少分数"}
				case !inRange(*d.Score, sc.Min, sc.Max):
					return "", &api.Error{Code: api.CodeOutputInvalid, Detail: "分数超出量表范围"}
				}
			}
			return checkOutcome(d)
		})
	if err != nil {
		return nil, err
	}
	return decision(req.TaskVersion, d, api.MeaningScale, r), nil
}

func decisionBad(version string, inputs []api.Input) string {
	switch {
	case version == "":
		return "缺少 task_version"
	case len(inputs) == 0:
		return "缺少 inputs"
	}
	return ""
}

// checkOutcome 校验未知与拒答结果不夹带标签或数值；供应商未给出未知原因时由 decision 记为 unspecified。
func checkOutcome(d ProviderDecision) (api.Outcome, error) {
	switch d.Outcome {
	case api.OutcomeDecided:
		if d.UnknownReason != "" {
			return "", &api.Error{Code: api.CodeOutputInvalid, Detail: "已决结果不应带未知原因"}
		}
	case api.OutcomeUnknown, api.OutcomeRefused:
		if d.Label != "" || d.Score != nil {
			return "", &api.Error{Code: api.CodeOutputInvalid, Detail: "未知或拒答结果不应带标签或数值"}
		}
		if d.Outcome == api.OutcomeRefused {
			if d.UnknownReason != "" {
				return "", &api.Error{Code: api.CodeOutputInvalid, Detail: "拒答结果不应带未知原因"}
			}
			break
		}
		switch d.UnknownReason {
		case "", api.UnknownOutOfScope, api.UnknownAmbiguous, api.UnknownInsufficientEvidence, api.UnknownLowConfidence, api.UnknownUnspecified:
		default:
			return "", &api.Error{Code: api.CodeOutputInvalid, Detail: "未知原因不在约定范围"}
		}
	default:
		return "", &api.Error{Code: api.CodeOutputInvalid, Detail: "结果状态不在约定范围"}
	}
	return d.Outcome, nil
}

func decision(version string, d ProviderDecision, meaning api.ScoreMeaning, r done) *api.Decision {
	if d.Outcome == api.OutcomeUnknown && d.UnknownReason == "" {
		d.UnknownReason = api.UnknownUnspecified
	}
	return &api.Decision{
		TaskVersion: version, Outcome: d.Outcome, Label: d.Label, Score: d.Score, ScoreMeaning: meaning,
		UnknownReason: d.UnknownReason, Model: r.ref, Usage: r.usage, Call: r.call, InvocationID: r.id,
	}
}

func inRange(v, lo, hi float64) bool { return !math.IsNaN(v) && v >= lo && v <= hi }

func appendInputs(parts []string, inputs []api.Input) []string {
	for _, in := range inputs {
		parts = append(parts, in.Name, in.Content)
	}
	return parts
}

// done 是成功调用的元数据。
type done struct {
	ref   api.ModelRef
	usage api.Usage
	call  api.CallCost
	id    string
}

// attempt 执行一次网关调用：校验请求与预算、按主备顺序尝试合规端点、校验输出并逐次计量。
// 只有端点不可用（含端点超时）才切换到下一个端点；输出校验失败或供应商拒绝请求时直接失败，避免重复计费。
// 每次尝试前按已发生的费用检查剩余上限，费用上限约束整次逻辑调用而不是单个端点。
// 决策能力的未知与拒答由 check 作为成功结果返回，不触发切换；路由已保证候选端点对该任务版本的输出语义一致。
func attempt[T any](ctx context.Context, s *Service, p plan, bad string,
	call func(context.Context, Provider, Endpoint) (T, ProviderUsage, error),
	check func(T, Endpoint) (api.Outcome, error),
) (T, done, error) {
	var zero T
	rc, ok := reqctx.From(ctx)
	if !ok {
		return zero, done{}, reqctx.ErrMissing
	}
	cc := api.CallCost{CallID: newID()}
	base := Invocation{
		CallID: cc.CallID, Tenant: rc.Tenant, Principal: rc.Principal.ID, Purpose: rc.Purpose, Task: p.call.Task, PromptRef: p.promptRef,
		Capability: p.capability, Classification: p.call.Classification, InputHash: p.inputHash,
	}
	reject := func(err *api.Error) (T, done, error) {
		inv := base
		inv.Status, inv.ErrorCode = StatusRejected, err.Code
		s.record(ctx, inv)
		return zero, done{}, err
	}
	b := p.call.Budget
	switch {
	case bad != "":
	case p.call.Task == "":
		bad = "缺少 task"
	case b.MaxInputTokens < 0 || b.MaxOutputTokens < 0 || b.MaxCostMicroYuan < 0:
		bad = "预算不能为负"
	}
	if bad != "" {
		return reject(&api.Error{Code: api.CodeInvalidRequest, Detail: bad})
	}
	routed, err := s.catalog.route(p.capability, p.call.Classification, p.promptRef)
	if err != nil {
		var ae *api.Error
		errors.As(err, &ae)
		return reject(ae)
	}
	type candidate struct {
		ep  Endpoint
		est int64 // 调用前费用上界
	}
	var eligible []candidate
	for _, ep := range routed {
		in := ep.inputUpper(p.payload)
		if (b.MaxInputTokens > 0 && in > b.MaxInputTokens) || (ep.MaxInputTokens > 0 && in > ep.MaxInputTokens) {
			continue // 超出本次输入上限或端点上下文的端点不参与本次调用
		}
		est, ok := ep.estimate(in, b.MaxOutputTokens)
		if !ok || est > b.MaxCostMicroYuan {
			continue // 预计费用超出上限或无法限定的端点不参与本次调用
		}
		eligible = append(eligible, candidate{ep, est})
	}
	if len(eligible) == 0 {
		return reject(&api.Error{Code: api.CodeBudgetExceeded, Detail: "没有在本次输入、上下文与费用上限内的端点"})
	}
	var spent int64 // 已发生费用的上界：已知费用加费用未知尝试的估算上界，饱和累加
	skipped := false
	for _, c := range eligible {
		ep := c.ep
		if c.est > b.MaxCostMicroYuan-spent { // 两者非负，减法不会溢出
			skipped = true // 主备切换后剩余上限不足以调用该端点
			continue
		}
		cc.Attempts++
		inv := base
		inv.Endpoint, inv.Model, inv.Attempt = ep.ID, ep.ref(), cc.Attempts
		timeout := ep.Timeout
		if timeout == 0 {
			timeout = defaultEndpointTimeout
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		out, pu, err := call(cctx, s.providers[ep.Provider], ep)
		cancel()
		inv.Latency = time.Since(start)
		known := false
		if err == nil || pu.Reported {
			inv.Usage, known = ep.usage(pu, err == nil)
		} else {
			inv.Usage = api.Usage{Unit: ep.Billing}
		}
		if known {
			spent = addSat(spent, inv.Usage.CostMicroYuan)
			cc.CostMicroYuan = addSat(cc.CostMicroYuan, inv.Usage.CostMicroYuan)
		} else {
			// 未报告、为负或超出可表示范围的用量：费用未知，按调用前估算上界扣减。
			inv.CostUnknown, cc.CostUnknown = true, true
			spent = addSat(spent, c.est)
		}
		if err != nil {
			inv.Status = StatusFailed
			if ctx.Err() != nil {
				inv.ErrorCode = api.CodeProviderUnavailable
				s.record(ctx, inv)
				return zero, done{}, ctx.Err()
			}
			if errors.Is(err, ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
				inv.ErrorCode = api.CodeProviderUnavailable
				s.record(ctx, inv)
				continue
			}
			inv.ErrorCode = api.CodeProviderResponseBad
			s.record(ctx, inv)
			return zero, done{}, &api.Error{Code: api.CodeProviderResponseBad, Detail: ep.ID}
		}
		outcome, err := check(out, ep)
		if err != nil {
			var ae *api.Error
			errors.As(err, &ae)
			inv.Status, inv.ErrorCode = StatusFailed, ae.Code
			s.record(ctx, inv)
			return zero, done{}, err
		}
		inv.Status, inv.Outcome = StatusOK, outcome
		inv.ID = newID()
		s.record(ctx, inv)
		return out, done{ref: ep.ref(), usage: inv.Usage, call: cc, id: inv.ID}, nil
	}
	if skipped {
		return zero, done{}, &api.Error{Code: api.CodeBudgetExceeded, Detail: "已调用端点不可用，剩余费用上限不足以调用其余端点"}
	}
	return zero, done{}, &api.Error{Code: api.CodeProviderUnavailable, Detail: "主备端点均不可用"}
}

// record 写入计量；失败只记日志，不改变调用结果。
func (s *Service) record(ctx context.Context, inv Invocation) {
	if inv.ID == "" {
		inv.ID = newID()
	}
	inv.At = time.Now().UTC()
	if s.meter == nil {
		return
	}
	if err := s.meter.Record(ctx, inv); err != nil {
		s.log.ErrorContext(ctx, "模型调用计量写入失败", "invocation", inv.ID, "task", inv.Task, "error", err)
	}
}

// newID 返回调用记录标识。类型化前缀在调用记录持久化（M1 首次真实调用）时登记到 contracts/identifiers.yaml。
func newID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), ulid.Monotonic(rand.Reader, 0)).String()
}

func hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// inputTokensUpper 是请求内容的输入 token 保守上界：UTF-8 字节数，每段再加 1。
// 字节级 BPE 与带字节回退的 SentencePiece 分词中每个 token 至少对应 1 个字节，分段时的前缀标记由每段加 1 覆盖；
// 字符数不是通用上界（一个汉字可能拆成多个 token）。适配器附加的模板与角色标记由端点的 request_overhead_tokens 覆盖。
// 真实适配器可在接入时换用供应商的精确计数，但不得低于实际发送内容。
func inputTokensUpper(parts ...string) int {
	n := 0
	for _, p := range parts {
		n += len(p) + 1
	}
	return n
}
