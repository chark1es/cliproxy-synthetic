package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	// providerID is the provider key shared by the auth provider, executor and
	// quota provider. Auth files select it through their "type" field.
	providerID = "synthetic"
	repoURL    = "https://github.com/chark1es/cliproxy-synthetic"
)

// hostClient abstracts callbacks into CLIProxyAPI so the plugin logic can be
// unit tested without loading the shared library into a host.
type hostClient interface {
	Call(method string, payload any) (json.RawMessage, error)
}

// host is replaced with the cgo implementation in init (see main.go).
var host hostClient

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type capabilities struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool                         `json:"management_api"`
	QuotaProvider         bool                         `json:"quota_provider"`
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

// errorEnvelope serializes err as a failed RPC envelope. A *pluginabi.Error
// keeps its HTTP status and retry hints; anything else becomes a plugin_error.
func errorEnvelope(err error) []byte {
	var pluginErr *pluginabi.Error
	if !errors.As(err, &pluginErr) || pluginErr == nil {
		pluginErr = pluginabi.NewError("plugin_error", err.Error())
	}
	raw, errMarshal := json.Marshal(pluginabi.Envelope{OK: false, Error: pluginErr})
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"failed to encode error"}}`)
	}
	return raw
}

// safeHandleMethod keeps a panic from unwinding across the cgo boundary,
// where it would take the whole proxy process down.
func safeHandleMethod(method string, request []byte) (raw []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			raw = nil
			err = pluginabi.NewError("plugin_panic", fmt.Sprintf("synthetic plugin panic in %s: %v", method, recovered))
		}
	}()
	return handleMethod(method, request)
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return handleRegister(request)
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return okEnvelope(struct{}{})

	case pluginabi.MethodModelStatic:
		return okEnvelope(pluginapi.ModelResponse{Provider: providerID})
	case pluginabi.MethodModelForAuth:
		return handleModelsForAuth(request)

	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerID})
	case pluginabi.MethodAuthParse:
		return handleAuthParse(request)
	case pluginabi.MethodAuthLoginStart:
		return nil, pluginabi.NewError("login_unsupported", loginUnsupportedMessage, 400)
	case pluginabi.MethodAuthLoginPoll:
		return okEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: loginUnsupportedMessage})
	case pluginabi.MethodAuthRefresh:
		return handleAuthRefresh()

	case pluginabi.MethodExecutorExecute:
		return handleExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecuteStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return handleCountTokens(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return handleExecutorHTTPRequest(request)

	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(pluginapi.QuotaDescribeResponse{
			SupportedProviders: []string{providerID},
			DisplayName:        "Synthetic",
		})
	case pluginabi.MethodQuotaFetch:
		return handleQuotaFetch(request)
	case pluginabi.MethodQuotaReset:
		return okEnvelope(pluginapi.QuotaResetResponse{Success: false, Message: "Synthetic quotas cannot be reset"})

	case pluginabi.MethodManagementRegister:
		return handleManagementRegister()
	case pluginabi.MethodManagementHandle:
		return handleManagementRequest(request)

	default:
		return nil, pluginabi.NewError("unknown_method", "unknown method: "+method)
	}
}

func handleRegister(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
		}
	}
	if errConfig := applyConfig(req.ConfigYAML); errConfig != nil {
		return nil, errConfig
	}
	// Never claim a newer schema than the host speaks; it rejects those.
	schema := pluginabi.SchemaVersion
	if req.SchemaVersion > 0 && req.SchemaVersion < schema {
		schema = req.SchemaVersion
	}
	return okEnvelope(registration{
		SchemaVersion: schema,
		Metadata:      pluginMetadata(),
		Capabilities: capabilities{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  []string{"chat-completions", "claude"},
			ExecutorOutputFormats: []string{"chat-completions", "claude"},
			ManagementAPI:         true,
			QuotaProvider:         true,
		},
	})
}

func pluginMetadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:             "Synthetic",
		Version:          version,
		Author:           "chark1es",
		GitHubRepository: repoURL,
		ConfigFields: []pluginapi.ConfigField{
			{
				Name:        "base_url",
				Type:        pluginapi.ConfigFieldTypeString,
				Default:     defaultBaseURL,
				Description: "Synthetic API origin. Change only to point at a mirror or test server.",
			},
			{
				Name:        "models_cache_seconds",
				Type:        pluginapi.ConfigFieldTypeInteger,
				Default:     defaultModelsCacheSeconds,
				Description: "How long a discovered model list is reused before Synthetic is asked again.",
			},
		},
	}
}
