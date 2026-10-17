package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/superailabs/miaoqia/services/subsystems/aiplatform/api"
)

// ErrUnavailable 表示端点暂时不可用（网络错误、限流、5xx、超时）；网关据此切换到下一个合规端点。
var ErrUnavailable = errors.New("模型端点不可用")

// ProviderRequest 是交给适配器的生成请求。适配器必须把 Trusted 为 false 的输入放在与指令分开的数据消息中并标注为不可信，不得拼入指令。
type ProviderRequest struct {
	Instructions    string
	Inputs          []api.Input
	OutputSchema    json.RawMessage
	MaxOutputTokens int
}

// ProviderUsage 是供应商报告的用量；费用由网关按目录的计费单位与价格计算。Requests 是供应商计费的请求数，按请求计费的端点使用。
// 失败的调用只有在 Reported 为真时才按其中的用量计费；否则费用未知，网关按调用前估算上界扣减本次预算。
type ProviderUsage struct {
	InputTokens  int
	OutputTokens int
	Requests     int
	Reported     bool
}

// ProviderClassifyRequest 是交给适配器的分类请求；输入的隔离要求同 ProviderRequest。
type ProviderClassifyRequest struct {
	Task            string // 任务@版本
	Inputs          []api.Input
	Labels          []api.Label
	MaxOutputTokens int
}

// ProviderScoreRequest 是交给适配器的评分请求。
type ProviderScoreRequest struct {
	Task            string
	Inputs          []api.Input
	Scale           api.Scale
	MaxOutputTokens int
}

// ProviderDecision 是适配器从供应商的类型化响应字段转换来的结果。适配器不得解析自由文本猜测标签，
// 也不得在供应商未提供数值时填写 Score。
type ProviderDecision struct {
	Outcome       api.Outcome
	Label         string
	Score         *float64
	UnknownReason api.UnknownReason
}

// Provider 是供应商适配器。适配器只负责协议转换，不做路由、预算与校验。
type Provider interface {
	Generate(ctx context.Context, ep Endpoint, req ProviderRequest) (json.RawMessage, ProviderUsage, error)
	Embed(ctx context.Context, ep Endpoint, texts []string) ([][]float32, ProviderUsage, error)
	Rerank(ctx context.Context, ep Endpoint, query string, candidates []string) ([]float64, ProviderUsage, error)
	Classify(ctx context.Context, ep Endpoint, req ProviderClassifyRequest) (ProviderDecision, ProviderUsage, error)
	Score(ctx context.Context, ep Endpoint, req ProviderScoreRequest) (ProviderDecision, ProviderUsage, error)
}

// StubBehavior 是故障桩对某个端点的行为。
type StubBehavior struct {
	Err    error           // 非空时返回该错误（例如 ErrUnavailable）与 Usage，用于验证失败但已计费的情形
	Block  bool            // 阻塞直到上下文取消，用于验证端点时限
	Output json.RawMessage // 生成输出；为空时返回 {}
	Usage  ProviderUsage
	Dim    int // 向量维度，默认 4
	// Decision 是分类与评分的结果；为空时分类返回第一个候选标签、评分返回量表下限，均不带数值以外的字段。
	Decision *ProviderDecision
}

// Stub 是确定性的故障桩适配器：不发起网络调用，用于离线验证网关的路由、切换、预算、校验与计量。
type Stub struct {
	mu        sync.Mutex
	behaviors map[string]StubBehavior
	calls     []string
}

// NewStub 按端点 ID 配置故障桩行为；未配置的端点正常返回。
func NewStub(behaviors map[string]StubBehavior) *Stub {
	return &Stub{behaviors: behaviors}
}

// Calls 返回按顺序被调用的端点 ID。
func (s *Stub) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *Stub) behave(ctx context.Context, ep Endpoint) (StubBehavior, error) {
	s.mu.Lock()
	s.calls = append(s.calls, ep.ID)
	b := s.behaviors[ep.ID]
	s.mu.Unlock()
	if b.Block {
		<-ctx.Done()
		return b, ctx.Err()
	}
	return b, b.Err
}

func (s *Stub) Generate(ctx context.Context, ep Endpoint, _ ProviderRequest) (json.RawMessage, ProviderUsage, error) {
	b, err := s.behave(ctx, ep)
	if err != nil {
		return nil, b.Usage, err
	}
	if b.Output == nil {
		return json.RawMessage(`{}`), b.Usage, nil
	}
	return b.Output, b.Usage, nil
}

func (s *Stub) Embed(ctx context.Context, ep Endpoint, texts []string) ([][]float32, ProviderUsage, error) {
	b, err := s.behave(ctx, ep)
	if err != nil {
		return nil, b.Usage, err
	}
	dim := b.Dim
	if dim == 0 {
		dim = 4
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, dim)
		v[0] = float32(len([]rune(t)))
		out[i] = v
	}
	return out, b.Usage, nil
}

func (s *Stub) Rerank(ctx context.Context, ep Endpoint, _ string, candidates []string) ([]float64, ProviderUsage, error) {
	b, err := s.behave(ctx, ep)
	if err != nil {
		return nil, b.Usage, err
	}
	out := make([]float64, len(candidates))
	for i := range candidates {
		out[i] = 1 / float64(i+1)
	}
	return out, b.Usage, nil
}

func (s *Stub) Classify(ctx context.Context, ep Endpoint, req ProviderClassifyRequest) (ProviderDecision, ProviderUsage, error) {
	b, err := s.behave(ctx, ep)
	if err != nil {
		return ProviderDecision{}, b.Usage, err
	}
	if b.Decision != nil {
		return *b.Decision, b.Usage, nil
	}
	return ProviderDecision{Outcome: api.OutcomeDecided, Label: req.Labels[0].ID}, b.Usage, nil
}

func (s *Stub) Score(ctx context.Context, ep Endpoint, req ProviderScoreRequest) (ProviderDecision, ProviderUsage, error) {
	b, err := s.behave(ctx, ep)
	if err != nil {
		return ProviderDecision{}, b.Usage, err
	}
	if b.Decision != nil {
		return *b.Decision, b.Usage, nil
	}
	v := req.Scale.Min
	return ProviderDecision{Outcome: api.OutcomeDecided, Score: &v}, b.Usage, nil
}
