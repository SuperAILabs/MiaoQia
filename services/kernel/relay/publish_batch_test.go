package relay

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestPublishBatchChecksAllRowsBeforeKafkaAndMarking(t *testing.T) {
	rejected := errors.New("topic outside source contract")
	fixture := []batchTestRow{
		{seq: 10, id: "first", topic: "public.evidence", key: "key", envelope: `{"value":"first"}`},
		{seq: 12, id: "second", topic: "cell-01.evidence", key: "key", envelope: `{"value":"second"}`},
	}
	for _, test := range []struct {
		name      string
		validate  func(string, []byte) error
		produce   error
		wantError error
		wantCalls int
		wantCount int
	}{
		{name: "rejected second row", wantError: rejected, validate: func(topic string, envelope []byte) error {
			if topic == fixture[0].topic && string(envelope) == fixture[0].envelope {
				return nil
			}
			if topic == fixture[1].topic && string(envelope) == fixture[1].envelope {
				return rejected
			}
			return errors.New("validator must receive the original topic and envelope")
		}},
		{name: "legacy without validation", wantCalls: 1, wantCount: len(fixture)},
		{name: "valid source batch", wantCalls: 1, wantCount: len(fixture), validate: func(string, []byte) error { return nil }},
		{name: "producer fails", wantCalls: 1, produce: rejected, wantError: rejected},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := &batchTestConn{rows: slices.Clone(fixture)}
			producer := &batchTestProducer{err: test.produce}
			pipeline := Relay{Producer: producer, Batch: len(fixture)}
			count, err := pipeline.publishBatch(t.Context(), conn, Source{Name: "evidence", Validate: test.validate})
			expectedUpdates := 0
			if test.wantCount > 0 {
				expectedUpdates = 1
			}
			if !errors.Is(err, test.wantError) || count != test.wantCount || producer.calls != test.wantCalls || conn.marked != test.wantCount || conn.execCalls != expectedUpdates {
				t.Fatalf("count=%d, error=%v, ProduceSync calls=%d, Exec calls=%d, rows marked=%d", count, err, producer.calls, conn.execCalls, conn.marked)
			}
			if test.wantCalls > 0 && len(producer.records) != len(fixture) {
				t.Fatalf("produced %d records, want %d", len(producer.records), len(fixture))
			}
			for index, row := range conn.rows {
				wantRow := fixture[index]
				wantRow.published = test.wantCount > 0
				if row != wantRow {
					t.Fatalf("row %d changed unexpectedly: %+v", index, row)
				}
				if test.wantCalls > 0 {
					record := producer.records[index]
					if record.Topic != row.topic || string(record.Key) != row.key || string(record.Value) != row.envelope {
						t.Fatalf("record %d changed or out of order: %+v", index, record)
					}
				}
			}
		})
	}
}

type batchTestRow struct {
	seq                      int64
	id, topic, key, envelope string
	published                bool
}

type batchTestConn struct {
	rows      []batchTestRow
	execCalls int
	marked    int
}

func (conn *batchTestConn) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &batchTestRows{rows: conn.rows}, nil
}

func (conn *batchTestConn) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	conn.execCalls++
	seqs := args[0].([]int64)
	for index, row := range conn.rows {
		if slices.Contains(seqs, row.seq) {
			conn.rows[index].published = true
			conn.marked++
		}
	}
	return pgconn.CommandTag{}, nil
}

type batchTestRows struct {
	pgx.Rows
	rows    []batchTestRow
	current batchTestRow
}

func (*batchTestRows) Close()     {}
func (*batchTestRows) Err() error { return nil }

func (rows *batchTestRows) Next() bool {
	if len(rows.rows) == 0 {
		return false
	}
	rows.current, rows.rows = rows.rows[0], rows.rows[1:]
	return true
}

func (rows *batchTestRows) Scan(dest ...any) error {
	*dest[0].(*int64) = rows.current.seq
	*dest[1].(*string) = rows.current.id
	*dest[2].(*string) = rows.current.topic
	*dest[3].(*string) = rows.current.key
	*dest[4].(*string) = rows.current.envelope
	return nil
}

type batchTestProducer struct {
	err     error
	calls   int
	records []*kgo.Record
}

func (producer *batchTestProducer) ProduceSync(_ context.Context, records ...*kgo.Record) kgo.ProduceResults {
	producer.calls++
	producer.records = records
	return kgo.ProduceResults{{Err: producer.err}}
}
