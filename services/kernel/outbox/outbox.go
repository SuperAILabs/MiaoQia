// Package outbox 是事务性发件箱：事件与业务写入同事务追加到 kernel_outbox，由 outbox-relay 发布。
// 信封为 CloudEvents 1.0（docs/03-architecture/03-integration-contracts.md 第 3 节）。
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/tracing"
)

// Envelope 是 CloudEvents 1.0 结构化模式的信封。
type Envelope struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Time            time.Time       `json:"time"`
	Subject         string          `json:"subject,omitempty"`
	DataContentType string          `json:"datacontenttype"`
	DataSchema      string          `json:"dataschema,omitempty"`
	Zone            reqctx.Zone     `json:"miaoqiazone"`
	Tenant          string          `json:"miaoqiatenant,omitempty"`
	Cell            string          `json:"miaoqiacell,omitempty"`
	Traceparent     string          `json:"traceparent,omitempty"`
	Data            json.RawMessage `json:"data"`
}

// Validate 检查信封的域与租户一致性：租户事件必须带租户与单元，公共域事件必须为空。
func (e *Envelope) Validate() error {
	switch e.Zone {
	case reqctx.ZonePublic:
		if e.Tenant != "" || e.Cell != "" {
			return fmt.Errorf("公共域事件 %s 不得携带租户或单元", e.ID)
		}
	case reqctx.ZoneTenant:
		if e.Tenant == "" || e.Cell == "" {
			return fmt.Errorf("租户事件 %s 必须携带租户与单元", e.ID)
		}
	case reqctx.ZonePlatform:
	default:
		return fmt.Errorf("事件 %s 的 miaoqiazone 取值非法：%q", e.ID, e.Zone)
	}
	if e.SpecVersion != "1.0" || e.ID == "" || e.Type == "" || e.Source == "" {
		return fmt.Errorf("事件 %s 缺少 CloudEvents 必填属性", e.ID)
	}
	return nil
}

// Namespace 返回事件类型的命名空间（<namespace>.<entity>.<verb>.v<n> 的第一段）。
func (e *Envelope) Namespace() string { ns, _, _ := strings.Cut(e.Type, "."); return ns }

// Event 是要发布的事件。
type Event struct {
	Type         string // 例如 operations.probe.requested.v1
	Subject      string // 聚合标识
	Data         any
	PartitionKey string // 缺省为 Subject
	Topic        string // 缺省按发布方命名空间与域推导（Topic 函数）
}

// Writer 为一个子系统追加事件。
type Writer struct {
	Subsystem string      // 例如 operations
	Namespace string      // 主题命名空间，例如 operations、domain
	Zone      reqctx.Zone // 本子系统发布事件的数据域
}

// Topic 按约定推导主题：public.<ns>、platform.<ns>、cell-<id>.<ns>。
func Topic(zone reqctx.Zone, cell, namespace string) string {
	switch zone {
	case reqctx.ZoneTenant:
		return "cell-" + cell + "." + namespace
	case reqctx.ZonePlatform:
		return "platform." + namespace
	}
	return "public." + namespace
}

// Append 在事务 tx 中追加事件并通知中继。租户与主体取自请求上下文，不取自事件数据。
func (w Writer) Append(ctx context.Context, tx pgx.Tx, ev Event) (*Envelope, error) {
	rc, _ := reqctx.From(ctx)
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return nil, err
	}
	env := &Envelope{
		SpecVersion: "1.0", ID: ids.New("evt"), Source: "miaoqia/" + w.Subsystem, Type: ev.Type,
		Time: time.Now().UTC(), Subject: ev.Subject, DataContentType: "application/json",
		Zone: w.Zone, Traceparent: tracing.Traceparent(ctx), Data: data,
	}
	if w.Zone != reqctx.ZonePublic {
		env.Tenant, env.Cell = rc.Tenant, rc.Cell
	}
	if err := env.Validate(); err != nil {
		return nil, err
	}
	topic := ev.Topic
	if topic == "" {
		topic = Topic(w.Zone, env.Cell, w.Namespace)
	}
	key := ev.PartitionKey
	if key == "" {
		key = ev.Subject
	}
	if w.Zone == reqctx.ZoneTenant {
		key = env.Tenant + "/" + key // 租户事件按租户 + 聚合分区
	}
	body, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO kernel_outbox (event_id, topic, partition_key, envelope) VALUES ($1, $2, $3, $4)`,
		env.ID, topic, key, body); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify('kernel_outbox', '')`); err != nil {
		return nil, err
	}
	return env, nil
}
