package outbox

import (
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

func TestTopic(t *testing.T) {
	cases := map[string]string{
		Topic(reqctx.ZonePublic, "", "domain"):       "public.domain",
		Topic(reqctx.ZonePlatform, "", "operations"): "platform.operations",
		Topic(reqctx.ZoneTenant, "01", "detection"):  "cell-01.detection",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("主题 %s，期望 %s", got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	base := Envelope{SpecVersion: "1.0", ID: "evt_1", Source: "miaoqia/x", Type: "x.y.z.v1"}
	pub := base
	pub.Zone, pub.Tenant = reqctx.ZonePublic, "tnt_a"
	if pub.Validate() == nil {
		t.Error("公共域事件携带租户应无效")
	}
	ten := base
	ten.Zone, ten.Tenant = reqctx.ZoneTenant, "tnt_a"
	if ten.Validate() == nil {
		t.Error("租户事件缺少单元应无效")
	}
	ten.Cell = "01"
	if err := ten.Validate(); err != nil {
		t.Error(err)
	}
}
