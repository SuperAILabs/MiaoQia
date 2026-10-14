// Package consumer 是事件消费框架：按消费组读取主题，校验信封，在子系统数据库事务中以事件 id 去重并调用处理函数，
// 失败按退避重试，超过上限转入死信主题 dlq.<子系统> 并告警（docs/03-architecture/03-integration-contracts.md 第 3.4 节）。
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/tracing"
)

// Handler 处理一条事件；返回错误时事务回滚并重试。
type Handler func(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error

// Consumer 是一个消费者（一个消费组）。
type Consumer struct {
	Name        string // 消费者标识，同时作为消费组与去重键，例如 operations.probe-completion
	Subsystem   string // 所属子系统
	DeadLetter  string // 死信主题，默认 dlq.<子系统>
	Brokers     []string
	Topics      []string
	Handlers    map[string]Handler // 按事件类型；未登记的类型直接确认（消费者必须容忍新事件类型）
	DB          *db.DB
	MaxAttempts int           // 默认 5
	Backoff     time.Duration // 首次重试间隔，默认 200ms，逐次翻倍
	Log         *slog.Logger

	client *kgo.Client
}

// Run 消费直到 ctx 结束。
func (c *Consumer) Run(ctx context.Context) error {
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 5
	}
	if c.Backoff == 0 {
		c.Backoff = 200 * time.Millisecond
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.DeadLetter == "" {
		c.DeadLetter = "dlq." + c.Subsystem
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(c.Brokers...), kgo.ConsumerGroup(c.Name), kgo.ConsumeTopics(c.Topics...),
		kgo.DisableAutoCommit(), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		return err
	}
	c.client = cl
	defer cl.Close()
	for {
		fs := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := fs.Err0(); err != nil && !errors.Is(err, context.Canceled) {
			c.Log.Warn("拉取失败", "consumer", c.Name, "error", err)
		}
		var done []*kgo.Record
		fs.EachRecord(func(r *kgo.Record) {
			if err := c.handle(ctx, r); err != nil {
				return // ctx 结束：不确认，重启后重新投递
			}
			done = append(done, r)
		})
		if len(done) > 0 {
			if err := cl.CommitRecords(ctx, done...); err != nil && ctx.Err() == nil {
				c.Log.Warn("确认位点失败", "consumer", c.Name, "error", err)
			}
		}
	}
}

func (c *Consumer) handle(ctx context.Context, r *kgo.Record) error {
	var env outbox.Envelope
	if err := json.Unmarshal(r.Value, &env); err != nil {
		return c.deadLetter(ctx, r, fmt.Errorf("信封无法解析：%w", err))
	}
	if err := checkEnvelope(r.Topic, &env); err != nil {
		return c.deadLetter(ctx, r, err)
	}
	h, ok := c.Handlers[env.Type]
	if !ok {
		return nil
	}
	ectx := reqctx.Service(reqctx.With(ctx, reqctx.Context{Tenant: env.Tenant, Cell: env.Cell, Purpose: "event"}), c.Subsystem)
	ectx, span := tracing.Tracer(c.Subsystem).Start(tracing.WithTraceparent(ectx, env.Traceparent), "consume "+env.Type,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attribute.String("messaging.destination.name", r.Topic), attribute.String("cloudevents.event_id", env.ID),
			attribute.String("messaging.consumer.group.name", c.Name)))
	defer span.End()

	var last error
	for attempt := 1; attempt <= c.MaxAttempts; attempt++ {
		last = c.DB.Tx(ectx, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ectx, `INSERT INTO kernel_inbox (consumer, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, c.Name, env.ID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				span.SetAttributes(attribute.Bool("miaoqia.duplicate", true))
				return nil // 已处理过：重复投递
			}
			return h(ectx, tx, &env)
		})
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.Log.Warn("事件处理失败，重试", "consumer", c.Name, "event", env.ID, "type", env.Type, "attempt", attempt, "error", last)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.Backoff << (attempt - 1)):
		}
	}
	span.SetStatus(codes.Error, last.Error())
	return c.deadLetter(ectx, r, last)
}

// checkEnvelope 校验信封与主题一致：单元主题只接受该单元的租户事件，公共主题只接受公共域事件。
func checkEnvelope(topic string, env *outbox.Envelope) error {
	if err := env.Validate(); err != nil {
		return err
	}
	prefix, _, _ := strings.Cut(topic, ".")
	switch {
	case prefix == "public" && env.Zone != reqctx.ZonePublic,
		prefix == "platform" && env.Zone != reqctx.ZonePlatform,
		strings.HasPrefix(prefix, "cell-") && (env.Zone != reqctx.ZoneTenant || "cell-"+env.Cell != prefix):
		return fmt.Errorf("事件 %s 的域 %s / 单元 %q 与主题 %s 不符", env.ID, env.Zone, env.Cell, topic)
	}
	return nil
}

func (c *Consumer) deadLetter(ctx context.Context, r *kgo.Record, cause error) error {
	c.Log.Error("事件转入死信主题", "consumer", c.Name, "topic", r.Topic, "offset", r.Offset, "error", cause)
	dl := &kgo.Record{Topic: c.DeadLetter, Key: r.Key, Value: r.Value, Headers: append(r.Headers,
		kgo.RecordHeader{Key: "dlq_consumer", Value: []byte(c.Name)},
		kgo.RecordHeader{Key: "dlq_source_topic", Value: []byte(r.Topic)},
		kgo.RecordHeader{Key: "dlq_error", Value: []byte(cause.Error())})}
	return c.client.ProduceSync(ctx, dl).FirstErr()
}
