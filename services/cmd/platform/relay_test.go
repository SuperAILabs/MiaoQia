package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

const testRelayTemplate = "postgres://outbox_relay@db.invalid/{db}"

func relayTestRegistry(t *testing.T) registry {
	t.Helper()
	var reg registry
	if err := readYAML(filepath.Join("..", "..", "..", "deploy", "subsystems.yaml"), &reg); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestRelaySources(t *testing.T) {
	reg := relayTestRegistry(t)
	selected := map[string]string{"acquisition": "acq", "evidence": "evd", "domain-intelligence": "dom", "entity-graph": "ent"}
	sources, err := relaySources(reg, testRelayTemplate, "entity-graph, acquisition ,domain-intelligence,evidence", true)
	if err != nil || len(sources) != len(selected) {
		t.Fatalf("select public databases: count=%d err=%v", len(sources), err)
	}
	var names []string
	for _, source := range sources {
		config, err := pgx.ParseConfig(source.DSN)
		if err != nil || config.Database != selected[source.Name] || source.Validate == nil {
			t.Fatalf("invalid public source %s", source.Name)
		}
		names = append(names, source.Name)
	}
	if !slices.Equal(names, []string{"acquisition", "domain-intelligence", "entity-graph", "evidence"}) {
		t.Fatalf("unexpected public sources: %v", names)
	}
	subset, err := relaySources(reg, testRelayTemplate, "evidence", true)
	if err != nil || len(subset) != 1 || subset[0].Name != "evidence" {
		t.Fatalf("select single source: count=%d err=%v", len(subset), err)
	}
	legacy, err := relaySources(reg, testRelayTemplate, "", false)
	if err != nil {
		t.Fatal(err)
	}
	var expected []string
	for name, entry := range reg.Subsystems {
		if entry.Database != "" {
			expected = append(expected, name)
		}
	}
	slices.Sort(expected)
	names = nil
	for _, source := range legacy {
		if source.Validate != nil {
			t.Fatalf("legacy source %s has a public validator", source.Name)
		}
		names = append(names, source.Name)
	}
	if !slices.Equal(names, expected) {
		t.Fatalf("legacy sources = %v, want %v", names, expected)
	}
}

func TestRelayRejectsMisconfiguration(t *testing.T) {
	for _, test := range []struct {
		name, selection, template string
		amend                     func(*registry)
	}{
		{name: "empty", template: testRelayTemplate},
		{name: "whitespace", selection: " ", template: testRelayTemplate},
		{name: "empty list item", selection: "acquisition,", template: testRelayTemplate},
		{name: "duplicate", selection: "evidence,evidence", template: testRelayTemplate},
		{name: "unknown after valid", selection: "acquisition,unknown", template: testRelayTemplate},
		{name: "tenant", selection: "detection", template: testRelayTemplate},
		{name: "platform", selection: "identity", template: testRelayTemplate},
		{name: "public consumer", selection: "search", template: testRelayTemplate},
		{name: "missing placeholder", selection: "acquisition", template: "postgres://db.invalid/acq"},
		{name: "wrong database", selection: "acquisition", template: "postgres://db.invalid/ctx?application_name={db}"},
		{name: "database override", selection: "acquisition", template: testRelayTemplate + "?dbname=ctx"},
		{name: "role override", selection: "acquisition", template: testRelayTemplate + "?role=ctx_app"},
		{name: "invalid DSN", selection: "acquisition", template: "postgres://db.invalid:invalid/{db}?password=synthetic-marker"},
		{name: "missing database", selection: "acquisition", template: testRelayTemplate, amend: func(reg *registry) {
			entry := reg.Subsystems["acquisition"]
			entry.Database = ""
			reg.Subsystems["acquisition"] = entry
		}},
		{name: "shared database", selection: "acquisition", template: testRelayTemplate, amend: func(reg *registry) {
			entry := reg.Subsystems["detection"]
			entry.Database = reg.Subsystems["acquisition"].Database
			reg.Subsystems["detection"] = entry
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reg := relayTestRegistry(t)
			if test.amend != nil {
				test.amend(&reg)
			}
			sources, err := relaySources(reg, test.template, test.selection, true)
			if err == nil || sources != nil || strings.Contains(err.Error(), "synthetic-marker") {
				t.Fatalf("expected sanitized rejection and no sources, got sources=%d err=%v", len(sources), err)
			}
		})
	}
}

func TestRunRelayRejectsInvalidPublicConfigBeforeStart(t *testing.T) {
	t.Setenv("MIAOQIA_DEPLOY_DIR", filepath.Join("..", "..", "..", "deploy"))
	t.Setenv("MIAOQIA_RELAY_DSN_TEMPLATE", testRelayTemplate)
	t.Setenv("MIAOQIA_RELAY_SOURCES", "")
	if err := runRelay(t.Context(), nil, []string{"db.invalid:9092"}); err == nil || !strings.Contains(err.Error(), "MIAOQIA_RELAY_SOURCES") {
		t.Fatalf("explicit empty selection: %v", err)
	}
	t.Setenv("MIAOQIA_RELAY_SOURCES", "acquisition")
	if err := runRelay(t.Context(), nil, []string{" "}); err == nil || !strings.Contains(err.Error(), "MIAOQIA_KAFKA_BROKERS") {
		t.Fatalf("empty Kafka endpoint: %v", err)
	}
}

func TestRelayPublicEventContracts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "contracts", "events", "payloads.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Cases []struct {
			Event string          `json:"event"`
			Name  string          `json:"name"`
			Valid bool            `json:"valid"`
			Value json.RawMessage `json:"value"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for name, namespace := range map[string]string{"acquisition": "acquisition", "evidence": "evidence", "domain-intelligence": "domain", "entity-graph": "entity"} {
		sources, err := relaySources(relayTestRegistry(t), testRelayTemplate, name, true)
		if err != nil {
			t.Fatal(err)
		}
		checked := 0
		for _, fixture := range fixtures.Cases {
			if !fixture.Valid || fixture.Name != "base" || !strings.HasPrefix(fixture.Event, namespace+".") {
				continue
			}
			checked++
			t.Run(fixture.Event, func(t *testing.T) {
				envelope := relayTestEnvelope("miaoqia/"+name, fixture.Event, fixture.Value)
				if err := sources[0].Validate("public."+namespace, marshalRelayEnvelope(t, envelope)); err != nil {
					t.Fatal(err)
				}
			})
		}
		if checked == 0 {
			t.Fatalf("no public event fixtures for %s", name)
		}
	}
}

func TestRelayRejectsInvalidPublicEvents(t *testing.T) {
	allowed := relayTestEnvelope("miaoqia/acquisition", "acquisition.probe.emitted.v1", json.RawMessage(`{"probe_id":"prb_00000000000000000000000000","hops":["fixture-value"]}`))
	validate := publicRelayValidator("acquisition", "acquisition")
	for _, test := range []struct {
		name, topic string
		amend       func(*outbox.Envelope)
	}{
		{name: "tenant topic", topic: "cell-01.acquisition"},
		{name: "platform topic", topic: "platform.acquisition"},
		{name: "different public topic", topic: "public.evidence"},
		{name: "topic lookalike", topic: "public.acquisition.other"},
		{name: "wrong source", topic: "public.acquisition", amend: func(env *outbox.Envelope) { env.Source = "miaoqia/evidence" }},
		{name: "wrong namespace", topic: "public.acquisition", amend: func(env *outbox.Envelope) { env.Type = "evidence.probe.forwarded.v1" }},
		{name: "unknown type", topic: "public.acquisition", amend: func(env *outbox.Envelope) { env.Type = "acquisition.probe.emitted.v999" }},
		{name: "invalid payload", topic: "public.acquisition", amend: func(env *outbox.Envelope) { env.Data = json.RawMessage(`{}`) }},
		{name: "tenant envelope on public topic", topic: "public.acquisition", amend: func(env *outbox.Envelope) {
			env.Zone, env.Tenant, env.Cell = reqctx.ZoneTenant, "tenant", "01"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			envelope := allowed
			if test.amend != nil {
				test.amend(&envelope)
			}
			if err := validate(test.topic, marshalRelayEnvelope(t, envelope)); err == nil {
				t.Fatal("invalid record admitted")
			}
		})
	}
}

func relayTestEnvelope(source, eventType string, data json.RawMessage) outbox.Envelope {
	return outbox.Envelope{SpecVersion: "1.0", ID: "evt_00000000000000000000000000",
		Source: source, Type: eventType, Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		DataContentType: "application/json", Zone: reqctx.ZonePublic, Data: data}
}

func marshalRelayEnvelope(t *testing.T, envelope outbox.Envelope) []byte {
	t.Helper()
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
