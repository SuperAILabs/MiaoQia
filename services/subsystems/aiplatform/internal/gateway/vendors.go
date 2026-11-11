package gateway

import (
	"fmt"
	"net/http"
	"strings"
)

// Vendor adapter names as used in the catalog's provider field (ADR-0015).
const (
	VendorDeepSeek  = "deepseek"
	VendorDoubao    = "doubao"
	VendorOpenAI    = "openai"
	VendorAnthropic = "anthropic"
	VendorGoogle    = "google"
)

// openAICompatibleDefaults holds the vendors served by the shared OpenAI-compatible adapter.
// Structured is the default mode: DeepSeek accepts only json_object, the others accept json_schema.
// Strict (decision schemas only) defaults on for OpenAI, whose strict mode is documented; others opt in.
var openAICompatibleDefaults = []OpenAICompatibleConfig{
	{Vendor: VendorDeepSeek, BaseURL: "https://api.deepseek.com", Structured: StructuredJSONObject, MaxTokensParam: "max_tokens"},
	{Vendor: VendorDoubao, BaseURL: "https://ark.cn-beijing.volces.com/api/v3", Structured: StructuredJSONSchema, MaxTokensParam: "max_tokens"},
	{Vendor: VendorOpenAI, BaseURL: "https://api.openai.com/v1", Structured: StructuredJSONSchema, Strict: true, MaxTokensParam: "max_completion_tokens"},
	{Vendor: VendorGoogle, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", Structured: StructuredJSONSchema, MaxTokensParam: "max_tokens"},
}

const anthropicDefaultBaseURL = "https://api.anthropic.com"

// VendorProviders builds the adapters of the vendors whose API key is configured. config reads the
// subsystem configuration (in the platform binary, KEY resolves to the env var MIAOQIA_AI_PLATFORM_<KEY>):
//
//	<VENDOR>_API_KEY     API key; the vendor is registered only when set. Never committed to the repository.
//	<VENDOR>_BASE_URL    optional base URL override (another region, a proxy, or a private deployment)
//	<VENDOR>_STRUCTURED  optional structured output mode for OpenAI-compatible vendors: json_schema or json_object
//	<VENDOR>_STRICT      optional true or false: strict json_schema for the decision schemas (OpenAI-compatible vendors)
//
// VENDOR is DEEPSEEK, DOUBAO, OPENAI, ANTHROPIC or GOOGLE. A catalog endpoint whose provider is not
// registered makes New fail, so a missing key surfaces at startup rather than at the first call.
func VendorProviders(config func(string) string, client *http.Client) (map[string]Provider, error) {
	out := map[string]Provider{}
	for _, cfg := range openAICompatibleDefaults {
		prefix := strings.ToUpper(cfg.Vendor) + "_"
		if cfg.APIKey = config(prefix + "API_KEY"); cfg.APIKey == "" {
			continue
		}
		if v := config(prefix + "BASE_URL"); v != "" {
			cfg.BaseURL = v
		}
		if v := config(prefix + "STRUCTURED"); v != "" {
			cfg.Structured = v
		}
		switch v := config(prefix + "STRICT"); v {
		case "":
		case "true":
			cfg.Strict = true
		case "false":
			cfg.Strict = false
		default:
			return nil, fmt.Errorf("%s: %sSTRICT must be true or false, got %q", cfg.Vendor, prefix, v)
		}
		p, err := NewOpenAICompatible(cfg, client)
		if err != nil {
			return nil, err
		}
		out[cfg.Vendor] = p
	}
	if key := config("ANTHROPIC_API_KEY"); key != "" {
		base := anthropicDefaultBaseURL
		if v := config("ANTHROPIC_BASE_URL"); v != "" {
			base = v
		}
		p, err := NewAnthropic(AnthropicConfig{BaseURL: base, APIKey: key}, client)
		if err != nil {
			return nil, err
		}
		out[VendorAnthropic] = p
	}
	return out, nil
}
