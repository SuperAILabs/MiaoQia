package outbox

import (
	"encoding/json"
	"os"
	"testing"
)

// 两语言直接读取同一完整实例，正反例由mqc校验；避免各自维护不同边界表。
func TestEnvelopeContractFixtures(t *testing.T) {
	raw, err := os.ReadFile("../../../contracts/tests/integration.event-envelope.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Base  json.RawMessage `json:"base"`
		Cases []struct {
			Name     string          `json:"name"`
			Valid    bool            `json:"valid"`
			Instance json.RawMessage `json:"instance"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	var base Envelope
	if err := json.Unmarshal(spec.Base, &base); err != nil {
		t.Fatal(err)
	}
	for _, c := range spec.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var env Envelope
			err := json.Unmarshal(c.Instance, &env)
			if (err == nil) != c.Valid {
				t.Fatalf("valid=%v err=%v", c.Valid, err)
			}
		})
	}
	for _, topic := range []string{"public.domain", "platform.audit", "cell-01.detection", "evil.domain", "public", "public.domain.extra", "cell-02.detection"} {
		want := topic == "public.domain"
		if (base.CheckTopic(topic) == nil) != want {
			t.Errorf("topic=%s", topic)
		}
	}
}
