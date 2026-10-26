package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// synthModel is the subset of Synthetic's /models entries the proxy needs.
type synthModel struct {
	ID                string   `json:"id"`
	DisplayName       string   `json:"display_name"`
	Description       string   `json:"description"`
	Created           int64    `json:"created"`
	ContextLength     int64    `json:"context_length"`
	MaxOutputLength   int64    `json:"max_output_length"`
	InputModalities   []string `json:"input_modalities"`
	OutputModalities  []string `json:"output_modalities"`
	SupportedFeatures []string `json:"supported_features"`
	SupportedSampling []string `json:"supported_sampling_parameters"`
	HuggingFaceID     string   `json:"hugging_face_id"`
	AlwaysOn          bool     `json:"always_on"`
}

type modelsCacheEntry struct {
	models  []pluginapi.ModelInfo
	fetched time.Time
}

var (
	modelsMu    sync.Mutex
	modelsCache = map[string]modelsCacheEntry{}
)

// keyFingerprint identifies an account in the cache without storing the key.
func keyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

func toModelInfo(m synthModel) pluginapi.ModelInfo {
	params := append([]string(nil), m.SupportedSampling...)
	for _, feature := range m.SupportedFeatures {
		switch feature {
		case "tools":
			params = append(params, "tools", "tool_choice")
		case "json_mode", "structured_outputs":
			params = append(params, "response_format")
		case "reasoning":
			params = append(params, "reasoning_effort")
		}
	}
	sort.Strings(params)
	params = dedupeSorted(params)

	name := m.DisplayName
	if name == "" {
		name = m.ID
	}
	return pluginapi.ModelInfo{
		ID:                         m.ID,
		Object:                     "model",
		Created:                    m.Created,
		OwnedBy:                    providerID,
		DisplayName:                name,
		Name:                       m.ID,
		Description:                m.Description,
		InputTokenLimit:            m.ContextLength,
		OutputTokenLimit:           m.MaxOutputLength,
		ContextLength:              m.ContextLength,
		MaxCompletionTokens:        m.MaxOutputLength,
		SupportedGenerationMethods: []string{"chat"},
		SupportedParameters:        params,
		SupportedInputModalities:   m.InputModalities,
		SupportedOutputModalities:  m.OutputModalities,
		UserDefined:                true,
	}
}

func dedupeSorted(in []string) []string {
	out := in[:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// parseModels decodes Synthetic's OpenAI-style model list, skipping entries
// with no id.
func parseModels(body []byte) ([]pluginapi.ModelInfo, error) {
	var list struct {
		Data []synthModel `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &list); errUnmarshal != nil {
		return nil, fmt.Errorf("decode synthetic model list: %w", errUnmarshal)
	}
	models := make([]pluginapi.ModelInfo, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID == "" {
			continue
		}
		models = append(models, toModelInfo(m))
	}
	return models, nil
}

// fetchModels asks Synthetic for the models this key may use, reusing a recent
// result and falling back to a stale one if Synthetic is unreachable.
func fetchModels(callbackID string, acct account) ([]pluginapi.ModelInfo, error) {
	fingerprint := keyFingerprint(acct.APIKey)
	ttl := time.Duration(cfg().ModelsCacheSeconds) * time.Second

	modelsMu.Lock()
	cached, haveCached := modelsCache[fingerprint]
	modelsMu.Unlock()
	if haveCached && time.Since(cached.fetched) < ttl {
		return cached.models, nil
	}

	result, errDo := hostHTTPDo(httpCall{
		CallbackID: callbackID,
		Method:     http.MethodGet,
		URL:        cfg().openAIURL("/models"),
		Header:     bearer(acct.APIKey),
	})
	var models []pluginapi.ModelInfo
	var errFetch error
	switch {
	case errDo != nil:
		errFetch = errDo
	case result.Status < 200 || result.Status >= 300:
		errFetch = upstreamError(result.Status, result.Header, result.Body)
	default:
		models, errFetch = parseModels(result.Body)
	}
	if errFetch != nil {
		if haveCached {
			return cached.models, nil
		}
		return nil, errFetch
	}
	modelsMu.Lock()
	modelsCache[fingerprint] = modelsCacheEntry{models: models, fetched: time.Now()}
	modelsMu.Unlock()
	return models, nil
}

type authModelRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleModelsForAuth(request []byte) ([]byte, error) {
	var req authModelRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode model request: %w", errUnmarshal)
	}
	acct, errKey := requireKey(req.StorageJSON)
	if errKey != nil {
		return nil, errKey
	}
	models, errFetch := fetchModels(req.HostCallbackID, acct)
	if errFetch != nil {
		return nil, pluginabi.NewError("model_discovery_failed", errFetch.Error())
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerID, Models: models})
}
