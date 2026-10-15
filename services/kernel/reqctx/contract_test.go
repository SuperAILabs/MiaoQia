package reqctx

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

func TestContextContractFixtures(t *testing.T) {
	raw, err := os.ReadFile("../../../contracts/tests/integration.request-context.json")
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
	if _, err := Decode(base64.RawURLEncoding.EncodeToString(spec.Base)); err != nil {
		t.Fatal(err)
	}
	for _, c := range spec.Cases {
		t.Run(c.Name, func(t *testing.T) {
			_, err := Decode(base64.RawURLEncoding.EncodeToString(c.Instance))
			if (err == nil) != c.Valid {
				t.Fatalf("valid=%v err=%v", c.Valid, err)
			}
		})
	}
	for _, input := range []string{"", "*invalid*", base64.RawURLEncoding.EncodeToString([]byte("null")), base64.RawURLEncoding.EncodeToString([]byte(`{"principal":{"id":"svc:x","kind":"service"}} {}`))} {
		if _, err := Decode(input); err == nil {
			t.Errorf("invalid wire=%q", input)
		}
	}
}
