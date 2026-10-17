package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/binding"
)

// GatewayBinding 是模型网关在绑定表中的名称。
const GatewayBinding = "ai-platform.Gateway"

// callTimeout 是一次网关调用（含主备切换）的总时限；单个端点的时限由模型目录配置。
const callTimeout = 2 * time.Minute

// Capability 是模型能力类别。
type Capability string

const (
	CapGenerate Capability = "generate"
	CapEmbed    Capability = "embed"
	CapRerank   Capability = "rerank"
	CapOCR      Capability = "ocr"
)

// Classification 是输入数据的最高分级（docs/03-architecture/06-security-and-tenancy.md 第 5 节）。
type Classification string

const (
	Public    Classification = "public"
	Internal  Classification = "internal"
	Sensitive Classification = "sensitive"
)

// Budget 是单次调用的上限；任何一项超出即在调用供应商前停止。零值表示不设该项上限，MaxCostMicroYuan 除外：零值表示不允许产生费用。
type Budget struct {
	MaxInputTokens   int   `json:"max_input_tokens,omitempty"`
	MaxOutputTokens  int   `json:"max_output_tokens,omitempty"`
	MaxCostMicroYuan int64 `json:"max_cost_micro_yuan"`
}

// Call 是每次调用共有的治理信息；租户、调用方与处理目的取自请求上下文。
type Call struct {
	Task           string         `json:"task"` // 调用场景，例如 evidence.category_enrich
	Classification Classification `json:"classification"`
	Budget         Budget         `json:"budget"`
}

// Input 是一段送入模型的内容。Trusted 为 false 的内容（公告正文、客户文档等）由适配器与指令隔离并标注为不可信数据。
type Input struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	Trusted bool   `json:"trusted,omitempty"`
}

// GenerateRequest 是结构化生成请求；输出必须通过 OutputSchema 校验。
type GenerateRequest struct {
	Call
	PromptRef    string          `json:"prompt_ref"` // 提示模板与版本，例如 evidence.category@3
	Instructions string          `json:"instructions"`
	Inputs       []Input         `json:"inputs"`
	OutputSchema json.RawMessage `json:"output_schema"`
}

// ModelRef 指向模型目录中的一个端点与模型版本（策略视图中“产出者版本”的来源）。
type ModelRef struct {
	Endpoint string `json:"endpoint"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Version  string `json:"version"`
}

// Usage 是一次调用的用量与费用。
type Usage struct {
	InputTokens   int   `json:"input_tokens"`
	OutputTokens  int   `json:"output_tokens"`
	CostMicroYuan int64 `json:"cost_micro_yuan"`
}

// GenerateResult 是已通过输出校验的结果。
type GenerateResult struct {
	Output       json.RawMessage `json:"output"`
	Model        ModelRef        `json:"model"`
	Usage        Usage           `json:"usage"`
	InvocationID string          `json:"invocation_id"`
}

// EmbedRequest 是向量化请求。
type EmbedRequest struct {
	Call
	Purpose string   `json:"purpose"` // 例如 chunk、query
	Texts   []string `json:"texts"`
}

// EmbedResult 的 Model 需记入索引版本；更换模型即构建新版本索引（ADR-0015）。
type EmbedResult struct {
	Vectors      [][]float32 `json:"vectors"`
	Model        ModelRef    `json:"model"`
	Usage        Usage       `json:"usage"`
	InvocationID string      `json:"invocation_id"`
}

// RerankRequest 是重排请求。
type RerankRequest struct {
	Call
	Query      string   `json:"query"`
	Candidates []string `json:"candidates"`
}

// RerankResult 的 Scores 与 Candidates 一一对应。
type RerankResult struct {
	Scores       []float64 `json:"scores"`
	Model        ModelRef  `json:"model"`
	Usage        Usage     `json:"usage"`
	InvocationID string    `json:"invocation_id"`
}

// Gateway 是模型网关。业务子系统只经此接口使用模型，不直接接供应商。
type Gateway interface {
	Generate(context.Context, GenerateRequest) (*GenerateResult, error)
	Embed(context.Context, EmbedRequest) (*EmbedResult, error)
	Rerank(context.Context, RerankRequest) (*RerankResult, error)
}

// 错误码。调用方可据此决定转人工复核或标为未知，不得改用其他途径调用模型。
const (
	CodeInvalidRequest      = "invalid_request"       // 请求缺字段或输出 Schema 无法编译
	CodeSensitiveNotAllowed = "sensitive_not_allowed" // 敏感数据默认不进入模型，租户授权尚未接入
	CodeNoEligibleModel     = "no_eligible_model"     // 没有满足能力、备案、地域与分级要求的端点
	CodeBudgetExceeded      = "budget_exceeded"       // 预计用量或费用超出本次上限，未调用供应商
	CodeProviderUnavailable = "provider_unavailable"  // 主备端点均不可用；不降级到不合规模型
	CodeOutputInvalid       = "output_invalid"        // 模型输出未通过 Schema 校验
	CodeProviderResponseBad = "provider_response_bad" // 供应商返回与请求不一致（如向量数量不符）
)

// Error 是网关的业务错误，可安全暴露给调用方。
type Error struct {
	Code   string
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return e.Code
	}
	return e.Code + ": " + e.Detail
}

// LocalGateway 把本地实现包装为经调用管线的接口。生成与重排计费且不保证幂等，不自动重试；端点间的主备切换在网关内完成。
func LocalGateway(impl Gateway) Gateway { return localGateway{impl} }

type localGateway struct{ impl Gateway }

func (g localGateway) Generate(ctx context.Context, req GenerateRequest) (*GenerateResult, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "Generate", Timeout: callTimeout},
		func(ctx context.Context) (*GenerateResult, error) { return g.impl.Generate(ctx, req) })
}

func (g localGateway) Embed(ctx context.Context, req EmbedRequest) (*EmbedResult, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "Embed", Timeout: callTimeout},
		func(ctx context.Context) (*EmbedResult, error) { return g.impl.Embed(ctx, req) })
}

func (g localGateway) Rerank(ctx context.Context, req RerankRequest) (*RerankResult, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "Rerank", Timeout: callTimeout},
		func(ctx context.Context) (*RerankResult, error) { return g.impl.Rerank(ctx, req) })
}
