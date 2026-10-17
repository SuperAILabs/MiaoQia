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
	estInput   int // 按字符数估算的输入 token 上界
	maxOutput  int
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
	parts := []string{req.Instructions}
	for _, in := range req.Inputs {
		parts = append(parts, in.Name, in.Content)
	}
	p := plan{capability: api.CapGenerate, call: req.Call, promptRef: req.PromptRef, inputHash: hash(parts...), estInput: runes(parts...), maxOutput: req.Budget.MaxOutputTokens}
	preq := ProviderRequest{Instructions: req.Instructions, Inputs: req.Inputs, OutputSchema: req.OutputSchema, MaxOutputTokens: req.Budget.MaxOutputTokens}
	out, ref, usage, id, err := attempt(ctx, s, p, bad,
		func(ctx context.Context, pr Provider, ep Endpoint) (json.RawMessage, ProviderUsage, error) {
			return pr.Generate(ctx, ep, preq)
		},
		func(out json.RawMessage) error {
			var v any
			if err := json.Unmarshal(out, &v); err != nil {
				return &api.Error{Code: api.CodeOutputInvalid, Detail: "输出不是 JSON"}
			}
			if err := schema.Validate(v); err != nil {
				return &api.Error{Code: api.CodeOutputInvalid, Detail: err.Error()}
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return &api.GenerateResult{Output: out, Model: ref, Usage: usage, InvocationID: id}, nil
}

func (s *Service) Embed(ctx context.Context, req api.EmbedRequest) (*api.EmbedResult, error) {
	bad := ""
	if len(req.Texts) == 0 || req.Purpose == "" {
		bad = "缺少 texts 或 purpose"
	}
	p := plan{capability: api.CapEmbed, call: req.Call, inputHash: hash(req.Texts...), estInput: runes(req.Texts...)}
	out, ref, usage, id, err := attempt(ctx, s, p, bad,
		func(ctx context.Context, pr Provider, ep Endpoint) ([][]float32, ProviderUsage, error) {
			return pr.Embed(ctx, ep, req.Texts)
		},
		func(out [][]float32) error {
			if len(out) != len(req.Texts) {
				return &api.Error{Code: api.CodeProviderResponseBad, Detail: "向量数量与输入不符"}
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return &api.EmbedResult{Vectors: out, Model: ref, Usage: usage, InvocationID: id}, nil
}

func (s *Service) Rerank(ctx context.Context, req api.RerankRequest) (*api.RerankResult, error) {
	bad := ""
	if req.Query == "" || len(req.Candidates) == 0 {
		bad = "缺少 query 或 candidates"
	}
	parts := append([]string{req.Query}, req.Candidates...)
	p := plan{capability: api.CapRerank, call: req.Call, inputHash: hash(parts...), estInput: runes(parts...)}
	out, ref, usage, id, err := attempt(ctx, s, p, bad,
		func(ctx context.Context, pr Provider, ep Endpoint) ([]float64, ProviderUsage, error) {
			return pr.Rerank(ctx, ep, req.Query, req.Candidates)
		},
		func(out []float64) error {
			if len(out) != len(req.Candidates) {
				return &api.Error{Code: api.CodeProviderResponseBad, Detail: "分数数量与候选不符"}
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return &api.RerankResult{Scores: out, Model: ref, Usage: usage, InvocationID: id}, nil
}

// attempt 执行一次网关调用：校验请求与预算、按主备顺序尝试合规端点、校验输出并逐次计量。
// 只有端点不可用（含端点超时）才切换到下一个端点；输出校验失败或供应商拒绝请求时直接失败，避免重复计费。
func attempt[T any](ctx context.Context, s *Service, p plan, bad string,
	call func(context.Context, Provider, Endpoint) (T, ProviderUsage, error),
	check func(T) error,
) (T, api.ModelRef, api.Usage, string, error) {
	var zero T
	rc, ok := reqctx.From(ctx)
	if !ok {
		return zero, api.ModelRef{}, api.Usage{}, "", reqctx.ErrMissing
	}
	base := Invocation{
		Tenant: rc.Tenant, Principal: rc.Principal.ID, Purpose: rc.Purpose, Task: p.call.Task, PromptRef: p.promptRef,
		Capability: p.capability, Classification: p.call.Classification, InputHash: p.inputHash,
	}
	reject := func(err *api.Error) (T, api.ModelRef, api.Usage, string, error) {
		inv := base
		inv.Status, inv.ErrorCode = StatusRejected, err.Code
		s.record(ctx, inv)
		return zero, api.ModelRef{}, api.Usage{}, "", err
	}
	if bad == "" && p.call.Task == "" {
		bad = "缺少 task"
	}
	if bad != "" {
		return reject(&api.Error{Code: api.CodeInvalidRequest, Detail: bad})
	}
	routed, err := s.catalog.route(p.capability, p.call.Classification)
	if err != nil {
		var ae *api.Error
		errors.As(err, &ae)
		return reject(ae)
	}
	b := p.call.Budget
	if b.MaxInputTokens > 0 && p.estInput > b.MaxInputTokens {
		return reject(&api.Error{Code: api.CodeBudgetExceeded, Detail: "输入超出本次上限"})
	}
	var eligible []Endpoint
	for _, ep := range routed {
		if ep.MaxInputTokens > 0 && p.estInput > ep.MaxInputTokens {
			continue // 上下文不足的端点不参与本次调用
		}
		if ep.cost(p.estInput, p.maxOutput) > b.MaxCostMicroYuan {
			continue // 预计费用超出上限的端点不参与本次调用
		}
		eligible = append(eligible, ep)
	}
	if len(eligible) == 0 {
		return reject(&api.Error{Code: api.CodeBudgetExceeded, Detail: "没有在本次上下文与费用上限内的端点"})
	}
	for i, ep := range eligible {
		inv := base
		inv.Endpoint, inv.Model, inv.Attempt = ep.ID, ep.ref(), i+1
		timeout := ep.Timeout
		if timeout == 0 {
			timeout = defaultEndpointTimeout
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		out, pu, err := call(cctx, s.providers[ep.Provider], ep)
		cancel()
		inv.Latency = time.Since(start)
		inv.Usage = api.Usage{InputTokens: pu.InputTokens, OutputTokens: pu.OutputTokens, CostMicroYuan: ep.cost(pu.InputTokens, pu.OutputTokens)}
		if err != nil {
			inv.Status = StatusFailed
			if ctx.Err() != nil {
				inv.ErrorCode = api.CodeProviderUnavailable
				s.record(ctx, inv)
				return zero, api.ModelRef{}, api.Usage{}, "", ctx.Err()
			}
			if errors.Is(err, ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
				inv.ErrorCode = api.CodeProviderUnavailable
				s.record(ctx, inv)
				continue
			}
			inv.ErrorCode = api.CodeProviderResponseBad
			s.record(ctx, inv)
			return zero, api.ModelRef{}, api.Usage{}, "", &api.Error{Code: api.CodeProviderResponseBad, Detail: ep.ID}
		}
		if err := check(out); err != nil {
			var ae *api.Error
			errors.As(err, &ae)
			inv.Status, inv.ErrorCode = StatusFailed, ae.Code
			s.record(ctx, inv)
			return zero, api.ModelRef{}, api.Usage{}, "", err
		}
		inv.Status = StatusOK
		inv.ID = newID()
		s.record(ctx, inv)
		return out, ep.ref(), inv.Usage, inv.ID, nil
	}
	return zero, api.ModelRef{}, api.Usage{}, "", &api.Error{Code: api.CodeProviderUnavailable, Detail: "主备端点均不可用"}
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

func runes(parts ...string) int {
	n := 0
	for _, p := range parts {
		n += len([]rune(p))
	}
	return n
}
