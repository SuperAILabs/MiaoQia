package integration_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"

	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/relay"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/kernel/tracing"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	"github.com/superailabs/miaoqia/services/subsystems/governance"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	identityremote "github.com/superailabs/miaoqia/services/subsystems/identity/api/remote"
	"github.com/superailabs/miaoqia/services/subsystems/operations"
	operationsapi "github.com/superailabs/miaoqia/services/subsystems/operations/api"
)

func TestM0RealComponents(t *testing.T) {
	issuer := os.Getenv("MIAOQIA_TEST_KEYCLOAK")
	if issuer == "" {
		t.Skip("MIAOQIA_TEST_KEYCLOAK not set")
	}
	fga := os.Getenv("MIAOQIA_TEST_OPENFGA")
	if fga == "" {
		t.Skip("MIAOQIA_TEST_OPENFGA not set")
	}
	t.Setenv("MIAOQIA_ENV", "production")
	began := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	oldProvider := otel.GetTracerProvider()
	shutdown, err := tracing.Setup(ctx, "platform", "m0-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()); otel.SetTracerProvider(oldProvider) }()
	roots, err := transportsecurity.Roots(os.Getenv("MIAOQIA_GATEWAY_CA_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	issuerClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}, Timeout: 10 * time.Second}
	token := func(user string) string {
		values := url.Values{"grant_type": {"password"}, "client_id": {"miaoqia-dev"}, "username": {user}, "password": {user}}
		return testenv.KeycloakToken(t, issuerClient, issuer, values)
	}
	operatorToken, visitorToken := token("operator"), token("visitor")
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(operatorToken, claims); err != nil {
		t.Fatal(err)
	}
	if err := identity.Grant(ctx, fga, "user:"+claims["sub"].(string), "operator", "platform:miaoqia"); err != nil {
		t.Fatal(err)
	}
	brokers := testenv.Brokers(t)
	producer, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()
	adm := kadm.NewClient(producer)
	topics := []string{"platform.operations", "public.acquisition", "public.evidence", "platform.audit", "dlq.acquisition", "dlq.evidence", "dlq.operations", "dlq.governance"}
	if _, err := adm.CreateTopics(ctx, 3, 1, nil, topics...); err != nil {
		t.Fatal(err)
	}
	bindings := runtime.NewBindings()
	identityModule := identity.Module()
	identityEnv := &runtime.Env{Bindings: bindings, Config: func(string) string { return fga }, Log: log}
	if err := identityModule.Start(ctx, identityEnv); err != nil {
		t.Fatal(err)
	}
	// 实际identity内部路由，使用真实mTLS，operations通过生成的远程客户端调用。
	certs := testenv.NewTLS(t)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CERT", certs.CertPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_KEY", certs.KeyPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CA", certs.CA)
	t.Setenv("MIAOQIA_INTERNAL_ALLOWED_PEERS", "spiffe://miaoqia/service/platform-api")
	internal := http.NewServeMux()
	identityModule.Routes(identityEnv, http.NewServeMux(), internal)
	server := httptest.NewUnstartedServer(internal)
	server.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: certs.Pool, Certificates: []tls.Certificate{certs.Cert}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	defer server.Close()
	remote, err := identityremote.NewAuthorizer(server.URL + "/internal")
	if err != nil {
		t.Fatal(err)
	}
	bindings.Provide("identity.Authorizer", remote)
	modules := []runtime.Module{operations.Module(), governance.Module()}
	var envs []*runtime.Env
	var databases []testenv.Database
	var sources []relay.Source
	for _, m := range modules {
		d := testenv.NewDatabase(t)
		databases = append(databases, d)
		if err := db.Migrate(ctx, d.OwnerDSN, d.Name+"_app", m.Migrations); err != nil {
			t.Fatal(err)
		}
		d.GrantRelay(t)
		app, err := db.Open(ctx, d.AppDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer app.Close()
		env := &runtime.Env{DB: app, Brokers: brokers, Log: log, Bindings: bindings}
		if m.Start != nil {
			if err := m.Start(ctx, env); err != nil {
				t.Fatal(err)
			}
		}
		envs = append(envs, env)
		sources = append(sources, relay.Source{Name: m.Name, DSN: d.RelayDSN})
	}
	// 真正的Python模块在独立进程中消费；Go中继负责所有四个子系统的发件箱。
	run := fmt.Sprint(time.Now().UnixNano())
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	var workers []*exec.Cmd
	var workerLogs []*bytes.Buffer
	for _, name := range []string{"acquisition", "evidence"} {
		d := testenv.NewDatabase(t)
		databases = append(databases, d)
		if err := db.Migrate(ctx, d.OwnerDSN, "", nil); err != nil {
			t.Fatal(err)
		}
		d.GrantRelay(t)
		sources = append(sources, relay.Source{Name: name, DSN: d.RelayDSN})
		cmd := exec.CommandContext(ctx, "uv", "run", "--project", filepath.Join(root, "data"), "--locked", "--all-packages", "python", filepath.Join(root, "data/kernel/tests/m0_worker.py"))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Env = append(os.Environ(), "M0_WORKER="+name, "M0_RUN="+run, "M0_APP_DSN="+d.AppDSN, "MIAOQIA_TEST_KAFKA="+strings.Join(brokers, ","))
		output := &bytes.Buffer{}
		cmd.Stdout = output
		cmd.Stderr = output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		workers = append(workers, cmd)
		workerLogs = append(workerLogs, output)
	}
	defer func() {
		for _, w := range workers {
			_ = syscall.Kill(-w.Process.Pid, syscall.SIGTERM)
		}
		cancel()
		for _, w := range workers {
			_ = w.Wait()
		}
	}()
	errors := make(chan error, 5)
	go func() {
		errors <- (&relay.Relay{Sources: sources, Producer: producer, Log: log, Poll: 20 * time.Millisecond}).Run(ctx)
	}()
	for i, m := range modules {
		for _, c := range m.Consumers(envs[i]) {
			c.Name += "." + run
			go func() { errors <- c.Run(ctx) }()
		}
	}
	public := http.NewServeMux()
	modules[0].Routes(envs[0], public, http.NewServeMux())
	secured, err := gateway.Public(ctx, issuer, "miaoqia-api", public)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewTLSServer(secured)
	defer api.Close()
	client := api.Client()
	request := func(method, path, token string, spoof bool) *http.Response {
		r, _ := http.NewRequestWithContext(ctx, method, api.URL+path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		if spoof {
			r.Header.Set("Miaoqia-Context", "forged-context")
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	denied := request("POST", "/v1/operations/probes", visitorToken, true)
	denied.Body.Close()
	if denied.StatusCode != 403 {
		t.Fatalf("visitor=%d", denied.StatusCode)
	}
	missing := request("POST", "/v1/operations/probes", "", false)
	missing.Body.Close()
	if missing.StatusCode != 401 {
		t.Fatal(missing.StatusCode)
	}
	triggered := request("POST", "/v1/operations/probes", operatorToken, true)
	var probe operationsapi.Probe
	if err := json.NewDecoder(triggered.Body).Decode(&probe); err != nil {
		t.Fatal(err)
	}
	triggered.Body.Close()
	if triggered.StatusCode != 202 || probe.ID == "" {
		t.Fatal("trigger not accepted")
	}
	wait := func(check func() bool, message string) {
		deadline := time.Now().Add(50 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			select {
			case err := <-errors:
				t.Fatalf("component exit: %v", err)
			default:
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal(message)
	}
	wait(func() bool {
		response := request("GET", "/v1/operations/probes/"+probe.ID, operatorToken, false)
		defer response.Body.Close()
		var value operationsapi.Probe
		if json.NewDecoder(response.Body).Decode(&value) != nil {
			return false
		}
		probe = value
		return value.Status == "completed"
	}, "complete M0 probe timed out")
	if strings.Join(probe.Hops, ",") != "operations,acquisition,evidence,operations" || probe.CompletedAt == "" {
		t.Fatalf("incomplete probe: %+v", probe)
	}
	count := func(dsn, sql string, args ...any) int {
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(ctx)
		var value int
		if err := c.QueryRow(ctx, sql, args...).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	wait(func() bool {
		return count(databases[1].OwnerDSN, `SELECT count(*) FROM audit_entries WHERE resource=$1`, probe.ID) == 2
	}, "trigger/completion audit missing")
	wait(func() bool {
		return count(databases[1].OwnerDSN, `SELECT count(*) FROM audit_entries WHERE result='denied' AND occurred_at>$1`, began) >= 1
	}, "denied audit missing")
	// 验证四个子系统新探针的信封均保留同一追踪ID，不靠标题或状态代证链路。
	traceID := ""
	for _, source := range sources {
		c, err := pgx.Connect(ctx, source.DSN)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := c.Query(ctx, `SELECT envelope->>'traceparent' FROM kernel_outbox WHERE envelope->>'subject'=$1`, probe.ID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var trace string
			if err := rows.Scan(&trace); err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(trace, "-")
			if len(parts) != 4 {
				t.Fatalf("missing trace in %s", source.Name)
			}
			if traceID == "" {
				traceID = parts[1]
			} else if parts[1] != traceID {
				t.Fatal("trace continuity broken")
			}
		}
		rows.Close()
		c.Close(ctx)
	}
	// 重放完成事件不会重复审计；无效载荷须死信且不会完成另一个探针。
	c, err := pgx.Connect(ctx, databases[0].OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var completedRaw []byte
	evd := sources[3]
	ec, err := pgx.Connect(ctx, evd.DSN)
	if err != nil {
		t.Fatal(err)
	}
	err = ec.QueryRow(ctx, `SELECT envelope::text FROM kernel_outbox WHERE envelope->>'subject'=$1 LIMIT 1`, probe.ID).Scan(&completedRaw)
	ec.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}

	before := count(databases[0].OwnerDSN, `SELECT count(*) FROM probes`)
	if _, err := c.Exec(ctx, `ALTER TABLE kernel_outbox ADD CONSTRAINT m0_audit_fault CHECK (topic <> 'platform.audit') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	for _, access := range []string{operatorToken, visitorToken} {
		response := request("POST", "/v1/operations/probes", access, false)
		response.Body.Close()
		if response.StatusCode != 500 {
			t.Fatalf("audit failure must not be hidden: %d", response.StatusCode)
		}
	}
	if count(databases[0].OwnerDSN, `SELECT count(*) FROM probes`) != before {
		t.Fatal("audit failure did not roll back business record")
	}
	if _, err := c.Exec(ctx, `ALTER TABLE kernel_outbox DROP CONSTRAINT m0_audit_fault`); err != nil {
		t.Fatal(err)
	}
	var envelope outbox.Envelope
	if err := json.Unmarshal(completedRaw, &envelope); err != nil {
		t.Fatal(err)
	}
	originalID := envelope.ID
	if err := producer.ProduceSync(ctx, &kgo.Record{Topic: "public.evidence", Key: []byte(probe.ID), Value: completedRaw}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	envelope.ID = ids.New("evt")
	barrierID := envelope.ID
	barrier, _ := json.Marshal(envelope)
	if err := producer.ProduceSync(ctx, &kgo.Record{Topic: "public.evidence", Key: []byte(probe.ID), Value: barrier}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	wait(func() bool {
		return count(databases[0].OwnerDSN, `SELECT count(*) FROM kernel_inbox WHERE event_id=$1`, barrierID) == 1
	}, "same-partition replay barrier missing")
	if count(databases[0].OwnerDSN, `SELECT count(*) FROM kernel_inbox WHERE event_id=$1`, originalID) != 1 || count(databases[1].OwnerDSN, `SELECT count(*) FROM audit_entries WHERE resource=$1`, probe.ID) != 2 {
		t.Fatal("duplicate completion accounting")
	}
	type invalidCase struct {
		topic, typ, source string
		index              int
		data               json.RawMessage
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	cases := []invalidCase{
		{"platform.operations", "operations.probe.requested.v1", "operations", 2, json.RawMessage(`{"secret":"placeholder"}`)},
		{"public.acquisition", "acquisition.probe.emitted.v1", "acquisition", 3, json.RawMessage(`{"secret":"placeholder"}`)},
		{"public.evidence", "evidence.probe.forwarded.v1", "evidence", 0, json.RawMessage(`{"secret":"placeholder"}`)},
		{"platform.audit", "audit.entry.recorded.v1", "operations", 1, json.RawMessage(`{"secret":"placeholder"}`)},
		{"platform.operations", "operations.probe.requested.v1", "operations", 2, json.RawMessage(fmt.Sprintf(`{"probe_id":%q,"requested_at":%q}`, probe.ID, now))},
		{"public.acquisition", "acquisition.probe.emitted.v1", "acquisition", 3, json.RawMessage(fmt.Sprintf(`{"probe_id":%q,"hops":["operations","acquisition"]}`, probe.ID))},
		{"public.evidence", "evidence.probe.forwarded.v1", "evidence", 0, json.RawMessage(fmt.Sprintf(`{"probe_id":%q,"hops":["operations","acquisition","evidence"]}`, probe.ID))},
		{"platform.audit", "audit.entry.recorded.v1", "operations", 1, json.RawMessage(fmt.Sprintf(`{"actor":"svc:operations","actor_kind":"service","tenant":"other","subsystem":"operations","action":"test","resource":"test","result":"succeeded","occurred_at":%q}`, now))},
	}
	expected := map[string]invalidCase{}
	for _, item := range cases {
		envelope.ID = ids.New("evt")
		envelope.Subject = ids.New("prb")
		envelope.Source = "miaoqia/" + item.source
		envelope.Type = item.typ
		envelope.Data = item.data
		envelope.Zone = "public"
		if strings.HasPrefix(item.topic, "platform.") {
			envelope.Zone = "platform"
		}
		raw, _ := json.Marshal(envelope)
		expected[envelope.ID] = item
		if err := producer.ProduceSync(ctx, &kgo.Record{Topic: item.topic, Key: []byte(envelope.Subject), Value: raw}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
	dlq, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics("dlq.operations", "dlq.acquisition", "dlq.evidence", "dlq.governance"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer dlq.Close()
	seen := map[string]bool{}
	for len(seen) < len(expected) && ctx.Err() == nil {
		fetch := dlq.PollFetches(ctx)
		fetch.EachRecord(func(r *kgo.Record) {
			var e outbox.Envelope
			if json.Unmarshal(r.Value, &e) == nil {
				if _, ok := expected[e.ID]; ok {
					seen[e.ID] = true
				}
			}
		})
	}
	if len(seen) != len(expected) {
		t.Fatalf("DLQ coverage %d/%d", len(seen), len(expected))
	}
	for id, item := range expected {
		if count(databases[item.index].OwnerDSN, `SELECT count(*) FROM kernel_inbox WHERE event_id=$1`, id) != 0 {
			t.Fatal("invalid payload entered business inbox")
		}
	}
	if count(databases[0].OwnerDSN, `SELECT count(*) FROM probes`) != before {
		t.Fatal("invalid payload changed probe count")
	}
}
