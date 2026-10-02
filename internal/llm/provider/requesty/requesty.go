// Package requesty is the hosted Requesty Chat Completions llm.Provider.
//
// It is pure Go, available in every build. Requesty is an LLM gateway
// that fronts many vendors behind one OpenAI-compatible
// /v1/chat/completions endpoint (router.requesty.ai); model ids carry a
// vendor prefix, e.g. "openai/gpt-4o-mini" or
// "anthropic/claude-sonnet-4-5". The wire format, the json_schema
// structured-output mechanism, and the hollow-200 retry all live in the
// shared openaicompat.Client; this package is just the constructor that
// addresses the router with a Bearer key.
package requesty

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/provider/openaicompat"
)

// New constructs the Requesty provider. The API key is read from the env
// var named by cfg.APIKeyEnv (default REQUESTY_API_KEY); an unset key is
// a hard error.
//
// Requesty publishes its base URLs with the version segment
// ("https://router.requesty.ai/v1", or the regional
// "https://router.eu.requesty.ai/v1"), so a trailing "/v1" is accepted
// and stripped before "/v1/chat/completions" is appended.
func New(cfg llm.RemoteConfig) (llm.Provider, error) {
	keyEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if keyEnv == "" {
		keyEnv = "REQUESTY_API_KEY"
	}
	key := strings.TrimSpace(os.Getenv(keyEnv))
	if key == "" {
		return nil, fmt.Errorf("requesty: API key env %q is not set", keyEnv)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("requesty: llm.requesty.model is empty")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://router.requesty.ai"
	}
	base = strings.TrimSuffix(base, "/v1")
	return &openaicompat.Client{
		ProviderID:      "requesty",
		Tag:             "requesty",
		Model:           cfg.Model,
		URL:             base + "/v1/chat/completions",
		Headers:         map[string]string{"authorization": "Bearer " + key},
		HTTPClient:      &http.Client{Timeout: 120 * time.Second},
		SchemaMode:      openaicompat.SchemaJSONSchema,
		ReasoningEffort: strings.TrimSpace(cfg.Effort),
	}, nil
}
