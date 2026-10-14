package relay_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/relay"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

type fixture struct {
	app     *db.DB
	brokers []string
	topics  map[string]string
	writer  outbox.Writer
}

func setup(t *testing.T) fixture {
	t.Helper()
	brokers := testenv.Brokers(t)
	d := testenv.NewDatabase(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, d.OwnerDSN, "", nil); err != nil {
		t.Fatal(err)
	}
	d.GrantRelay(t)
	app, err := db.Open(ctx, d.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	topics := testenv.Topics(t, "public.kt", "dlq.kt")
	prod, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(prod.Close)
	rctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	r := &relay.Relay{Sources: []relay.Source{{Name: "kt", DSN: d.RelayDSN}}, Producer: prod, Poll: 200 * time.Millisecond}
	go func() { _ = r.Run(rctx) }()
	return fixture{app: app, brokers: brokers, topics: topics,
		writer: outbox.Writer{Subsystem: "kt", Namespace: "kt", Zone: reqctx.ZonePublic}}
}

func (f fixture) publish(t *testing.T, typ, subject string, fail bool) {
	t.Helper()
	ctx := reqctx.Service(context.Background(), "kt")
	err := f.app.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := f.writer.Append(ctx, tx, outbox.Event{Type: typ, Subject: subject, Topic: f.topics["public.kt"],
			Data: map[string]any{"fail": fail}}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// 发件箱 → 中继 → 消费：至少一次投递、按事件 id 去重、失败重试后转死信。
func TestOutboxRelayConsumer(t *testing.T) {
	f := setup(t)
	var handled, failures atomic.Int32
	c := &consumer.Consumer{
		Name: "kt.probe", Subsystem: "kt", DeadLetter: f.topics["dlq.kt"], Brokers: f.brokers,
		Topics: []string{f.topics["public.kt"]}, DB: f.app, MaxAttempts: 3, Backoff: 20 * time.Millisecond,
		Handlers: map[string]consumer.Handler{"kt.probe.emitted.v1": func(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
			var d struct{ Fail bool }
			_ = json.Unmarshal(env.Data, &d)
			if d.Fail {
				failures.Add(1)
				return errors.New("模拟处理失败")
			}
			if rc, _ := reqctx.From(ctx); rc.Principal.ID != "svc:kt" {
				t.Errorf("消费者应以服务身份处理，得到 %q", rc.Principal.ID)
			}
			handled.Add(1)
			return nil
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	f.publish(t, "kt.probe.emitted.v1", "prb_1", false)
	f.publish(t, "kt.probe.emitted.v1", "prb_2", true)
	f.publish(t, "kt.other.happened.v1", "x_1", false) // 未登记的类型：确认并跳过

	waitFor(t, func() bool { return handled.Load() == 1 && failures.Load() == 3 }, "正常事件处理 1 次、失败事件重试 3 次")

	// 死信主题收到失败事件
	dl := readOne(t, f.brokers, f.topics["dlq.kt"])
	var env outbox.Envelope
	_ = json.Unmarshal(dl.Value, &env)
	if env.Subject != "prb_2" || header(dl, "dlq_error") == "" {
		t.Fatalf("死信记录不符：subject=%s error=%q", env.Subject, header(dl, "dlq_error"))
	}

	// 重复投递：把已处理事件原样再发一次，不应再次处理
	first := readOne(t, f.brokers, f.topics["public.kt"])
	prod, _ := kgo.NewClient(kgo.SeedBrokers(f.brokers...))
	defer prod.Close()
	if err := prod.ProduceSync(context.Background(), &kgo.Record{Topic: first.Topic, Key: first.Key, Value: first.Value}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	// 再发布一条新事件作为栅栏：它被处理时，重复事件必已被消费
	f.publish(t, "kt.probe.emitted.v1", "prb_3", false)
	waitFor(t, func() bool { return handled.Load() >= 2 }, "栅栏事件被处理")
	if n := handled.Load(); n != 2 {
		t.Fatalf("重复投递被再次处理：处理次数 %d", n)
	}
}

// 信封与主题的域不符（公共主题上的租户事件）直接转入死信。
func TestEnvelopeZoneMismatchGoesToDeadLetter(t *testing.T) {
	f := setup(t)
	var handled atomic.Int32
	c := &consumer.Consumer{Name: "kt.zone", Subsystem: "kt", DeadLetter: f.topics["dlq.kt"], Brokers: f.brokers,
		Topics: []string{f.topics["public.kt"]}, DB: f.app,
		Handlers: map[string]consumer.Handler{"kt.probe.emitted.v1": func(context.Context, pgx.Tx, *outbox.Envelope) error {
			handled.Add(1)
			return nil
		}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	env, _ := json.Marshal(outbox.Envelope{SpecVersion: "1.0", ID: "evt_x", Source: "miaoqia/kt", Type: "kt.probe.emitted.v1",
		Zone: reqctx.ZoneTenant, Tenant: "tnt_a", Cell: "01", Data: json.RawMessage(`{}`)})
	prod, _ := kgo.NewClient(kgo.SeedBrokers(f.brokers...))
	defer prod.Close()
	if err := prod.ProduceSync(context.Background(), &kgo.Record{Topic: f.topics["public.kt"], Value: env}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	dl := readOne(t, f.brokers, f.topics["dlq.kt"])
	if handled.Load() != 0 || header(dl, "dlq_error") == "" {
		t.Fatal("域不符的事件不应被处理，应转入死信")
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("超时：%s", what)
}

func readOne(t *testing.T, brokers []string, topic string) *kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		fs := cl.PollRecords(ctx, 1)
		if ctx.Err() != nil {
			t.Fatalf("读取 %s 超时", topic)
		}
		if recs := fs.Records(); len(recs) > 0 {
			return recs[0]
		}
	}
}

func header(r *kgo.Record, k string) string {
	for _, h := range r.Headers {
		if h.Key == k {
			return string(h.Value)
		}
	}
	return ""
}
