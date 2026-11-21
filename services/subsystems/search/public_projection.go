package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
)

// PublicProjectionSource resolves owner data (including current eligibility) into
// ordered mutations. An empty result means an authoritative no-op. Consumption
// retries an error. The resolver supplies per-object ordering for merge members,
// corrections and withdrawals, even when their events lack such ordering.
type PublicProjectionSource interface {
	LoadPublicMutationPage(context.Context, outbox.Envelope, string) (PublicMutationPage, error)
}

// PublicMutationPage advances a single event in bounded, ordered owner pages.
type PublicMutationPage struct {
	Mutations []PublicMutation
	NextAfter string
}

type PublicProjector struct {
	db     *db.DB
	index  *PublicOpenSearch
	source PublicProjectionSource
}

func NewPublicProjector(database *db.DB, index *PublicOpenSearch, source PublicProjectionSource) (*PublicProjector, error) {
	if database == nil || index == nil || source == nil {
		return nil, errors.New("search: public projection requires its own database, OpenSearch and an owner source")
	}
	return &PublicProjector{db: database, index: index, source: source}, nil
}

type publicEventStream struct {
	topic, source string
	types         []string
	noops         []string
}

// These topics carry both projection inputs and contracted events unrelated
// to this index. Known no-ops are validated and recorded without their payload.
var publicEventStreams = []publicEventStream{
	{"public.evidence", "miaoqia/evidence", []string{
		"evidence.snapshot.created.v1", "evidence.extraction.revised.v1",
		"evidence.enrichment.completed.v1", "evidence.enricher.promoted.v1",
	}, []string{
		"evidence.probe.forwarded.v1", "evidence.quality.flagged.v1", "evidence.artifact.refetch_requested.v1",
	}},
	{"public.domain", "miaoqia/domain-intelligence", []string{"domain.fact.upserted.v1"}, []string{
		"domain.aggregation.changed.v1", "domain.coverage.gap_detected.v1",
	}},
	{"public.entity", "miaoqia/entity-graph", []string{
		"entity.entity.created.v1", "entity.entity.updated.v1", "entity.merge.applied.v1", "entity.merge.reverted.v1",
	}, []string{
		"entity.relationship.changed.v1", "entity.project_link.changed.v1",
	}},
	{"public.acquisition", "miaoqia/acquisition", []string{"acquisition.source.rights_changed.v1"}, []string{
		"acquisition.probe.emitted.v1", "acquisition.fetch.completed.v1", "acquisition.fetch.completed.v2",
		"acquisition.fetch.completed.v3", "acquisition.fetch.completed.v4", "acquisition.fetch.failed.v1",
		"acquisition.source.health_changed.v1",
	}},
}

// PublicEventTopics returns the topic allowlist shared by consumption and
// publish-time broker sampling.
func PublicEventTopics() []string {
	topics := make([]string, 0, len(publicEventStreams))
	for _, stream := range publicEventStreams {
		topics = append(topics, stream.topic)
	}
	return topics
}

// Consumers use the kernel's transactional inbox with one validated route
// per public topic. Each topic's ACL is on the producer trust boundary.
func PublicConsumers(database *db.DB, brokers []string) []*consumer.Consumer {
	consumers := make([]*consumer.Consumer, 0, len(publicEventStreams))
	for _, stream := range publicEventStreams {
		c := &consumer.Consumer{
			Name: "search.projection." + stream.topic, Subsystem: "search",
			Brokers: brokers, DB: database, Topics: []string{stream.topic},
			Handlers:        make(map[string]consumer.Handler, len(stream.types)+len(stream.noops)),
			ObserveDelivery: observePublicDelivery(stream),
		}
		for _, eventType := range stream.types {
			c.Handlers[eventType] = publicEventHandler(stream)
		}
		for _, eventType := range stream.noops {
			c.Handlers[eventType] = func(_ context.Context, _ pgx.Tx, event *outbox.Envelope) error {
				return validatePublicProducer(stream, event)
			}
		}
		consumers = append(consumers, c)
	}
	return consumers
}

func publicEventHandler(stream publicEventStream) consumer.Handler {
	return func(ctx context.Context, tx pgx.Tx, event *outbox.Envelope) error {
		if err := validatePublicProducer(stream, event); err != nil {
			return err
		}
		// Serialize append and checkpoint transactions so assigned sequence values
		// can never appear behind an already-advanced checkpoint after commit.
		if err := lockPublicProjection(ctx, tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO sea_public_event (event_id, envelope) VALUES ($1, $2) ON CONFLICT (event_id) DO NOTHING`, event.ID, event)
		return err
	}
}

func validatePublicProducer(stream publicEventStream, event *outbox.Envelope) error {
	if event.Source != stream.source || event.CheckTopic(stream.topic) != nil {
		return errors.New("search: unexpected public projection event scope or producer")
	}
	return nil
}

func lockPublicProjection(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(714423091892431::bigint)`)
	return err
}

// PublicOffsets maps a Kafka topic and partition to its next offset.
type PublicOffsets map[string]map[int32]int64

// PublicBuildBasis is the immutable owner snapshot and its replay start.
// AsOf is a conservative timestamp from the snapshot, independent of wall
// time at alias switch or per-event Kafka delivery time.
type PublicBuildBasis struct {
	SnapshotID  string        `json:"snapshot_id"`
	AsOf        time.Time     `json:"as_of"`
	ReplayStart PublicOffsets `json:"replay_start"`
}

// PublicBuildAssessment binds the external evaluation to the build and
// requests a replay barrier. Coverage is verified against the public journal.
type PublicBuildAssessment struct {
	Generation      string        `json:"generation"`
	SnapshotID      string        `json:"snapshot_id"`
	AppliedSequence int64         `json:"applied_sequence"`
	Barrier         PublicOffsets `json:"barrier"`
	Compared        bool          `json:"compared"`
}

// BeginBuild registers a fresh physical generation. Only the public migration
// set and the public database credentials can back this projector.
func (p *PublicProjector) BeginBuild(ctx context.Context, basis PublicBuildBasis) (string, error) {
	if !validPublicBasis(basis) {
		return "", errors.New("search: invalid public snapshot or replay start")
	}
	ctx, cancel := context.WithTimeout(ctx, publicQueryTimeout)
	defer cancel()
	generation := randomPublicID()
	if err := p.index.create(ctx, generation); err != nil {
		return "", err
	}
	// A failure here leaves an unpublished generation; fresh build IDs prevent
	// adoption of an orphan with unknown contents on retry.
	err := p.db.Tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO sea_public_generation (name, basis) VALUES ($1, $2)`, generation, basis)
		return err
	})
	if err != nil {
		return "", err
	}
	return generation, nil
}

func validPublicBasis(basis PublicBuildBasis) bool {
	if basis.SnapshotID == "" || basis.AsOf.UnixMilli() <= 0 || len(basis.ReplayStart) != len(publicEventStreams) {
		return false
	}
	for _, stream := range publicEventStreams {
		partitions := basis.ReplayStart[stream.topic]
		if len(partitions) == 0 {
			return false
		}
		for partition, offset := range partitions {
			if partition < 0 || int(partition) >= len(partitions) || offset < 0 {
				return false
			}
		}
	}
	return true
}

// LoadSnapshot applies a resumable batch. The owner must enumerate immutable
// snapshot data for this exact basis before setting final on the last batch.
func (p *PublicProjector) LoadSnapshot(ctx context.Context, generation, snapshotID string, mutations []PublicMutation, final bool) error {
	var finalDigest string
	if final {
		raw, err := json.Marshal(mutations)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		finalDigest = hex.EncodeToString(sum[:])
	}
	return p.withGeneration(ctx, generation, func(ctx context.Context, tx pgx.Tx, build publicBuild) error {
		if build.Basis.SnapshotID != snapshotID {
			return errors.New("search: generation does not accept snapshot batches")
		}
		if build.SnapshotFinished {
			if final && finalDigest == build.finalDigest {
				return nil
			}
			return errors.New("search: snapshot is already sealed")
		}
		if err := p.apply(ctx, generation, mutations); err != nil {
			return err
		}
		if !final {
			return nil
		}
		_, err := tx.Exec(ctx, `UPDATE sea_public_generation SET final_batch_digest=$2 WHERE name=$1`, generation, finalDigest)
		return err
	})
}

// ApplyNext advances one durable event after the snapshot. Run it for each live
// generation, including a retained rollback target. Failure leaves the event
// pending; partial external writes are safe to retry by business revision.
func (p *PublicProjector) ApplyNext(ctx context.Context, generation string) (bool, error) {
	var advanced bool
	err := p.withGeneration(ctx, generation, func(ctx context.Context, tx pgx.Tx, build publicBuild) error {
		if !build.SnapshotFinished {
			return errors.New("search: public snapshot has not finished")
		}
		var seq int64
		var event outbox.Envelope
		err := tx.QueryRow(ctx, `SELECT seq, envelope FROM sea_public_event WHERE seq > $1 ORDER BY seq LIMIT 1`, build.AppliedSequence).Scan(&seq, &event)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		page, err := p.source.LoadPublicMutationPage(ctx, event, build.pendingAfter)
		if err != nil {
			return err
		}
		if page.NextAfter != "" && (!continuedPublicEvent(event.Type) || page.NextAfter <= build.pendingAfter) {
			return errors.New("search: invalid owner projection continuation")
		}
		if err := p.apply(ctx, generation, page.Mutations); err != nil {
			return err
		}
		if page.NextAfter != "" {
			seq = build.AppliedSequence
		}
		_, err = tx.Exec(ctx, `UPDATE sea_public_generation SET applied_seq=$2, pending_after=$3 WHERE name=$1`,
			generation, seq, page.NextAfter)
		advanced = err == nil
		return err
	})
	return advanced && err == nil, err
}

func continuedPublicEvent(eventType string) bool {
	switch eventType {
	case "acquisition.source.rights_changed.v1", "entity.merge.applied.v1", "entity.merge.reverted.v1":
		return true
	default:
		return false
	}
}

// Publish activates a caught-up and evaluated generation, including a retained
// rollback target. brokerTip is sampled directly by the caller using the same
// Kafka cluster as the public journal consumer.
func (p *PublicProjector) Publish(ctx context.Context, generation, expectedActivation string, assessment PublicBuildAssessment, brokerTip PublicOffsets) (PublicPublication, error) {
	var next PublicPublication
	err := p.withGeneration(ctx, generation, func(ctx context.Context, tx pgx.Tx, build publicBuild) error {
		if !build.SnapshotFinished || assessment.Generation != generation ||
			assessment.AppliedSequence != build.AppliedSequence || !assessment.Compared || assessment.SnapshotID != build.Basis.SnapshotID {
			return errors.New("search: snapshot, replay coverage and comparison are required")
		}
		if !validPublicBarrier(build.Basis.ReplayStart, assessment.Barrier) || !validPublicBarrier(assessment.Barrier, brokerTip) {
			return errors.New("search: requested replay barrier must fit the snapshot and sampled Kafka offsets for every partition")
		}
		replay, err := checkPublicReplay(ctx, tx, build, brokerTip)
		if err != nil {
			return err
		}
		for _, gap := range replay.Gaps {
			return fmt.Errorf("search: public replay gap %s[%d] at offset %d: %s", gap.Topic, gap.Partition, gap.Offset, gap.Reason)
		}
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sea_public_event WHERE seq > $1)`, build.AppliedSequence).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return errors.New("search: public generation has pending events")
		}
		current, err := p.index.Current(ctx)
		if err != nil && !errors.Is(err, ErrPublicIndexUnpublished) {
			return err
		}
		if current.Activation != expectedActivation || current.Generation == generation {
			return errors.New("search: unexpected current public activation")
		}
		millis := build.Basis.AsOf.UnixMilli()
		next = PublicPublication{
			Generation: generation,
			Activation: randomPublicID() + ":" + strconv.FormatInt(millis, 10),
			IndexedAt:  time.UnixMilli(millis).UTC(),
		}
		return p.index.switchAlias(ctx, current, next)
	})
	if err != nil {
		// A lost alias acknowledgment is resolved by re-reading Current.
		return PublicPublication{}, err
	}
	return next, nil
}

// PublicBuildStatus identifies the durable sequence to which comparison
// evidence must be bound.
type PublicBuildStatus struct {
	Basis            PublicBuildBasis `json:"basis"`
	SnapshotFinished bool             `json:"snapshot_finished"`
	AppliedSequence  int64            `json:"applied_sequence"`
}

// BuildStatus reports the snapshot and durable projection checkpoint.
func (p *PublicProjector) BuildStatus(ctx context.Context, generation string) (PublicBuildStatus, error) {
	var status PublicBuildStatus
	err := p.withGeneration(ctx, generation, func(_ context.Context, _ pgx.Tx, build publicBuild) error {
		status = build.PublicBuildStatus
		return nil
	})
	return status, err
}

func (p *PublicProjector) CurrentPublication(ctx context.Context) (PublicPublication, error) {
	return p.index.Current(ctx)
}

type publicBuild struct {
	PublicBuildStatus
	finalDigest  string
	pendingAfter string
}

func (p *PublicProjector) withGeneration(ctx context.Context, name string, fn func(context.Context, pgx.Tx, publicBuild) error) error {
	if !generationID.MatchString(name) {
		return errors.New("search: invalid public generation")
	}
	ctx, cancel := context.WithTimeout(ctx, publicQueryTimeout)
	defer cancel()
	return p.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockPublicProjection(ctx, tx); err != nil {
			return err
		}
		var build publicBuild
		if err := tx.QueryRow(ctx, `SELECT basis, final_batch_digest, applied_seq, pending_after FROM sea_public_generation WHERE name=$1`, name).
			Scan(&build.Basis, &build.finalDigest, &build.AppliedSequence, &build.pendingAfter); err != nil {
			return fmt.Errorf("search: read public generation: %w", err)
		}
		build.SnapshotFinished = build.finalDigest != ""
		return fn(ctx, tx, build)
	})
}

func (p *PublicProjector) apply(ctx context.Context, generation string, mutations []PublicMutation) error {
	for _, mutation := range mutations {
		if err := p.index.apply(ctx, generation, mutation); err != nil {
			return err
		}
	}
	if len(mutations) != 0 {
		// Also refresh version-conflict retries that follow a lost write
		// acknowledgment. The journal checkpoint follows search visibility.
		return p.index.refresh(ctx, generation)
	}
	return nil
}
