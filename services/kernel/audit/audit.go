// Package audit 是审计 SDK：审计条目与业务写入同事务追加到本子系统的发件箱（本地持久化），
// 经中继异步投递到 platform.audit，由 governance 不可篡改地存储（docs/03-architecture/subsystems/governance.md）。
// 审计条目只包含引用与摘要，不包含敏感内容本身。
package audit

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// EventType 是审计事件类型（contracts/events/audit/events.asyncapi.yaml）。
const EventType = "audit.entry.recorded.v1"

// Topic 是审计主题。
const Topic = "platform.audit"

// Entry 是一条审计条目：谁、何时、对什么、做了什么、结果、依据。
type Entry struct {
	Action   string // 例如 operations.probe.trigger
	Resource string // 资源引用，例如 prb_…
	Result   string // succeeded / denied / failed
	Basis    string // 依据，例如授权关系 can_trigger_probe
	// Delegation 在妙洽人员代表租户操作时填写（identity.md 第 7.8 节）；普通成员与服务的操作为 nil。
	Delegation *Delegation
}

// Delegation 是代表租户操作所依据的授予记录。
type Delegation struct {
	Kind  string `json:"kind"`  // support_access / service_delegation
	Grant string `json:"grant"` // 授予记录标识
}

// Data 是审计事件载荷。
type Data struct {
	Actor      string      `json:"actor"`
	ActorKind  string      `json:"actor_kind"`
	Tenant     string      `json:"tenant,omitempty"`
	Subsystem  string      `json:"subsystem"`
	Action     string      `json:"action"`
	Resource   string      `json:"resource"`
	Result     string      `json:"result"`
	Basis      string      `json:"basis,omitempty"`
	Delegation *Delegation `json:"delegation,omitempty"`
	OccurredAt time.Time   `json:"occurred_at"`
}

// Recorder 为一个子系统写审计条目。
type Recorder struct {
	Subsystem string
}

// Record 在事务 tx 中追加审计条目。
func (r Recorder) Record(ctx context.Context, tx pgx.Tx, e Entry) error {
	rc, _ := reqctx.From(ctx)
	w := outbox.Writer{Subsystem: r.Subsystem, Namespace: "audit", Zone: reqctx.ZonePlatform}
	_, err := w.Append(ctx, tx, outbox.Event{
		Type: EventType, Subject: e.Resource, Topic: Topic,
		PartitionKey: rc.Tenant + "/" + e.Resource,
		Data: Data{Actor: rc.Principal.ID, ActorKind: rc.Principal.Kind, Tenant: rc.Tenant, Subsystem: r.Subsystem,
			Action: e.Action, Resource: e.Resource, Result: e.Result, Basis: e.Basis, Delegation: e.Delegation, OccurredAt: time.Now().UTC()},
	})
	return err
}
