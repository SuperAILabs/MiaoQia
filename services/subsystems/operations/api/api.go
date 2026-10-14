// Package api 是 operations 的公开接口。规格见 docs/03-architecture/subsystems/operations.md。
package api

// Name 是子系统名。
const Name = "operations"

// 链路探针事件（contracts/events）。
const (
	ProbeRequested = "operations.probe.requested.v1"
	ProbeForwarded = "evidence.probe.forwarded.v1"
)

// ProbeRelation 是发起链路探针所需的授权关系，资源为 platform:miaoqia。
const ProbeRelation = "can_trigger_probe"

// Probe 是链路探针的状态。
type Probe struct {
	ID          string   `json:"probe_id"`
	Status      string   `json:"status"` // requested / completed
	RequestedBy string   `json:"requested_by"`
	Hops        []string `json:"hops"`
	RequestedAt string   `json:"requested_at"`
	CompletedAt string   `json:"completed_at,omitempty"`
}
