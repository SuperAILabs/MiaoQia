package consumer

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"testing"
)

func TestMissingPayloadSchemaFailsClosed(t *testing.T) {
	c := Consumer{Handlers: map[string]Handler{"test.undefined.event.v1": func(context.Context, pgx.Tx, *outbox.Envelope) error { return nil }}}
	if err := c.Prepare(); err == nil {
		t.Fatal("unvalidated handler accepted")
	}
}
func TestCanonicalPayloadCannotBeOverridden(t *testing.T) {
	name := "operations.probe.requested.v1"
	c := Consumer{Handlers: map[string]Handler{name: func(context.Context, pgx.Tx, *outbox.Envelope) error { return nil }}, PayloadSchemas: map[string]any{name: map[string]any{}}}
	if err := c.Prepare(); err == nil {
		t.Fatal("canonical schema bypass accepted")
	}
}
