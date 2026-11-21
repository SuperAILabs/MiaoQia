package search

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
)

func observePublicDelivery(stream publicEventStream) consumer.DeliveryObserver {
	return func(ctx context.Context, tx pgx.Tx, delivery consumer.Delivery, outcome consumer.DeliveryOutcome, event *outbox.Envelope) error {
		if delivery.Topic != stream.topic {
			return errors.New("search: unexpected public projection topic")
		}
		if err := lockPublicProjection(ctx, tx); err != nil {
			return err
		}
		if outcome != consumer.DeliveryHandled {
			return recordPublicNonProjection(ctx, tx, delivery, string(outcome))
		}
		if err := validatePublicProducer(stream, event); err != nil {
			return err
		}
		if slices.Contains(stream.noops, event.Type) {
			return recordPublicNonProjection(ctx, tx, delivery, "verified_noop")
		}
		// The inbox may have deduplicated this delivery. Require the same validated
		// envelope in the journal before crediting this distinct Kafka position.
		tag, err := tx.Exec(ctx, `INSERT INTO sea_public_delivery AS delivery
			(topic, kafka_partition, kafka_offset, value_sha256, outcome, journal_seq)
			SELECT $1, $2, $3, $4, 'journaled', seq
			FROM sea_public_event WHERE event_id = $5 AND envelope = $6::jsonb
			ON CONFLICT (topic, kafka_partition, kafka_offset) DO UPDATE
			SET outcome = EXCLUDED.outcome, journal_seq = EXCLUDED.journal_seq
			WHERE delivery.value_sha256 = EXCLUDED.value_sha256 AND delivery.outcome <> 'conflict'
			AND (delivery.journal_seq IS NULL OR delivery.journal_seq = EXCLUDED.journal_seq)`,
			delivery.Topic, delivery.Partition, delivery.Offset, delivery.ValueSHA256[:], event.ID, event)
		if err == nil && tag.RowsAffected() != 1 {
			return errors.New("search: Kafka delivery conflicts with public journal evidence")
		}
		return err
	}
}

func recordPublicNonProjection(ctx context.Context, tx pgx.Tx, delivery consumer.Delivery, outcome string) error {
	// Replaying successfully journaled bytes cannot downgrade a receipt. A
	// different payload at the same Kafka position invalidates it permanently.
	var recorded string
	err := tx.QueryRow(ctx, `INSERT INTO sea_public_delivery AS delivery
		(topic, kafka_partition, kafka_offset, value_sha256, outcome)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (topic, kafka_partition, kafka_offset) DO UPDATE SET
		outcome = CASE
			WHEN delivery.value_sha256 <> EXCLUDED.value_sha256 THEN 'conflict'
			WHEN delivery.outcome = 'skipped' THEN EXCLUDED.outcome
			WHEN delivery.outcome = 'dead_lettered' AND EXCLUDED.outcome = 'verified_noop' THEN EXCLUDED.outcome
			ELSE delivery.outcome END,
		journal_seq = CASE WHEN delivery.value_sha256 = EXCLUDED.value_sha256 THEN delivery.journal_seq END
		RETURNING outcome`, delivery.Topic, delivery.Partition, delivery.Offset, delivery.ValueSHA256[:], outcome).Scan(&recorded)
	if err == nil && outcome == "verified_noop" && recorded != outcome {
		return errors.New("search: Kafka delivery conflicts with public journal evidence")
	}
	return err
}

type PublicReplayGap struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Offset    int64  `json:"offset"`
	Reason    string `json:"reason"`
}

// PublicReplayStatus describes the contiguous replay each generation can
// prove from its snapshot start. All reported watermarks are next offsets.
type PublicReplayStatus struct {
	AppliedSequence int64             `json:"applied_sequence"`
	Checked         PublicOffsets     `json:"checked"`
	Verified        PublicOffsets     `json:"verified"`
	Gaps            []PublicReplayGap `json:"gaps"`
}

// ReplayStatus checks a requested barrier against the durable journal and
// generation checkpoint. It detects delivery gaps known beyond that barrier.
func (p *PublicProjector) ReplayStatus(ctx context.Context, generation string, barrier PublicOffsets) (PublicReplayStatus, error) {
	var replay PublicReplayStatus
	err := p.withGeneration(ctx, generation, func(ctx context.Context, tx pgx.Tx, build publicBuild) error {
		var err error
		replay, err = checkPublicReplay(ctx, tx, build, barrier)
		return err
	})
	return replay, err
}

func checkPublicReplay(ctx context.Context, tx pgx.Tx, build publicBuild, barrier PublicOffsets) (PublicReplayStatus, error) {
	start := build.Basis.ReplayStart
	if !validPublicBasis(build.Basis) || !validPublicBarrier(start, barrier) {
		return PublicReplayStatus{}, errors.New("search: replay barrier must cover the snapshot partitions and start offsets")
	}
	replay := PublicReplayStatus{
		AppliedSequence: build.AppliedSequence,
		Checked:         make(PublicOffsets, len(start)),
		Verified:        make(PublicOffsets, len(start)),
		Gaps:            []PublicReplayGap{},
	}
	for _, stream := range publicEventStreams {
		topic := stream.topic
		partitions := start[topic]
		replay.Checked[topic] = make(map[int32]int64, len(partitions))
		replay.Verified[topic] = make(map[int32]int64, len(partitions))
		for partition := int32(0); int(partition) < len(partitions); partition++ {
			checked, gap, err := checkPublicPartition(ctx, tx, topic, partition, partitions[partition], barrier[topic][partition], build.AppliedSequence)
			if err != nil {
				return PublicReplayStatus{}, err
			}
			replay.Checked[topic][partition] = checked
			replay.Verified[topic][partition] = checked
			if gap.Reason != "" {
				replay.Verified[topic][partition] = gap.Offset
				replay.Gaps = append(replay.Gaps, gap)
			}
		}
		var extra PublicReplayGap
		err := tx.QueryRow(ctx, `SELECT kafka_partition, kafka_offset FROM sea_public_delivery
			WHERE topic = $1 AND kafka_partition >= $2 ORDER BY kafka_partition, kafka_offset LIMIT 1`,
			topic, len(partitions)).Scan(&extra.Partition, &extra.Offset)
		if err == nil {
			extra.Topic, extra.Reason = topic, "partition_without_replay_start"
			replay.Gaps = append(replay.Gaps, extra)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return PublicReplayStatus{}, err
		}
	}
	return replay, nil
}

func validPublicBarrier(start, barrier PublicOffsets) bool {
	if len(start) != len(barrier) {
		return false
	}
	for topic, partitions := range start {
		if len(barrier[topic]) != len(partitions) {
			return false
		}
		for partition, first := range partitions {
			end, present := barrier[topic][partition]
			if !present || end < first {
				return false
			}
		}
	}
	return true
}

func checkPublicPartition(ctx context.Context, tx pgx.Tx, topic string, partition int32, start, target, applied int64) (int64, PublicReplayGap, error) {
	var checked int64
	err := tx.QueryRow(ctx, `SELECT GREATEST($3, COALESCE((SELECT kafka_offset + 1 FROM sea_public_delivery
		WHERE topic = $1 AND kafka_partition = $2 ORDER BY kafka_offset DESC LIMIT 1), 0))`, topic, partition, target).Scan(&checked)
	if err != nil || checked == start {
		return checked, PublicReplayGap{}, err
	}
	// The sentinel exposes a missing tail as well as holes between deliveries.
	// The scan visits stored offsets only, even if the target is far ahead.
	gap := PublicReplayGap{Topic: topic, Partition: partition}
	err = tx.QueryRow(ctx, `WITH ordered AS (
			SELECT *, lag(kafka_offset, 1, $3::bigint - 1) OVER (ORDER BY kafka_offset) AS previous
			FROM (
				SELECT kafka_offset, outcome, journal_seq FROM sea_public_delivery
				WHERE topic = $1 AND kafka_partition = $2 AND kafka_offset >= $3 AND kafka_offset < $4
				UNION ALL SELECT $4::bigint, 'end', NULL::bigint
			) AS positions
		)
		SELECT CASE WHEN kafka_offset > previous + 1 THEN previous + 1 ELSE kafka_offset END,
			CASE WHEN kafka_offset > previous + 1 THEN 'missing'
				WHEN journal_seq > $5 THEN 'unapplied' ELSE outcome END
		FROM ordered WHERE kafka_offset > previous + 1
			OR outcome NOT IN ('journaled', 'verified_noop', 'end') OR journal_seq > $5
		ORDER BY kafka_offset LIMIT 1`, topic, partition, start, checked, applied).Scan(&gap.Offset, &gap.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return checked, gap, err
}
