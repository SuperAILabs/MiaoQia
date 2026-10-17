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

// ProviderUsage 是供应商报告的 token 用量；费用由网关按目录价格计算。
type ProviderUsage struct {
	InputTokens  int
	OutputTokens int
}

// Provider 是供应商适配器。适配器只负责协议转换，不做路由、预算与校验。
type Provider interface {
	Generate(ctx context.Context, ep Endpoint, req ProviderRequest) (json.RawMessage, ProviderUsage, error)
	Embed(ctx context.Context, ep Endpoint, texts []string) ([][]float32, ProviderUsage, error)
	Rerank(ctx context.Context, ep Endpoint, query string, candidates []string) ([]float64, ProviderUsage, error)
}

// StubBehavior 是故障桩对某个端点的行为。
type StubBehavior struct {
	Err    error           // 非空时返回该错误（例如 ErrUnavailable）
	Block  bool            // 阻塞直到上下文取消，用于验证端点时限
	Output json.RawMessage // 生成输出；为空时返回 {}
	Usage  ProviderUsage
	Dim    int // 向量维度，默认 4
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
		return nil, ProviderUsage{}, err
	}
	if b.Output == nil {
		return json.RawMessage(`{}`), b.Usage, nil
	}
	return b.Output, b.Usage, nil
}

func (s *Stub) Embed(ctx context.Context, ep Endpoint, texts []string) ([][]float32, ProviderUsage, error) {
	b, err := s.behave(ctx, ep)
	if err != nil {
		return nil, ProviderUsage{}, err
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
		return nil, ProviderUsage{}, err
	}
	out := make([]float64, len(candidates))
	for i := range candidates {
		out[i] = 1 / float64(i+1)
	}
	return out, b.Usage, nil
}
