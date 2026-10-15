package consumer

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
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

// 信封契约允许平台域事件只带租户；事件上下文须是有效请求上下文（租户与单元成对），否则 DB.Tx 会拒绝该合法事件。
func TestEventContextForPlatformEventWithTenantOnly(t *testing.T) {
	env := &outbox.Envelope{Zone: reqctx.ZonePlatform, Tenant: "tnt_1"}
	c := reqctx.Service(reqctx.With(context.Background(), eventContext(env)), "test")
	rc, _ := reqctx.From(c)
	if err := rc.Validate(); err != nil || rc.Tenant != "" || rc.Cell != "" {
		t.Fatalf("context %+v err %v", rc, err)
	}
	env = &outbox.Envelope{Zone: reqctx.ZoneTenant, Tenant: "tnt_1", Cell: "c1"}
	if got := eventContext(env); got.Tenant != "tnt_1" || got.Cell != "c1" {
		t.Fatalf("tenant event context %+v", got)
	}
}
