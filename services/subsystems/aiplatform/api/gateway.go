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
	// CapClassify 与 CapScore 是决策能力：从任务给定的候选标签中选择，或在给定量表上评分，结果是类型化字段而不是自由文本。
	// 它们服务于边界明确的小决策（公告类型、品类初筛、消息意图与处理路由），结果只是业务判断的输入；
	// 金额门槛、权限、审批与策略排除仍由确定性规则执行，开放式分析与写作仍用 CapGenerate（ai-platform 规格第 11 节）。
	CapClassify Capability = "classify"
	CapScore    Capability = "score"
)

// Decision 报告该能力是否为决策能力（结果是类型化的标签或分数）。
func (c Capability) Decision() bool { return c == CapClassify || c == CapScore }

// Classification 是输入数据的最高分级（docs/03-architecture/06-security-and-tenancy.md 第 5 节）。
type Classification string

const (
	Public    Classification = "public"
	Internal  Classification = "internal"
	Sensitive Classification = "sensitive"
)

// Budget 是单次调用的上限；任何一项超出即在调用供应商前停止。零值表示不设该项上限，MaxCostMicroYuan 除外：零值表示不允许产生费用。
// 费用上限对所有计费单位生效：按请求计费的端点同样先按目录价格估算，不因不计 token 而视为免费。
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

// BillingUnit 是端点的计费单位，登记在模型目录中。
type BillingUnit string

const (
	BillToken   BillingUnit = "token"   // 按输入/输出 token 计费
	BillRequest BillingUnit = "request" // 按请求计费，与 token 数无关
)

// Usage 是一次调用的用量与费用。Unit 说明费用按什么计：按请求计费时 token 字段可以为零，Requests 与费用不为零。
type Usage struct {
	Unit          BillingUnit `json:"billing_unit"`
	InputTokens   int         `json:"input_tokens"`
	OutputTokens  int         `json:"output_tokens"`
	Requests      int         `json:"requests"`
	CostMicroYuan int64       `json:"cost_micro_yuan"`
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

// Label 是分类任务的一个候选标签。Definition 是业务定义，与标签一起送入模型并随任务版本固定。
type Label struct {
	ID         string `json:"id"`
	Definition string `json:"definition"`
}

// Scale 是评分任务的量表。分数必须落在 [Min, Max] 内；Definition 说明两端与刻度的业务含义。
type Scale struct {
	Min        float64 `json:"min"`
	Max        float64 `json:"max"`
	Definition string  `json:"definition"`
}

// ClassifyRequest 是单标签分类请求。Call.Task 是任务名，TaskVersion 固定标签集合、定义与评测结果；
// 端点只有在目录中声明支持该任务版本时才参与路由。
type ClassifyRequest struct {
	Call
	TaskVersion string  `json:"task_version"`
	Inputs      []Input `json:"inputs"`
	Labels      []Label `json:"labels"`
}

// ScoreRequest 是量表评分请求，任务版本的含义同 ClassifyRequest。
type ScoreRequest struct {
	Call
	TaskVersion string  `json:"task_version"`
	Inputs      []Input `json:"inputs"`
	Scale       Scale   `json:"scale"`
}

// Outcome 是决策结果的状态。未知与拒答是成功的调用结果（已计费、已计量），不是供应商故障；
// 调用方按任务策略转复核或升级，不得改选一个标签或默认为否。
type Outcome string

const (
	OutcomeDecided Outcome = "decided" // 给出了标签或分数
	OutcomeUnknown Outcome = "unknown" // 模型判断无法给出：标签外、歧义、证据不足或模型自身置信不足
	OutcomeRefused Outcome = "refused" // 模型拒绝回答（例如内容安全）
)

// UnknownReason 是模型返回未知时给出的原因。
type UnknownReason string

const (
	UnknownOutOfScope           UnknownReason = "out_of_scope"          // 输入不属于任何候选标签或量表范围
	UnknownAmbiguous            UnknownReason = "ambiguous"             // 多个标签同样可能
	UnknownInsufficientEvidence UnknownReason = "insufficient_evidence" // 输入缺少判断所需信息
	UnknownLowConfidence        UnknownReason = "low_confidence"        // 模型按自身机制判断置信不足
	UnknownUnspecified          UnknownReason = "unspecified"           // 供应商未给出原因
)

// ScoreMeaning 说明结果中的数值是什么，由模型目录按任务版本声明，主备端点必须一致。
// 网关不把任意数值称为概率或置信度：模型没有提供的数值保持为空，不由网关或适配器生成。
type ScoreMeaning string

const (
	MeaningNone        ScoreMeaning = "none"        // 分类只给标签，不给数值
	MeaningProbability ScoreMeaning = "probability" // 模型提供的所选标签概率，[0,1]；是否已对本任务校准由任务评测说明
	MeaningConfidence  ScoreMeaning = "confidence"  // 模型提供的未声明为概率的置信分，[0,1]，不可当作概率使用
	MeaningScale       ScoreMeaning = "scale"       // 请求量表上的位置，含义见 Scale.Definition
)

// Decision 是分类或评分的结果。阈值与转人工条件属于调用方的任务版本，网关不对结果套用阈值。
type Decision struct {
	TaskVersion   string        `json:"task_version"`
	Outcome       Outcome       `json:"outcome"`
	Label         string        `json:"label,omitempty"`          // 分类且 Outcome 为 decided 时必有，取自请求的候选标签
	Score         *float64      `json:"score,omitempty"`          // 只在模型提供时出现
	ScoreMeaning  ScoreMeaning  `json:"score_meaning"`            // 目录为该任务版本声明的数值含义
	UnknownReason UnknownReason `json:"unknown_reason,omitempty"` // Outcome 为 unknown 时必有
	Model         ModelRef      `json:"model"`
	Usage         Usage         `json:"usage"`
	InvocationID  string        `json:"invocation_id"`
}

// Gateway 是模型网关。业务子系统只经此接口使用模型，不直接接供应商。
type Gateway interface {
	Generate(context.Context, GenerateRequest) (*GenerateResult, error)
	Embed(context.Context, EmbedRequest) (*EmbedResult, error)
	Rerank(context.Context, RerankRequest) (*RerankResult, error)
	Classify(context.Context, ClassifyRequest) (*Decision, error)
	Score(context.Context, ScoreRequest) (*Decision, error)
}

// 错误码。调用方可据此决定转人工复核或标为未知，不得改用其他途径调用模型。
const (
	CodeInvalidRequest      = "invalid_request"       // 请求缺字段或输出 Schema 无法编译
	CodeSensitiveNotAllowed = "sensitive_not_allowed" // 敏感数据默认不进入模型，租户授权尚未接入
	CodeNoEligibleModel     = "no_eligible_model"     // 没有满足能力（决策能力还须声明该任务版本）、备案、地域与分级要求的端点
	CodeBudgetExceeded      = "budget_exceeded"       // 预计用量或费用超出本次上限，未调用供应商
	CodeProviderUnavailable = "provider_unavailable"  // 主备端点均不可用；不降级到不合规模型
	CodeOutputInvalid       = "output_invalid"        // 模型输出未通过 Schema 校验，或决策结果的标签不在候选中、分数越界、含义与目录声明不符
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

// LocalGateway 把本地实现包装为经调用管线的接口。各能力都计费且不保证幂等，不自动重试；端点间的主备切换在网关内完成。
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

func (g localGateway) Classify(ctx context.Context, req ClassifyRequest) (*Decision, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "Classify", Timeout: callTimeout},
		func(ctx context.Context) (*Decision, error) { return g.impl.Classify(ctx, req) })
}

func (g localGateway) Score(ctx context.Context, req ScoreRequest) (*Decision, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "Score", Timeout: callTimeout},
		func(ctx context.Context) (*Decision, error) { return g.impl.Score(ctx, req) })
}
