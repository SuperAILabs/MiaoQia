package events

import (
	"encoding/json"
	"testing"
)

func TestAllPayloadFixtures(t *testing.T) {
	var all struct {
		Cases []struct {
			Event, Name string
			Valid       bool
			Value       json.RawMessage
		}
	}
	if err := json.Unmarshal(payloads, &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Cases) == 0 {
		t.Fatal("empty payload cases")
	}
	for _, c := range all.Cases {
		t.Run(c.Event+"/"+c.Name, func(t *testing.T) {
			s, err := Schema(c.Event)
			if err != nil {
				t.Fatal(err)
			}
			if err := Validate(s, c.Value); (err == nil) != c.Valid {
				t.Fatalf("valid=%v error=%v", c.Valid, err)
			}
		})
	}
}
