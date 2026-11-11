package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// anthropicVersion is the anthropic-version header the adapter is written against.
const anthropicVersion = "2023-06-01"

// AnthropicConfig configures the Anthropic Messages API adapter.
type AnthropicConfig struct {
	BaseURL          string // the request goes to BaseURL + "/v1/messages"
	APIKey           string
	DefaultMaxTokens int // sent when the call sets no output cap; max_tokens is required by the API
}

// Anthropic is the native adapter for the Anthropic Messages API. Structured output uses
// output_config.format {type: json_schema}; the model returns the JSON as a text block.
type Anthropic struct {
	cfg  AnthropicConfig
	http vendorHTTP
}

var _ Provider = (*Anthropic)(nil)

// NewAnthropic builds the adapter; client nil means http.DefaultClient.
func NewAnthropic(cfg AnthropicConfig, client *http.Client) (*Anthropic, error) {
	if cfg.BaseURL == "" || cfg.APIKey == "" {
		return nil, fmt.Errorf("%s: base URL and API key are required", VendorAnthropic)
	}
	if cfg.DefaultMaxTokens <= 0 {
		cfg.DefaultMaxTokens = defaultDecisionMaxTokens
	}
	if client == nil {
		client = http.DefaultClient
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Anthropic{cfg: cfg, http: vendorHTTP{client: client, vendor: VendorAnthropic}}, nil
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func (p *Anthropic) complete(ctx context.Context, ep Endpoint, r rendered, maxTokens int) (completion, error) {
	if maxTokens <= 0 {
		maxTokens = p.cfg.DefaultMaxTokens
	}
	body := map[string]any{
		"model":         ep.Model,
		"max_tokens":    maxTokens,
		"system":        r.System,
		"messages":      []chatMessage{{Role: "user", Content: r.User}},
		"output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": r.Schema}},
	}
	var resp anthropicResponse
	err := p.http.post(ctx, p.cfg.BaseURL+"/v1/messages",
		map[string]string{"x-api-key": p.cfg.APIKey, "anthropic-version": anthropicVersion}, body, &resp)
	if err != nil {
		return completion{}, err
	}
	if resp.Usage == nil {
		return completion{}, fmt.Errorf("%s: response has no usage", VendorAnthropic)
	}
	u := resp.Usage
	// Cache writes and reads are input tokens too; metering them at the input price is an upper bound.
	res := completion{usage: ProviderUsage{
		InputTokens:  u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens,
		OutputTokens: u.OutputTokens, Requests: 1, Reported: true,
	}}
	if resp.StopReason == "refusal" {
		res.refused = true
		return res, nil
	}
	for _, c := range resp.Content {
		if c.Type == "text" {
			res.content = c.Text
			return res, nil
		}
	}
	return res, fmt.Errorf("%s: %w: response has no text block (stop_reason %q)", VendorAnthropic, ErrInvalidOutput, resp.StopReason)
}

// RequestOverheadFloor implements OverheadFloor.
func (p *Anthropic) RequestOverheadFloor() int { return fixedTemplateBytes(false) }

func (p *Anthropic) Generate(ctx context.Context, ep Endpoint, req ProviderRequest) (json.RawMessage, ProviderUsage, error) {
	return generateWith(ctx, VendorAnthropic, p.complete, ep, req)
}

func (p *Anthropic) Classify(ctx context.Context, ep Endpoint, req ProviderClassifyRequest) (ProviderDecision, ProviderUsage, error) {
	return decideWith(ctx, p.complete, ep, renderClassify(req), req.MaxOutputTokens, false)
}

func (p *Anthropic) Score(ctx context.Context, ep Endpoint, req ProviderScoreRequest) (ProviderDecision, ProviderUsage, error) {
	return decideWith(ctx, p.complete, ep, renderScore(req), req.MaxOutputTokens, true)
}
