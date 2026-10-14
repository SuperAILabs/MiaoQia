// Package relay 是发件箱中继：读取各子系统数据库的 kernel_outbox，按 seq 顺序发布到 Kafka 并标记。
// 投递语义为至少一次；消费者以事件 id 去重。每个数据库同一时刻只有一个中继实例工作（会话级咨询锁），
// 多副本部署时其余实例待命。
package relay

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Source 是一个子系统数据库（以 outbox_relay 账号连接）。
type Source struct {
	Name string // 子系统名，用于日志与指标
	DSN  string
}

// Relay 发布若干数据库的发件箱。
type Relay struct {
	Sources   []Source
	Producer  *kgo.Client
	Batch     int           // 每批最多发布条数，默认 500
	Poll      time.Duration // 无通知时的轮询间隔，默认 1s
	Log       *slog.Logger
	published func(source string, n int) // 测试钩子
}

const lockKey = 7_302_174_133 // 中继咨询锁

// Run 为每个数据库启动一个发布循环，直到 ctx 结束。
func (r *Relay) Run(ctx context.Context) error {
	if r.Batch == 0 {
		r.Batch = 500
	}
	if r.Poll == 0 {
		r.Poll = time.Second
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	errc := make(chan error, len(r.Sources))
	for _, s := range r.Sources {
		go func() { errc <- r.loop(ctx, s) }()
	}
	var first error
	for range r.Sources {
		if err := <-errc; err != nil && !errors.Is(err, context.Canceled) && first == nil {
			first = err
		}
	}
	return first
}

func (r *Relay) loop(ctx context.Context, s Source) error {
	for {
		err := r.serve(ctx, s)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.Log.Warn("发件箱中继重连", "source", s.Name, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (r *Relay) serve(ctx context.Context, s Source) error {
	conn, err := pgx.Connect(ctx, s.DSN)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var locked bool
	for !locked {
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&locked); err != nil {
			return err
		}
		if !locked {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second): // 其他实例持有锁：待命
			}
		}
	}
	if _, err := conn.Exec(ctx, `LISTEN kernel_outbox`); err != nil {
		return err
	}
	for {
		n, err := r.publishBatch(ctx, conn)
		if err != nil {
			return err
		}
		if r.published != nil && n > 0 {
			r.published(s.Name, n)
		}
		if n == r.Batch {
			continue // 还有积压
		}
		wctx, cancel := context.WithTimeout(ctx, r.Poll)
		_, err = conn.WaitForNotification(wctx)
		cancel()
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (r *Relay) publishBatch(ctx context.Context, conn *pgx.Conn) (int, error) {
	rows, err := conn.Query(ctx, `SELECT seq, event_id, topic, partition_key, envelope::text
		FROM kernel_outbox WHERE published_at IS NULL ORDER BY seq LIMIT $1`, r.Batch)
	if err != nil {
		return 0, err
	}
	var recs []*kgo.Record
	var seqs []int64
	for rows.Next() {
		var seq int64
		var id, topic, key, env string
		if err := rows.Scan(&seq, &id, &topic, &key, &env); err != nil {
			rows.Close()
			return 0, err
		}
		seqs = append(seqs, seq)
		recs = append(recs, &kgo.Record{Topic: topic, Key: []byte(key), Value: []byte(env),
			Headers: []kgo.RecordHeader{{Key: "ce_id", Value: []byte(id)}, {Key: "content-type", Value: []byte("application/cloudevents+json")}}})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(recs) == 0 {
		return 0, nil
	}
	// 同一分区键的记录按 seq 顺序进入同一分区；生产者开启幂等，失败时整批重试（至少一次）
	if err := r.Producer.ProduceSync(ctx, recs...).FirstErr(); err != nil {
		return 0, err
	}
	if _, err := conn.Exec(ctx, `UPDATE kernel_outbox SET published_at = now() WHERE seq = ANY($1)`, seqs); err != nil {
		return 0, err
	}
	return len(recs), nil
}
