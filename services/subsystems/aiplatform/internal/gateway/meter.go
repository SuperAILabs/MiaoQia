package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/superailabs/miaoqia/services/subsystems/aiplatform/api"
)

// Invocation 是一次端点尝试或一次被拒绝调用的计量记录。不保存输入输出原文，只存内容哈希（ai-platform 规格第 8 节）。
type Invocation struct {
	ID             string
	CallID         string // 同一逻辑调用（含主备切换）的所有记录共享
	At             time.Time
	Tenant         string
	Principal      string
	Purpose        string
	Task           string
	PromptRef      string // 生成为提示模板版本，决策能力为任务@版本
	Capability     api.Capability
	Classification api.Classification
	Endpoint       string // 被拒绝的调用为空
	Model          api.ModelRef
	Attempt        int // 本次调用中的第几次端点尝试，从 1 开始；被拒绝的调用为 0
	InputHash      string
	Usage          api.Usage
	CostUnknown    bool // 失败尝试的供应商未报告用量，Usage 中的费用不代表实际费用
	Latency        time.Duration
	Status         string      // ok / failed / rejected
	Outcome        api.Outcome // 决策能力成功调用的结果状态；未知与拒答在此区分，不记为失败
	ErrorCode      string
}

const (
	StatusOK       = "ok"
	StatusFailed   = "failed"
	StatusRejected = "rejected"
)

// Meter 接收计量记录。持久化与 ai_platform.invocation.recorded.v1 事件随 M1 首次真实调用接入；记录失败不改变调用结果，但必须可观测。
type Meter interface {
	Record(context.Context, Invocation) error
}

// MemoryMeter 把计量记录保存在内存中，用于测试与本地开发。
type MemoryMeter struct {
	mu   sync.Mutex
	recs []Invocation
}

func (m *MemoryMeter) Record(_ context.Context, inv Invocation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, inv)
	return nil
}

// Records 返回已记录的计量记录副本。
func (m *MemoryMeter) Records() []Invocation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Invocation(nil), m.recs...)
}
