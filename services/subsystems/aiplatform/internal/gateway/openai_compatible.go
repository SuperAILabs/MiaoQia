package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Structured output modes of an OpenAI-compatible endpoint.
const (
	// StructuredJSONSchema sends response_format {type: json_schema} with the output schema.
	StructuredJSONSchema = "json_schema"
	// StructuredJSONObject sends response_format {type: json_object} and puts the schema in the system
	// message, for vendors without JSON Schema support. The gateway validates the output either way.
	StructuredJSONObject = "json_object"
)

// OpenAICompatibleConfig configures one vendor that speaks the OpenAI Chat Completions protocol.
type OpenAICompatibleConfig struct {
	Vendor  string // name used in errors, for example deepseek
	BaseURL string // the request goes to BaseURL + "/chat/completions"
	APIKey  string
	// Structured is StructuredJSONSchema or StructuredJSONObject.
	Structured string
	// Strict sets json_schema.strict; strict mode rejects schemas that are not closed and fully required.
	Strict bool
	// MaxTokensParam is the request field for the output cap: max_tokens or max_completion_tokens.
	MaxTokensParam string
	// DefaultMaxTokens is sent when the call sets no output cap (request-billed decision endpoints).
	DefaultMaxTokens int
}

// OpenAICompatible is the adapter for OpenAI Chat Completions compatible endpoints
// (OpenAI, DeepSeek, Doubao on Volcengine Ark, and Google Gemini's OpenAI-compatible endpoint).
type OpenAICompatible struct {
	cfg  OpenAICompatibleConfig
	http vendorHTTP
}

var _ Provider = (*OpenAICompatible)(nil)

// NewOpenAICompatible builds the adapter; client nil means http.DefaultClient. The endpoint timeout comes from the gateway's context.
func NewOpenAICompatible(cfg OpenAICompatibleConfig, client *http.Client) (*OpenAICompatible, error) {
	if cfg.BaseURL == "" || cfg.APIKey == "" {
		return nil, fmt.Errorf("%s: base URL and API key are required", cfg.Vendor)
	}
	switch cfg.Structured {
	case StructuredJSONSchema, StructuredJSONObject:
	default:
		return nil, fmt.Errorf("%s: unknown structured output mode %q", cfg.Vendor, cfg.Structured)
	}
	switch cfg.MaxTokensParam {
	case "":
		cfg.MaxTokensParam = "max_tokens"
	case "max_tokens", "max_completion_tokens":
	default:
		return nil, fmt.Errorf("%s: unknown max tokens parameter %q", cfg.Vendor, cfg.MaxTokensParam)
	}
	if cfg.DefaultMaxTokens <= 0 {
		cfg.DefaultMaxTokens = defaultDecisionMaxTokens
	}
	if client == nil {
		client = http.DefaultClient
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &OpenAICompatible{cfg: cfg, http: vendorHTTP{client: client, vendor: cfg.Vendor}}, nil
}

// defaultDecisionMaxTokens is the output cap for calls that set none; it bounds reasoning tokens as well.
const defaultDecisionMaxTokens = 1024

type oaiResponse struct {
	Choices []struct {
		Message struct {
			Content *string `json:"content"`
			Refusal *string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (p *OpenAICompatible) complete(ctx context.Context, ep Endpoint, r rendered, maxTokens int) (completion, error) {
	system := r.System
	body := map[string]any{"model": ep.Model}
	switch p.cfg.Structured {
	case StructuredJSONSchema:
		body["response_format"] = map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": r.SchemaName, "schema": r.Schema, "strict": p.cfg.Strict},
		}
	case StructuredJSONObject:
		system += "\n\nRespond with one JSON object that conforms to this JSON Schema:\n" + string(r.Schema)
		body["response_format"] = map[string]any{"type": "json_object"}
	}
	body["messages"] = []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: r.User}}
	if maxTokens <= 0 {
		maxTokens = p.cfg.DefaultMaxTokens
	}
	body[p.cfg.MaxTokensParam] = maxTokens

	var resp oaiResponse
	if err := p.http.post(ctx, p.cfg.BaseURL+"/chat/completions",
		map[string]string{"Authorization": "Bearer " + p.cfg.APIKey}, body, &resp); err != nil {
		return completion{}, err
	}
	if resp.Usage == nil {
		// Without usage the call cannot be metered; it fails and the gateway charges the pre-call upper bound.
		return completion{}, fmt.Errorf("%s: response has no usage", p.cfg.Vendor)
	}
	res := completion{usage: ProviderUsage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens, Requests: 1, Reported: true}}
	if len(resp.Choices) == 0 {
		return res, fmt.Errorf("%s: %w: response has no choices", p.cfg.Vendor, ErrInvalidOutput)
	}
	c := resp.Choices[0]
	if (c.Message.Refusal != nil && *c.Message.Refusal != "") || c.FinishReason == "content_filter" {
		res.refused = true
		return res, nil
	}
	if c.Message.Content == nil {
		return res, fmt.Errorf("%s: %w: response has no content (finish_reason %q)", p.cfg.Vendor, ErrInvalidOutput, c.FinishReason)
	}
	res.content = *c.Message.Content
	return res, nil
}

func (p *OpenAICompatible) Generate(ctx context.Context, ep Endpoint, req ProviderRequest) (json.RawMessage, ProviderUsage, error) {
	return generateWith(ctx, p.cfg.Vendor, p.complete, ep, req)
}

func (p *OpenAICompatible) Classify(ctx context.Context, ep Endpoint, req ProviderClassifyRequest) (ProviderDecision, ProviderUsage, error) {
	return decideWith(ctx, p.complete, ep, renderClassify(req), req.MaxOutputTokens, false)
}

func (p *OpenAICompatible) Score(ctx context.Context, ep Endpoint, req ProviderScoreRequest) (ProviderDecision, ProviderUsage, error) {
	return decideWith(ctx, p.complete, ep, renderScore(req), req.MaxOutputTokens, true)
}
