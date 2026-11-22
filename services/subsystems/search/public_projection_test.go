package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/superailabs/miaoqia/services/contracts/events"
	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/search/api"
	"github.com/superailabs/miaoqia/services/subsystems/search/publicmigrations"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

type publicProjectionFunc func(context.Context, outbox.Envelope) ([]PublicMutation, error)

func (f publicProjectionFunc) LoadPublicMutationPage(ctx context.Context, envelope outbox.Envelope, _ string) (PublicMutationPage, error) {
	mutations, err := f(ctx, envelope)
	return PublicMutationPage{Mutations: mutations}, err
}

func publicProjectionDB(t *testing.T, dsn string) *db.DB {
	t.Helper()
	ctx := t.Context()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{"search_" + randomPublicID()}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		if err != nil {
			t.Error(err)
		}
		admin.Close(context.Background())
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	params := u.Query()
	params.Set("search_path", schema)
	u.RawQuery = params.Encode()
	if err := db.Migrate(ctx, u.String(), "", publicmigrations.FS); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	return database
}

func TestPublicNoticeArchiveEventsAreNoOps(t *testing.T) {
	var acquisition *consumer.Consumer
	for _, c := range PublicConsumers(nil, nil) {
		if slices.Equal(c.Topics, []string{"public.acquisition"}) {
			acquisition = c
			break
		}
	}
	if acquisition == nil {
		t.Fatal("missing public acquisition consumer")
	}
	if err := acquisition.Prepare(); err != nil {
		t.Fatalf("prepare public acquisition consumer: %v", err)
	}
	for _, eventType := range []string{
		"acquisition.notice.archived.v1",
		"acquisition.grouped_notice.archived.v1",
		"acquisition.browser_notice.archived.v1",
	} {
		t.Run(eventType, func(t *testing.T) {
			handler := acquisition.Handlers[eventType]
			if handler == nil {
				t.Fatal("missing notice archive handler")
			}
			event := outbox.Envelope{
				SpecVersion: "1.0", ID: ids.New("evt"), Source: "miaoqia/acquisition", Type: eventType,
				Time: time.Now(), Zone: reqctx.ZonePublic, DataContentType: "application/json", Data: json.RawMessage(`{}`),
			}
			// Payload schema validation precedes dispatch. A no-op needs no transaction.
			if err := handler(t.Context(), nil, &event); err != nil {
				t.Fatalf("notice archive no-op failed: %v", err)
			}
			wrongProducer := event
			wrongProducer.Source = "miaoqia/evidence"
			if err := handler(t.Context(), nil, &wrongProducer); err == nil {
				t.Fatal("notice archive accepted a mismatched producer")
			}
			event.Zone, event.Tenant, event.Cell = reqctx.ZoneTenant, "ta", "cell-01"
			if err := handler(t.Context(), nil, &event); err == nil {
				t.Fatal("notice archive accepted a tenant event")
			}
		})
	}
}

func TestPublicProjectionPostgresAndOpenSearch(t *testing.T) {
	dsn := os.Getenv("MIAOQIA_TEST_PG")
	endpoint := os.Getenv("MIAOQIA_TEST_OPENSEARCH")
	if dsn == "" || endpoint == "" {
		t.Skip("MIAOQIA_TEST_PG and MIAOQIA_TEST_OPENSEARCH must name local integration services")
	}
	database := publicProjectionDB(t, dsn)
	index, err := NewPublicOpenSearch(endpoint, "test-"+randomPublicID(), &http.Client{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := reqctx.Service(t.Context(), "search")
	published := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	factID := ids.New("ntc")
	project := func(revision int64, withdraw bool) PublicMutation {
		return PublicMutation{
			Ref:      api.PublicRef{Kind: api.PublicFact, Domain: "procurement", ID: factID, Version: strconv.FormatInt(revision, 10)},
			Revision: revision, Withdrawn: withdraw,
			Title: "安徽建设中标公告", Text: "只用于合成测试的公告内容", Identifiers: []string{"AH-TEST-01"},
			Region: "340000", NoticeType: "award", PublishedAt: &published,
		}
	}
	resolveFailure := errors.New("synthetic owner outage")
	ownerUnavailable := false
	source := publicProjectionFunc(func(_ context.Context, event outbox.Envelope) ([]PublicMutation, error) {
		if ownerUnavailable {
			return nil, resolveFailure
		}
		var change struct {
			Version int64 `json:"fact_version"`
		}
		if err := json.Unmarshal(event.Data, &change); err != nil {
			return nil, err
		}
		return []PublicMutation{project(change.Version, change.Version == 5)}, nil
	})
	projector, err := NewPublicProjector(database, index, source)
	if err != nil {
		t.Fatal(err)
	}
	const eventType = "domain.fact.upserted.v1"
	broker := os.Getenv("MIAOQIA_TEST_PUBLIC_SEARCH_KAFKA")
	consumers := PublicConsumers(database, []string{broker})
	start := PublicOffsets{}
	var domainConsumer *consumer.Consumer
	for _, consumer := range consumers {
		if err := consumer.Prepare(); err != nil {
			t.Fatal(err)
		}
		if len(consumer.Topics) != 1 {
			t.Fatal("public event handlers must only receive their owner topic")
		}
		topic := consumer.Topics[0]
		start[topic] = map[int32]int64{0: 0}
		if eventHandler := consumer.Handlers[eventType]; eventHandler != nil {
			if topic != "public.domain" {
				t.Fatal("domain handler accepts another public producer's topic")
			}
			domainConsumer = consumer
		}
	}
	if domainConsumer == nil {
		t.Fatal("missing domain projection handler")
	}
	if broker != "" {
		start["public.domain"][1] = 0
	}
	handler := domainConsumer.Handlers[eventType]
	schema, err := events.Schema(eventType)
	if err != nil {
		t.Fatal(err)
	}
	envelope := func(version int64) outbox.Envelope {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"info_domain": "INF-PROC", "fact_id": factID, "changed_fields": []string{"/title"},
			"rights_tags": []string{"synthetic"}, "snapshot_ref": ids.New("snp"), "revision_ref": ids.New("exr"),
			"object_type": "fixture", "fact_version": version, "schema_version": "1.0.0",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := events.Validate(schema, payload); err != nil {
			t.Fatal(err)
		}
		return outbox.Envelope{
			SpecVersion: "1.0", ID: ids.New("evt"), Source: "miaoqia/domain-intelligence", Subject: factID, Type: eventType,
			Time: published, Zone: reqctx.ZonePublic, DataContentType: "application/json", Data: payload,
		}
	}
	appendEvent := func(event outbox.Envelope) error {
		return database.Tx(ctx, func(tx pgx.Tx) error { return handler(ctx, tx, &event) })
	}
	wrongZone := envelope(3)
	wrongZone.Zone, wrongZone.Tenant, wrongZone.Cell = reqctx.ZoneTenant, "ta", "cell-01"
	if err := appendEvent(wrongZone); err == nil {
		t.Fatal("tenant event was written to the public journal")
	}
	wrongProducer := envelope(3)
	wrongProducer.Source = "miaoqia/acquisition"
	if err := appendEvent(wrongProducer); err == nil {
		t.Fatal("a mismatched public producer was admitted")
	}

	basis := PublicBuildBasis{SnapshotID: "synthetic-snapshot", AsOf: published, ReplayStart: start}
	barrier := make(PublicOffsets, len(start))
	for topic, offsets := range start {
		barrier[topic] = offsets
	}
	snapshot := []PublicMutation{project(3, false)}
	for _, kind := range []api.PublicKind{api.PublicNotice, api.PublicChunk, api.PublicEntity} {
		item := project(1, false)
		item.Ref.Kind = kind
		item.Ref.ID = "synthetic-" + string(kind)
		snapshot = append(snapshot, item)
	}
	build := func() string {
		t.Helper()
		generation, err := projector.BeginBuild(ctx, basis)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := index.call(context.Background(), http.MethodDelete, "/"+index.prefix+generation, nil, nil, nil); err != nil {
				t.Error(err)
			}
		})
		if err := projector.LoadSnapshot(ctx, generation, basis.SnapshotID, snapshot, true); err != nil {
			t.Fatal(err)
		}
		if err := projector.LoadSnapshot(ctx, generation, basis.SnapshotID, snapshot, true); err != nil {
			t.Fatalf("retry of sealed snapshot: %v", err)
		}
		return generation
	}
	assessment := func(generation string) PublicBuildAssessment {
		t.Helper()
		status, err := projector.BuildStatus(ctx, generation)
		if err != nil {
			t.Fatal(err)
		}
		return PublicBuildAssessment{
			Generation: generation, SnapshotID: basis.SnapshotID, AppliedSequence: status.AppliedSequence,
			Barrier: barrier, Compared: true,
		}
	}
	first := build()
	current, err := projector.Publish(ctx, first, "", assessment(first), barrier)
	if err != nil {
		t.Fatal(err)
	}

	// The authority fixture returns a different value from the indexed text.
	authority := publicAuthorityFunc(func(_ context.Context, _ api.PublicQuery, refs []api.PublicRef) ([]api.PublicHit, error) {
		hits := make([]api.PublicHit, 0, len(refs))
		for _, ref := range refs {
			hits = append(hits, api.PublicHit{Ref: ref, Excerpt: "合成权威摘录"})
		}
		return hits, nil
	})
	searcher, err := NewPublicSearcher(index, authority, bytes.Repeat([]byte{6}, 32))
	if err != nil {
		t.Fatal(err)
	}
	user := reqctx.With(ctx, publicUser())
	parameters := api.PublicSearchRequest{PageSize: 8, Query: api.PublicQuery{
		Text: "建设中标", Identifier: "AH-TEST-01", Region: "340000", NoticeType: "award", From: &published,
	}}
	query := func(request api.PublicSearchRequest) api.PublicSearchPage {
		t.Helper()
		page, err := searcher.Search(user, request)
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	all := query(parameters)
	if len(all.Hits) != 4 || !all.IndexedAt.Equal(published) {
		t.Fatalf("query didn't discover all indexed public families: %+v", all)
	}
	for _, hit := range all.Hits {
		if hit.Excerpt != "合成权威摘录" || hit.Text != "" {
			t.Fatalf("non-authority display content: %+v", hit)
		}
	}
	pageSizeOne := parameters
	pageSizeOne.PageSize = 1
	pageOne := query(pageSizeOne)
	pageSizeOne.Cursor = pageOne.NextCursor
	pageTwo := query(pageSizeOne)
	if pageSizeOne.Cursor == "" || len(pageTwo.Hits) != 1 || pageOne.Hits[0].Ref == pageTwo.Hits[0].Ref {
		t.Fatalf("real PIT did not advance: %+v %+v", pageOne, pageTwo)
	}
	wrongIdentifier := parameters
	wrongIdentifier.Query.Identifier = "AH-TEST"
	if page := query(wrongIdentifier); len(page.Hits) != 0 {
		t.Fatalf("nonexact identifier returned a public candidate: %+v", page)
	}
	parameters.Query.Kind = api.PublicFact

	shadow := build()
	update := envelope(4)
	events := []outbox.Envelope{update, update, envelope(2), envelope(4), envelope(5), envelope(3)}
	var kafka *publicKafkaTest
	var retryAttempts, failedAttempts atomic.Int32
	failed := envelope(6)
	if broker == "" {
		for _, event := range events {
			if err := appendEvent(event); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		noop := envelope(4)
		noop.Type = "domain.coverage.gap_detected.v1"
		noop.Data = json.RawMessage(`{"info_domain":"INF-PROC","source_id":"synthetic-src","reason_code":"fixture","query":"synthetic"}`)
		domainConsumer.MaxAttempts = 2
		domainConsumer.Backoff = time.Millisecond
		observe := domainConsumer.ObserveDelivery
		domainConsumer.ObserveDelivery = func(ctx context.Context, tx pgx.Tx, delivery consumer.Delivery, outcome consumer.DeliveryOutcome, event *outbox.Envelope) error {
			if err := observe(ctx, tx, delivery, outcome, event); err != nil || event == nil {
				return err
			}
			if event.ID == failed.ID {
				failedAttempts.Add(1)
				return errors.New("synthetic rollback after recording delivery evidence")
			}
			if event.ID == update.ID && delivery.Partition == 0 && delivery.Offset == 0 && retryAttempts.Add(1) == 1 {
				return errors.New("synthetic transient rollback after recording delivery evidence")
			}
			return nil
		}
		kafka = startPublicKafka(t, domainConsumer)
		barrier["public.domain"] = map[int32]int64{0: kafka.produce(t, 0, events...), 1: kafka.produce(t, 1, update, noop)}
		kafka.waitCommits(t, barrier["public.domain"])
		if retryAttempts.Load() != 2 {
			t.Fatalf("delivery was attempted %d times, want 2", retryAttempts.Load())
		}
		var noops int
		if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM sea_public_delivery
			WHERE topic='public.domain' AND kafka_partition=1 AND kafka_offset=1 AND outcome='verified_noop' AND journal_seq IS NULL`).Scan(&noops); err != nil || noops != 1 {
			t.Fatalf("known no-op has no verified receipt: %d %v", noops, err)
		}
		replay, err := projector.ReplayStatus(ctx, shadow, barrier)
		if err != nil || len(replay.Gaps) != 2 || replay.Gaps[0].Reason != "unapplied" || replay.Gaps[1].Reason != "unapplied" {
			t.Fatalf("Kafka receipts got credit before projection: %+v %v", replay, err)
		}
	}
	if _, err := projector.Publish(ctx, shadow, current.Activation, assessment(shadow), barrier); err == nil {
		t.Fatal("published a generation with pending events")
	}
	ownerUnavailable = true
	if ok, err := projector.ApplyNext(ctx, first); ok || !errors.Is(err, resolveFailure) {
		t.Fatalf("owner failure advanced durable work: %v %v", ok, err)
	}
	ownerUnavailable = false
	if ok, err := projector.ApplyNext(ctx, first); err != nil || !ok {
		t.Fatalf("first event: %v %v", ok, err)
	}
	if page := query(parameters); len(page.Hits) != 1 || page.Hits[0].Ref.Version != "4" {
		t.Fatalf("new owner version didn't replace prior content: %+v", page)
	}
	projector, err = NewPublicProjector(database, index, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, generation := range []string{first, shadow} {
		want := 5
		if generation == first {
			want--
		}
		count := 0
		for {
			advanced, err := projector.ApplyNext(ctx, generation)
			if err != nil {
				t.Fatal(err)
			}
			if !advanced {
				break
			}
			count++
		}
		if count != want {
			t.Fatalf("journal applied %d events, want %d", count, want)
		}
	}
	if page := query(parameters); len(page.Hits) != 0 {
		t.Fatalf("late update resurrected a withdrawn record: %+v", page)
	}
	conflict := project(5, false)
	if err := index.apply(ctx, first, conflict); err == nil {
		t.Fatal("conflicting equal revision was accepted")
	}
	insufficient := assessment(shadow)
	insufficient.Barrier = PublicOffsets{}
	if _, err := projector.Publish(ctx, shadow, current.Activation, insufficient, barrier); err == nil {
		t.Fatal("published a generation without successful replay evidence")
	}
	shadowActivation, err := projector.Publish(ctx, shadow, current.Activation, assessment(shadow), barrier)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := searcher.Search(user, pageSizeOne); !errors.Is(err, api.ErrPublicCursor) {
		t.Fatalf("cursor survived real alias switch: %v", err)
	}
	rolledBack, err := projector.Publish(ctx, first, shadowActivation.Activation, assessment(first), barrier)
	if err != nil || rolledBack.Activation == current.Activation || rolledBack.Generation != first {
		t.Fatalf("roll back: %+v %v", rolledBack, err)
	}
	if _, err := searcher.Search(user, pageSizeOne); !errors.Is(err, api.ErrPublicCursor) {
		t.Fatalf("cursor survived real alias rollback: %v", err)
	}
	if page := query(parameters); len(page.Hits) != 0 {
		t.Fatalf("rollback resurrected a withdrawn record: %+v", page)
	}
	if kafka != nil {
		verifyPublicReplayGaps(t, ctx, projector, database, kafka, shadow, rolledBack, assessment(shadow), update, failed)
		if failedAttempts.Load() != 2 {
			t.Fatalf("failed delivery was attempted %d times, want 2", failedAttempts.Load())
		}
	}
}

func verifyPublicReplayGaps(t *testing.T, ctx context.Context, projector *PublicProjector, database *db.DB, kafka *publicKafkaTest, generation string, current PublicPublication, assessment PublicBuildAssessment, update, failed outbox.Envelope) {
	t.Helper()
	topic := kafka.consumer.Topics[0]
	target := make(PublicOffsets, len(assessment.Barrier))
	for topic, partitions := range assessment.Barrier {
		target[topic] = partitions
	}
	ends := assessment.Barrier[topic]
	target[topic] = map[int32]int64{0: ends[0], 1: ends[1] + 1}
	replay, err := projector.ReplayStatus(ctx, generation, target)
	if err != nil || len(replay.Gaps) != 1 || replay.Gaps[0] != (PublicReplayGap{topic, 1, ends[1], "missing"}) {
		t.Fatalf("unseen Kafka offset got credit: %+v %v", replay, err)
	}
	if _, err := projector.Publish(ctx, generation, current.Activation, PublicBuildAssessment{
		Generation: assessment.Generation, SnapshotID: assessment.SnapshotID, AppliedSequence: assessment.AppliedSequence,
		Compared: true, Barrier: assessment.Barrier,
	}, target); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("published with an unseen offset: %v", err)
	}

	// Subsequent offsets are real duplicate deliveries. A group commit passes
	// both failures, but neither adjacent receipts nor retries can fill them.
	next := map[int32]int64{0: ends[0], 1: kafka.produce(t, 1, failed, update)}
	kafka.waitCommits(t, next)
	if _, err := projector.Publish(ctx, generation, current.Activation, assessment, assessment.Barrier); err == nil || !strings.Contains(err.Error(), "dead_lettered") {
		t.Fatalf("published over a committed dead letter: %v", err)
	}
	var events, inbox, proof int
	err = database.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM sea_public_event WHERE event_id=$1),
		(SELECT count(*) FROM kernel_inbox WHERE event_id=$1),
		(SELECT count(*) FROM sea_public_delivery WHERE topic=$2 AND kafka_partition=1 AND kafka_offset=$3 AND journal_seq IS NOT NULL)`,
		failed.ID, topic, ends[1]).Scan(&events, &inbox, &proof)
	if err != nil || events != 0 || inbox != 0 || proof != 0 {
		t.Fatalf("failed transaction left a journal event, inbox or success receipt: %d %d %d %v", events, inbox, proof, err)
	}
	skipped := update
	skipped.ID, skipped.Type = ids.New("evt"), "domain.fact.unregistered.v1"
	next[0] = kafka.produce(t, 0, skipped, update)
	kafka.waitCommits(t, next)
	want := []PublicReplayGap{{topic, 0, ends[0], "skipped"}, {topic, 1, ends[1], "dead_lettered"}}
	target[topic] = next
	for _, barrier := range []PublicOffsets{target, assessment.Barrier} {
		replay, err := projector.ReplayStatus(ctx, generation, barrier)
		if err != nil || !slices.Equal(replay.Gaps, want) || !maps.Equal(replay.Checked[topic], next) || !maps.Equal(replay.Verified[topic], ends) {
			t.Fatalf("did not expose gaps behind Kafka group commits: %+v %v", replay, err)
		}
		assessment.Barrier = barrier
		if _, err := projector.Publish(ctx, generation, current.Activation, assessment, barrier); err == nil || !strings.Contains(err.Error(), "skipped") {
			t.Fatalf("published over committed gaps: %v", err)
		}
	}
	publication, err := projector.CurrentPublication(ctx)
	if err != nil || publication != current {
		t.Fatalf("failed publication altered the alias: %+v %v", publication, err)
	}
}

type publicKafkaTest struct {
	ctx      context.Context
	consumer *consumer.Consumer
	client   *kgo.Client
	admin    *kadm.Client
	result   chan error
}

// The canonical topics must be absent. Use a fresh disposable broker.
func startPublicKafka(t *testing.T, c *consumer.Consumer) *publicKafkaTest {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	t.Cleanup(cancel)
	client, err := kgo.NewClient(kgo.SeedBrokers(c.Brokers...), kgo.RecordPartitioner(kgo.ManualPartitioner()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	admin := kadm.NewClient(client)
	if _, err := admin.CreateTopic(ctx, 2, 1, nil, c.Topics[0]); err != nil {
		t.Fatalf("create isolated Kafka topic: %v", err)
	}
	c.DeadLetter = "dlq.search"
	if _, err := admin.CreateTopic(ctx, 1, 1, nil, c.DeadLetter); err != nil {
		t.Fatalf("create isolated Kafka dead-letter topic: %v", err)
	}
	f := &publicKafkaTest{ctx: ctx, consumer: c, client: client, admin: admin, result: make(chan error, 1)}
	go func() {
		defer close(f.result)
		f.result <- c.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		if err, ok := <-f.result; ok && !errors.Is(err, context.Canceled) {
			t.Errorf("stop public Kafka consumer: %v", err)
		}
	})
	return f
}

func (f *publicKafkaTest) produce(t *testing.T, partition int32, events ...outbox.Envelope) int64 {
	t.Helper()
	records := make([]*kgo.Record, 0, len(events))
	for _, event := range events {
		value, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, &kgo.Record{Topic: f.consumer.Topics[0], Partition: partition, Key: []byte(event.Subject), Value: value})
	}
	if err := f.client.ProduceSync(f.ctx, records...).FirstErr(); err != nil {
		t.Fatal(err)
	}
	return records[len(records)-1].Offset + 1
}

func (f *publicKafkaTest) waitCommits(t *testing.T, expected map[int32]int64) {
	t.Helper()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		if offsets, err := f.admin.FetchOffsets(f.ctx, f.consumer.Name); err == nil {
			actual := make(map[int32]int64, len(expected))
			for partition := range expected {
				if offset, ok := offsets.Lookup(f.consumer.Topics[0], partition); ok && offset.Err == nil {
					actual[partition] = offset.At
				}
			}
			if maps.Equal(actual, expected) {
				t.Logf("Kafka %s committed next offsets %v", f.consumer.Topics[0], expected)
				return
			}
		}
		select {
		case <-f.ctx.Done():
			t.Fatalf("Kafka offsets %v were not committed: %v", expected, f.ctx.Err())
		case err := <-f.result:
			t.Fatalf("public Kafka consumer stopped before committing: %v", err)
		case <-poll.C:
		}
	}
}
