package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/superailabs/miaoqia/services/contracts/events"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/search"
	searchapi "github.com/superailabs/miaoqia/services/subsystems/search/api"
	"github.com/superailabs/miaoqia/services/subsystems/search/migrations"
	"github.com/superailabs/miaoqia/services/subsystems/search/publicmigrations"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

type publicTestAuthority struct{}

func (publicTestAuthority) AuthorizePublicSearch(_ context.Context, _ searchapi.PublicQuery, refs []searchapi.PublicRef) ([]searchapi.PublicHit, error) {
	hits := make([]searchapi.PublicHit, 0, len(refs))
	for _, ref := range refs {
		hits = append(hits, searchapi.PublicHit{Ref: ref})
	}
	return hits, nil
}

func TestPublicProjectionAgainstPythonOwner(t *testing.T) {
	ownerOrigin := os.Getenv("MIAOQIA_TEST_SEARCH_OWNER_URL")
	if ownerOrigin == "" {
		t.Skip("run via the Python owner fixture with MIAOQIA_TEST_OPENSEARCH")
	}
	owner, err := newPublicProjectionSource(ownerOrigin)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(os.Getenv("MIAOQIA_TEST_SEARCH_SCENARIO"))
	if err != nil {
		t.Fatal(err)
	}
	var scenario struct {
		SnapshotID  string               `json:"snapshot_id"`
		AsOf        time.Time            `json:"as_of"`
		Pages       []json.RawMessage    `json:"pages"`
		Withdrawal  outbox.Envelope      `json:"withdrawal"`
		ReplayStart search.PublicOffsets `json:"replay_start"`
	}
	if err := decodeStrictJSON(bytes.NewReader(raw), &scenario); err != nil {
		t.Fatal(err)
	}
	ctx := reqctx.Service(t.Context(), publicSearchWorker)
	if err := db.Migrate(ctx, os.Getenv("MIAOQIA_TEST_SEARCH_DB_OWNER"), os.Getenv("MIAOQIA_TEST_SEARCH_DB_APP_ROLE"), publicmigrations.FS); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(ctx, os.Getenv("MIAOQIA_TEST_SEARCH_DB_APP"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	namespace := "owner-" + strings.ToLower(ids.New("evt")[4:])
	indexURL := os.Getenv("MIAOQIA_TEST_OPENSEARCH")
	indexClient := &http.Client{Timeout: 5 * time.Second}
	index, err := search.NewPublicOpenSearch(indexURL, namespace, indexClient)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := search.NewPublicProjector(database, index, owner)
	if err != nil {
		t.Fatal(err)
	}
	offsets := scenario.ReplayStart
	broker := os.Getenv("MIAOQIA_TEST_SEARCH_OWNER_KAFKA")
	if broker == "" {
		offsets = search.PublicOffsets{}
		for _, topic := range search.PublicEventTopics() {
			offsets[topic] = map[int32]int64{0: 0}
		}
	}
	snapshotID := scenario.SnapshotID
	var output bytes.Buffer
	basis, err := json.Marshal(search.PublicBuildBasis{SnapshotID: snapshotID, AsOf: scenario.AsOf, ReplayStart: offsets})
	if err != nil {
		t.Fatal(err)
	}
	runBuild := func(args []string, input []byte) {
		t.Helper()
		sample := func(ctx context.Context) (search.PublicOffsets, error) {
			if broker != "" {
				return samplePublicSearchTip(ctx, []string{broker})
			}
			return offsets, nil
		}
		if err := executeSearchBuild(ctx, projector, args, bytes.NewReader(input), &output, sample); err != nil {
			t.Fatal(err)
		}
	}
	runBuild([]string{"begin"}, basis)
	var started map[string]string
	if err := json.Unmarshal(output.Bytes(), &started); err != nil || started["generation"] == "" {
		t.Fatalf("invalid generation: %s %v", output.Bytes(), err)
	}
	generation := started["generation"]
	t.Cleanup(func() {
		req, err := http.NewRequest(http.MethodDelete, indexURL+"/mq-public-"+namespace+"-"+generation, nil)
		if err != nil {
			t.Error(err)
			return
		}
		resp, err := indexClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
	})
	if len(scenario.Pages) != 2 {
		t.Fatalf("expected one notice and one project page, got %d", len(scenario.Pages))
	}
	runBuild([]string{"load", generation, snapshotID}, scenario.Pages[0])
	runBuild([]string{"seal", generation, snapshotID}, scenario.Pages[1])
	assessment, err := json.Marshal(search.PublicBuildAssessment{Generation: generation, SnapshotID: snapshotID, Barrier: offsets, Compared: true})
	if err != nil {
		t.Fatal(err)
	}
	runBuild([]string{"publish", generation, ""}, assessment)
	noticePage, err := decodePublicMutationPage(bytes.NewReader(scenario.Pages[0]), "")
	if err != nil || len(noticePage.Mutations) < 2 || noticePage.Mutations[0].Ref.Kind != searchapi.PublicNotice ||
		noticePage.Mutations[1].Ref.Kind != searchapi.PublicChunk {
		t.Fatalf("invalid notice and chunk page: %v", err)
	}
	projectPage, err := decodePublicMutationPage(bytes.NewReader(scenario.Pages[1]), "")
	if err != nil || len(projectPage.Mutations) != 1 || projectPage.Mutations[0].Ref.Kind != searchapi.PublicFact {
		t.Fatalf("invalid project page: %v", err)
	}
	searcher, err := search.NewPublicSearcher(index, publicTestAuthority{}, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	user := reqctx.With(t.Context(), reqctx.Context{
		Tenant: testPublicTenant, Cell: "cell-01", Principal: reqctx.Principal{ID: testPublicSubject, Kind: "user"}, Purpose: "interactive",
	})
	read := func(ref searchapi.PublicRef) []searchapi.PublicHit {
		t.Helper()
		page, err := searcher.Search(user, searchapi.PublicSearchRequest{PageSize: 2, Query: searchapi.PublicQuery{Identifier: ref.ID, Kind: ref.Kind}})
		if err != nil {
			t.Fatal(err)
		}
		return page.Hits
	}
	for _, mutation := range append(slices.Clone(noticePage.Mutations), projectPage.Mutations...) {
		ref := mutation.Ref
		if hits := read(ref); len(hits) != 1 || hits[0].Ref != ref {
			t.Fatalf("owner snapshot did not reach search: %v", hits)
		}
	}
	event := scenario.Withdrawal
	for _, consumer := range search.PublicConsumers(database, []string{broker}) {
		if handler := consumer.Handlers[event.Type]; handler != nil {
			if broker != "" {
				consumeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
				done := make(chan error, 1)
				go func() { done <- consumer.Run(consumeCtx) }()
				defer func() {
					cancel()
					if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
				}()
				producePublicOwnerEvent(t, consumeCtx, broker, consumer.Topics[0], event, database)
				break
			}
			if err := database.Tx(ctx, func(tx pgx.Tx) error { return handler(ctx, tx, &event) }); err != nil {
				t.Fatal(err)
			}
		}
	}
	if advanced, err := projector.ApplyNext(ctx, generation); err != nil || !advanced {
		t.Fatalf("owner withdrawal did not advance: %t %v", advanced, err)
	}
	status, err := projector.BuildStatus(ctx, generation)
	project := projectPage.Mutations[0].Ref
	if err != nil || status.AppliedSequence != 0 || len(read(project)) != 1 {
		t.Fatalf("first page did not preserve the event checkpoint: %+v %v", status, err)
	}
	for _, mutation := range noticePage.Mutations {
		if hits := read(mutation.Ref); len(hits) != 0 {
			t.Fatalf("withdrawn notice or chunk is still discoverable: %v", hits)
		}
	}
	// Reopen the worker to prove that the owner continuation is durable.
	projector, err = search.NewPublicProjector(database, index, owner)
	if err != nil {
		t.Fatal(err)
	}
	if advanced, err := projector.ApplyNext(ctx, generation); err != nil || !advanced {
		t.Fatalf("owner withdrawal continuation did not advance: %t %v", advanced, err)
	}
	status, err = projector.BuildStatus(ctx, generation)
	if err != nil || status.AppliedSequence < 1 || len(read(project)) != 0 {
		t.Fatalf("final page did not commit the event checkpoint: %+v %v", status, err)
	}
	if broker != "" {
		tip, err := samplePublicSearchTip(ctx, []string{broker})
		if err != nil {
			t.Fatal(err)
		}
		replay, err := projector.ReplayStatus(ctx, generation, tip)
		if err != nil || len(replay.Gaps) != 0 || replay.Verified["public.acquisition"][0] != offsets["public.acquisition"][0]+1 {
			t.Fatalf("owner replay did not certify the actual Kafka withdrawal: %+v %v", replay, err)
		}
	}
}

func producePublicOwnerEvent(t *testing.T, ctx context.Context, broker, topic string, event outbox.Envelope, database *db.DB) {
	t.Helper()
	client, err := kgo.NewClient(kgo.SeedBrokers(broker))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ProduceSync(ctx, &kgo.Record{Topic: topic, Key: []byte(event.Subject), Value: body}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		var recorded bool
		err := database.Tx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM sea_public_event WHERE event_id=$1)", event.ID).Scan(&recorded)
		})
		if recorded && err == nil {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	t.Fatalf("consumer did not journal owner event %s", event.ID)
}

func TestPublicSearchRejectsCellCredentials(t *testing.T) {
	configureSearchTest(t)
	cellOwnerDSN := "postgres://sea_owner@127.0.0.1:1/sea?sslmode=disable"
	cellAppDSN := os.Getenv("MIAOQIA_SEARCH_DB_DSN")
	t.Setenv("MIAOQIA_SEARCH_DB_OWNER_DSN", cellOwnerDSN)
	t.Setenv(publicSearchPrefix+"DB_OWNER_DSN", "")
	t.Setenv(publicSearchPrefix+"DB_DSN", "")
	if err := migrate(context.Background(), testSearchLog, publicSearchWorker); err == nil ||
		!strings.Contains(err.Error(), publicSearchPrefix+"DB_OWNER_DSN") {
		t.Fatalf("migration did not require a public owner DSN: %v", err)
	}
	t.Setenv(publicSearchPrefix+"DB_OWNER_DSN", cellOwnerDSN)
	if err := migrate(context.Background(), testSearchLog, publicSearchWorker); err == nil ||
		!strings.Contains(err.Error(), "requires database sea_public and role sea_public_owner") {
		t.Fatalf("migration did not reject a Cell owner DSN: %v", err)
	}
	t.Setenv(publicSearchPrefix+"DB_DSN", cellAppDSN)
	if _, err := publicSearchDSN("DB_DSN", publicSearchApp); err == nil {
		t.Fatal("public application resolved a Cell DSN")
	}
}

func TestPublicSearchProjectionWithPostgres(t *testing.T) {
	appDSN := os.Getenv("MIAOQIA_TEST_PUBLIC_SEARCH_PG_APP")
	ownerDSN := os.Getenv("MIAOQIA_TEST_PUBLIC_SEARCH_PG_OWNER")
	cellAppDSN := os.Getenv("MIAOQIA_TEST_CELL_SEARCH_PG_APP")
	cellOwnerDSN := os.Getenv("MIAOQIA_TEST_CELL_SEARCH_PG_OWNER")
	if appDSN == "" || ownerDSN == "" || cellAppDSN == "" || cellOwnerDSN == "" {
		t.Skip("需要独立临时 PostgreSQL search public/Cell owner 与 app DSN")
	}
	configureSearchTest(t)
	t.Setenv(publicSearchPrefix+"DB_DSN", appDSN)
	t.Setenv(publicSearchPrefix+"DB_OWNER_DSN", ownerDSN)
	t.Setenv("MIAOQIA_SEARCH_DB_DSN", cellAppDSN)
	t.Setenv("MIAOQIA_SEARCH_DB_OWNER_DSN", cellOwnerDSN)
	t.Setenv("MIAOQIA_DB_APP_ROLE", publicSearchApp) // Restore the kernel migration environment after the test.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, cellOwnerDSN, "sea_app", migrations.FS); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, testSearchLog, publicSearchConsumer); err != nil {
		t.Fatal(err)
	}
	app, err := pgx.Connect(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	assertSearchDBPrivileges(t, ctx, app, ownerDSN, cellAppDSN, cellOwnerDSN)

	certs := newSearchTLS(t)
	certs.configure("MIAOQIA_INTERNAL_TLS", "platform-public-search-workflow-worker")
	certs.configure(publicSearchPrefix+"OPENSEARCH_TLS", "public-index-writer")
	var writes, exports atomic.Int64
	var forbid, malformed atomic.Bool
	ref := searchapi.PublicRef{Kind: searchapi.PublicNotice, Domain: "INF-PROC", ID: testPublicNotice, Version: "1"}
	index := certs.server("mock-opensearch", "public-index-writer", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_refresh") {
			replySearchJSON(w, map[string]any{"_shards": map[string]int{"failed": 0}})
			return
		}
		if r.Method != http.MethodPut {
			t.Errorf("unexpected OpenSearch call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.Contains(r.URL.Path, "/_doc/") {
			var body struct {
				Ref       searchapi.PublicRef `json:"ref"`
				Withdrawn bool                `json:"withdrawn"`
				Text      string              `json:"text"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Ref != ref || !body.Withdrawn ||
				body.Text != "" || r.URL.Query().Get("version") != "2" || r.URL.Query().Get("require_alias") != "true" {
				t.Errorf("invalid OpenSearch tombstone: %v %+v", err, body)
			}
			writes.Add(1)
		}
		replySearchJSON(w, map[string]bool{"acknowledged": true})
	}))
	t.Setenv(publicSearchPrefix+"OPENSEARCH_URL", index.URL)

	event := outbox.Envelope{
		SpecVersion: "1.0", ID: ids.New("evt"), Source: "miaoqia/domain-intelligence", Type: "domain.fact.upserted.v1",
		Time: time.Now().UTC(), DataContentType: "application/json", Zone: reqctx.ZonePublic, Subject: ref.ID,
		Data: json.RawMessage(fmt.Sprintf(`{
			"info_domain":"INF-PROC","fact_id":%q,"changed_fields":["/title"],"rights_tags":["public:synthetic"],
			"snapshot_ref":"snp_00000000000000000000000000","revision_ref":"exr_00000000000000000000000000",
			"object_type":"notice","fact_version":1,"schema_version":"1.0.0"
		}`, ref.ID)),
	}
	schema, err := events.Schema(event.Type)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Validate(schema, event.Data); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := app.Exec(context.Background(), "DELETE FROM sea_public_event WHERE event_id=$1", event.ID); err != nil {
			t.Error(err)
		}
	}()
	exportHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exports.Add(1)
		if r.URL.Path != "/internal/v1/search-projection/mutations" || r.Method != http.MethodPost ||
			r.Header.Get(reqctx.Header) != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("owner was called outside its service operation")
		}
		var received outbox.Envelope
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil || received.ID != event.ID || received.Zone != reqctx.ZonePublic {
			t.Errorf("invalid owner event: %v %+v", err, received)
		}
		if forbid.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if malformed.Load() {
			replySearchJSON(w, map[string]any{"mutations": []any{
				map[string]any{"ref": ref, "revision": 2, "withdrawn": false, "text": "unapproved projection text"},
			}})
			return
		}
		replySearchJSON(w, map[string]any{"mutations": []any{
			map[string]any{"ref": ref, "revision": 2, "withdrawn": true},
		}})
	})
	owner := certs.server("public-search-export", "platform-public-search-workflow-worker", exportHandler)
	t.Setenv(publicSearchPrefix+"OWNER_EXPORT_URL", owner.URL)
	t.Setenv(publicSearchPrefix+"GENERATION", testGeneration)
	modules, envs, closeAll, err := assemble(ctx, testSearchLog, publicSearchConsumer, []string{"127.0.0.1:1"})
	defer closeAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 1 || envs[0].DB == nil || envs[0].DataScope != "public" || envs[0].Cell != "" {
		t.Fatal("consumer did not assemble one public-only module")
	}
	cctx := reqctx.Service(ctx, publicSearchConsumer)
	for _, c := range modules[0].Consumers(envs[0]) {
		if err := c.Prepare(); err != nil {
			t.Fatal(err)
		}
		if len(c.Topics) != 1 || !strings.HasPrefix(c.Topics[0], "public.") {
			t.Fatalf("consumer subscribed outside public topics: %v", c.Topics)
		}
		if handler, ok := c.Handlers[event.Type]; ok {
			if err := envs[0].DB.Tx(cctx, func(tx pgx.Tx) error { return handler(cctx, tx, &event) }); err != nil {
				t.Fatal(err)
			}
		}
	}
	if exports.Load() != 0 || writes.Load() != 0 {
		t.Fatal("public consumer contacted OpenSearch or the owner instead of journaling")
	}
	_, envs, releaseWorker, err := assemble(ctx, testSearchLog, publicSearchWorker, nil)
	defer releaseWorker()
	if err != nil {
		t.Fatal(err)
	}
	worker := envs[0]
	projector, ok := runtime.Lookup[*search.PublicProjector](worker.Bindings, publicProjectorBinding)
	if !ok || worker.DB == nil {
		t.Fatal("worker lacks a public projector and database")
	}
	wctx := reqctx.Service(ctx, publicSearchWorker)
	generation, err := projector.BeginBuild(wctx, search.PublicBuildBasis{
		SnapshotID: "synthetic-snapshot", AsOf: time.Now().UTC(),
		ReplayStart: search.PublicOffsets{
			"public.evidence": {0: 0}, "public.domain": {0: 0}, "public.entity": {0: 0}, "public.acquisition": {0: 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := app.Exec(context.Background(), "DELETE FROM sea_public_generation WHERE name=$1", generation); err != nil {
			t.Error(err)
		}
	}()
	worker.Config = func(key string) string {
		if key == "GENERATION" {
			return generation
		}
		return os.Getenv(publicSearchPrefix + key)
	}
	if err := projector.LoadSnapshot(wctx, generation, "synthetic-snapshot", nil, true); err != nil {
		t.Fatal(err)
	}
	for _, rejection := range []*atomic.Bool{&forbid, &malformed} {
		rejection.Store(true)
		if err := runPublicSearchWorker(ctx, worker); err == nil {
			t.Fatal("worker accepted a denied or invalid owner export")
		}
		status, err := projector.BuildStatus(wctx, generation)
		if err != nil || status.AppliedSequence != 0 || writes.Load() != 0 {
			t.Fatalf("rejected export advanced durable state: %+v %v", status, err)
		}
		rejection.Store(false)
	}
	attempt, finish := context.WithCancel(ctx)
	defer finish()
	finished := make(chan error, 1)
	go func() { finished <- runPublicSearchWorker(attempt, worker) }()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := projector.BuildStatus(wctx, generation)
		if err != nil {
			t.Fatal(err)
		}
		if status.AppliedSequence > 0 {
			break
		}
		select {
		case <-ticker.C:
		case err := <-finished:
			t.Fatalf("public worker exited without applying the owner export: %v", err)
		case <-ctx.Done():
			t.Fatal("worker did not advance before the fixture deadline")
		}
	}
	finish()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected worker cancellation: %v", err)
	}
	if exports.Load() != 3 || writes.Load() != 1 {
		t.Fatalf("expected two rejected exports and one applied tombstone, got exports=%d writes=%d", exports.Load(), writes.Load())
	}
	barrier := search.PublicOffsets{
		"public.evidence": {0: 0}, "public.domain": {0: 1}, "public.entity": {0: 0}, "public.acquisition": {0: 0},
	}
	rawBarrier, err := json.Marshal(barrier)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := executeSearchBuild(wctx, projector, []string{"replay-status", generation}, bytes.NewReader(rawBarrier), &output, nil); err != nil {
		t.Fatal(err)
	}
	var replay search.PublicReplayStatus
	if err := json.Unmarshal(output.Bytes(), &replay); err != nil || replay.AppliedSequence < 1 || len(replay.Gaps) != 1 ||
		replay.Gaps[0] != (search.PublicReplayGap{Topic: "public.domain", Partition: 0, Offset: 0, Reason: "missing"}) {
		t.Fatalf("unattributed journal entry was credited with a Kafka offset: %+v %v", replay, err)
	}
	if broker := os.Getenv("MIAOQIA_TEST_PUBLIC_SEARCH_BUILD_KAFKA"); broker != "" {
		t.Run("broker_tip_rejects_missing_delivery", func(t *testing.T) {
			assertPublicSearchBuildSamplesTip(t, ctx, broker, generation, replay.AppliedSequence)
		})
	}

	wrong := certs.server("untrusted-export-owner", "platform-public-search-workflow-worker", exportHandler)
	source, err := newPublicProjectionSource(wrong.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.LoadPublicMutationPage(ctx, event, ""); err == nil || exports.Load() != 3 {
		t.Fatalf("export accepted the wrong authenticated server peer: %v", err)
	}
}

func TestPublicSearchBuildRejectsOperatorObservedOffsets(t *testing.T) {
	input := strings.NewReader(`{"observed":{"public.domain":{"0":9}}}`)
	err := executeSearchBuild(t.Context(), nil, []string{"publish", testGeneration, ""}, input, &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "requires a generation") {
		t.Fatalf("publish accepted operator-supplied success offsets: %v", err)
	}
}

func assertPublicSearchBuildSamplesTip(t *testing.T, ctx context.Context, broker, generation string, sequence int64) {
	t.Helper()
	client, err := kgo.NewClient(kgo.SeedBrokers(broker))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	barrier := search.PublicOffsets{}
	for _, topic := range search.PublicEventTopics() {
		if _, err := kadm.NewClient(client).CreateTopic(ctx, 1, 1, nil, topic); err != nil {
			t.Fatalf("publish sampling test requires a disposable Kafka broker: %v", err)
		}
		barrier[topic] = map[int32]int64{0: 0}
	}
	if err := client.ProduceSync(ctx, &kgo.Record{Topic: "public.domain", Value: []byte(`{"synthetic":true}`)}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	approval := search.PublicBuildAssessment{
		Generation: generation, SnapshotID: "synthetic-snapshot", AppliedSequence: sequence, Barrier: barrier, Compared: true,
	}
	raw, err := json.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"publish", generation, ""}
	t.Setenv("MIAOQIA_KAFKA_BROKERS", "")
	if err := publicSearchBuild(ctx, publicSearchWorker, args, bytes.NewReader(raw), &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "requires MIAOQIA_KAFKA_BROKERS") {
		t.Fatalf("publish did not require a configured Kafka sample: %v", err)
	}
	t.Setenv("MIAOQIA_KAFKA_BROKERS", broker)
	if err := publicSearchBuild(ctx, publicSearchWorker, args, bytes.NewReader(raw), &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "public.domain[0] at offset 0: missing") {
		t.Fatalf("publish did not reject broker records beyond the operator barrier: %v", err)
	}
}

func assertSearchDBPrivileges(t *testing.T, ctx context.Context, app *pgx.Conn, ownerDSN, cellAppDSN, cellOwnerDSN string) {
	t.Helper()
	var database, user string
	var event, generation, delivery, inbox, cellExists bool
	if err := app.QueryRow(ctx, `SELECT current_database(), current_user,
		to_regclass('sea_public_event') IS NOT NULL, to_regclass('sea_public_generation') IS NOT NULL,
		to_regclass('sea_public_delivery') IS NOT NULL,
		to_regclass('kernel_inbox') IS NOT NULL, to_regclass('sea_watch_queries') IS NOT NULL`).
		Scan(&database, &user, &event, &generation, &delivery, &inbox, &cellExists); err != nil ||
		database != publicSearchDB || user != publicSearchApp || !event || !generation || !delivery || !inbox || cellExists {
		t.Fatalf("unexpected public database/role and migrations: %s %s %t %t %t %t %t %v",
			database, user, event, generation, delivery, inbox, cellExists, err)
	}
	var cellName string
	var publicTables, cellTables bool
	cell, err := pgx.Connect(ctx, cellAppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer cell.Close(ctx)
	if err := cell.QueryRow(ctx, `SELECT current_database(), to_regclass('sea_public_event') IS NOT NULL OR to_regclass('sea_public_delivery') IS NOT NULL,
		to_regclass('sea_watch_queries') IS NOT NULL`).Scan(&cellName, &publicTables, &cellTables); err != nil || cellName != "sea" || publicTables || !cellTables {
		t.Fatalf("unexpected Cell database and migration state: %s %t %t %v", cellName, publicTables, cellTables, err)
	}
	for _, wrong := range []struct{ source, database string }{
		{app.Config().ConnString(), "sea"}, {ownerDSN, "sea"},
		{cellAppDSN, publicSearchDB}, {cellOwnerDSN, publicSearchDB},
	} {
		config, err := pgx.ParseConfig(wrong.source)
		if err != nil {
			t.Fatal(err)
		}
		config.Database = wrong.database
		conn, err := pgx.ConnectConfig(ctx, config)
		if conn != nil {
			conn.Close(ctx)
		}
		var denied *pgconn.PgError
		if !errors.As(err, &denied) || denied.Code != "42501" {
			t.Fatalf("role %s connected to %s or failed for a different reason: %v", config.User, wrong.database, err)
		}
	}
}
