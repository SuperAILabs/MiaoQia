// Package events validates runtime payloads against generated, locally embedded AsyncAPI schemas.
package events

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

//go:embed payloads.json
var payloads []byte
var once sync.Once
var schemas map[string]*jsonschema.Schema
var loadError error
var ErrUnknown = errors.New("unknown event payload schema")

func Compile(raw any) (*jsonschema.Schema, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat = true
	c.LoadURL = func(string) (io.ReadCloser, error) { return nil, fmt.Errorf("external schema loads forbidden") }
	if err := c.AddResource("https://schemas.miaoqia.com/runtime.json", bytes.NewReader(b)); err != nil {
		return nil, err
	}
	return c.Compile("https://schemas.miaoqia.com/runtime.json")
}

func Schema(name string) (*jsonschema.Schema, error) {
	once.Do(func() {
		var all struct {
			Schemas map[string]any `json:"schemas"`
		}
		if loadError = json.Unmarshal(payloads, &all); loadError != nil {
			return
		}
		schemas = map[string]*jsonschema.Schema{}
		for name, raw := range all.Schemas {
			schema, err := Compile(raw)
			if err != nil {
				loadError = err
				return
			}
			schemas[name] = schema
		}
	})
	if loadError != nil {
		return nil, loadError
	}
	s, ok := schemas[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknown, name)
	}
	return s, nil
}
func Validate(schema *jsonschema.Schema, raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return fmt.Errorf("invalid JSON payload")
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("payload_schema_invalid")
	}
	return nil
}
