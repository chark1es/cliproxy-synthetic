package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type executorHTTPRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type executorStreamResult struct {
	Headers http.Header `json:"headers,omitempty"`
}

// isAnthropicFormat reports whether the host wants the Anthropic Messages API.
func isAnthropicFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "claude", "anthropic":
		return true
	}
	return false
}

// baseModel strips a trailing "(...)" thinking suffix, which is host syntax and
// not a Synthetic model name.
func baseModel(model string) string {
	model = strings.TrimSpace(model)
	if i := strings.LastIndex(model, "("); i > 0 && strings.HasSuffix(model, ")") {
		return strings.TrimSpace(model[:i])
	}
	return model
}

// preparePayload pins the model and stream flag on the translated request. When
// streaming an OpenAI request it also asks for a final usage chunk so the host
// can record token usage.
func preparePayload(payload []byte, model string, stream, anthropic bool) ([]byte, error) {
	var obj map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(payload, &obj); errUnmarshal != nil || obj == nil {
		return nil, pluginabi.NewError("invalid_request", "request payload is not a JSON object", http.StatusBadRequest)
	}
	set := func(key string, value any) error {
		raw, errMarshal := json.Marshal(value)
		if errMarshal != nil {
			return errMarshal
		}
		obj[key] = raw
		return nil
	}
	if m := baseModel(model); m != "" {
		if err := set("model", m); err != nil {
			return nil, err
		}
	}
	if err := set("stream", stream); err != nil {
		return nil, err
	}
	if stream && !anthropic {
		options := map[string]any{}
		if raw, ok := obj["stream_options"]; ok {
			_ = json.Unmarshal(raw, &options)
		}
		options["include_usage"] = true
		if err := set("stream_options", options); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(obj); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// upstreamRequest builds the Synthetic call for a host executor request. Only
// a fixed set of headers is forwarded: the inbound Authorization header holds
// the proxy client's key and must never reach Synthetic.
func upstreamRequest(req executorRequest, stream bool, path string) (httpCall, error) {
	acct, errKey := requireKey(req.StorageJSON)
	if errKey != nil {
		return httpCall{}, errKey
	}
	anthropic := isAnthropicFormat(req.Format)
	body, errBody := preparePayload(req.Payload, req.Model, stream, anthropic)
	if errBody != nil {
		return httpCall{}, errBody
	}
	header := bearer(acct.APIKey)
	header.Set("Content-Type", "application/json")
	if stream {
		header.Set("Accept", "text/event-stream")
	} else {
		header.Set("Accept", "application/json")
	}
	var target string
	if anthropic {
		target = cfg().anthropicURL(path)
		version := req.Headers.Get("Anthropic-Version")
		if version == "" {
			version = "2023-06-01"
		}
		header.Set("Anthropic-Version", version)
		if beta := req.Headers.Get("Anthropic-Beta"); beta != "" {
			header.Set("Anthropic-Beta", beta)
		}
	} else {
		target = cfg().openAIURL(path)
	}
	return httpCall{
		CallbackID: req.HostCallbackID,
		Method:     http.MethodPost,
		URL:        target,
		Header:     header,
		Body:       body,
	}, nil
}

func chatPath(req executorRequest) string {
	if isAnthropicFormat(req.Format) {
		return "/messages"
	}
	return "/chat/completions"
}

func contentType(header http.Header, fallback string) string {
	if ct := header.Get("Content-Type"); ct != "" {
		return ct
	}
	return fallback
}

func handleExecute(request []byte) ([]byte, error) {
	var req executorRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode executor request: %w", errUnmarshal)
	}
	call, errBuild := upstreamRequest(req, false, chatPath(req))
	if errBuild != nil {
		return nil, errBuild
	}
	result, errDo := hostHTTPDo(call)
	if errDo != nil {
		return nil, pluginabi.NewError("upstream_unreachable", errDo.Error(), http.StatusBadGateway)
	}
	if result.Status < 200 || result.Status >= 300 {
		return nil, upstreamError(result.Status, result.Header, result.Body)
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: result.Body,
		Headers: http.Header{"Content-Type": []string{contentType(result.Header, "application/json")}},
	})
}

// handleExecuteStream opens the upstream stream synchronously, so auth, quota
// and request errors surface with their real HTTP status, then pumps the body to
// the client from a goroutine.
func handleExecuteStream(request []byte) ([]byte, error) {
	var req executorRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode executor request: %w", errUnmarshal)
	}
	if req.StreamID == "" {
		return nil, pluginabi.NewError("executor_error", "stream_id is required for executor.execute_stream")
	}
	call, errBuild := upstreamRequest(req, true, chatPath(req))
	if errBuild != nil {
		return nil, errBuild
	}
	opened, errOpen := hostHTTPDoStream(call)
	if errOpen != nil {
		return nil, pluginabi.NewError("upstream_unreachable", errOpen.Error(), http.StatusBadGateway)
	}
	if opened.StatusCode < 200 || opened.StatusCode >= 300 {
		body := drainStream(opened.StreamID, 64<<10)
		hostHTTPStreamClose(opened.StreamID)
		return nil, upstreamError(opened.StatusCode, opened.Headers, body)
	}
	go pumpStream(req.StreamID, opened.StreamID, isAnthropicFormat(req.Format))
	return okEnvelope(executorStreamResult{
		Headers: http.Header{"Content-Type": []string{contentType(opened.Headers, "text/event-stream")}},
	})
}

// drainStream reads up to limit bytes of an error body from a host HTTP stream.
func drainStream(streamID string, limit int) []byte {
	var out []byte
	for len(out) < limit {
		chunk, errRead := hostHTTPStreamRead(streamID)
		if errRead != nil {
			break
		}
		out = append(out, chunk.Payload...)
		if chunk.Done || chunk.Error != "" {
			break
		}
	}
	return out
}

// pumpStream copies the upstream stream to the client stream until either side
// finishes, framing chunks for the protocol (see streamFramer). It always
// closes both streams.
func pumpStream(clientStream, upstreamStream string, anthropic bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			closeExecutorStream(clientStream, pluginabi.NewError("plugin_panic", fmt.Sprintf("stream panic: %v", recovered)))
		}
	}()
	defer hostHTTPStreamClose(upstreamStream)
	framer := newStreamFramer(anthropic)
	emitAll := func(chunks [][]byte) bool {
		for _, payload := range chunks {
			if errEmit := emitStreamChunk(clientStream, payload); errEmit != nil {
				// The client disconnected; the deferred close releases the upstream.
				return false
			}
		}
		return true
	}
	fail := func(message string) {
		closeExecutorStream(clientStream, pluginabi.NewError("upstream_stream_error", message, http.StatusBadGateway))
	}
	for {
		chunk, errRead := hostHTTPStreamRead(upstreamStream)
		if errRead != nil {
			fail(errRead.Error())
			return
		}
		if len(chunk.Payload) > 0 {
			framed, errFrame := framer.feed(chunk.Payload)
			if errFrame != nil {
				fail(errFrame.Error())
				return
			}
			if !emitAll(framed) {
				return
			}
		}
		if chunk.Error != "" {
			fail(chunk.Error)
			return
		}
		if chunk.Done {
			if !emitAll(framer.flush()) {
				return
			}
			closeExecutorStream(clientStream, nil)
			return
		}
	}
}

// handleCountTokens uses Synthetic's Anthropic-compatible count_tokens endpoint.
// There is no OpenAI-style equivalent.
func handleCountTokens(request []byte) ([]byte, error) {
	var req executorRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode executor request: %w", errUnmarshal)
	}
	if !isAnthropicFormat(req.Format) {
		return nil, pluginabi.NewError("count_tokens_unsupported", "token counting is only available for the Anthropic Messages format", http.StatusNotImplemented)
	}
	call, errBuild := upstreamRequest(req, false, "/messages/count_tokens")
	if errBuild != nil {
		return nil, errBuild
	}
	// count_tokens takes no stream flag.
	var obj map[string]json.RawMessage
	if json.Unmarshal(call.Body, &obj) == nil {
		delete(obj, "stream")
		if rebuilt, errMarshal := json.Marshal(obj); errMarshal == nil {
			call.Body = rebuilt
		}
	}
	result, errDo := hostHTTPDo(call)
	if errDo != nil {
		return nil, pluginabi.NewError("upstream_unreachable", errDo.Error(), http.StatusBadGateway)
	}
	if result.Status < 200 || result.Status >= 300 {
		return nil, upstreamError(result.Status, result.Header, result.Body)
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: result.Body,
		Headers: http.Header{"Content-Type": []string{contentType(result.Header, "application/json")}},
	})
}

// handleExecutorHTTPRequest is the host's raw authenticated HTTP bridge. It
// signs requests for the configured Synthetic origin and refuses every other
// target.
func handleExecutorHTTPRequest(request []byte) ([]byte, error) {
	var req executorHTTPRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode executor http request: %w", errUnmarshal)
	}
	if !cfg().sameOrigin(req.URL) {
		return nil, pluginabi.NewError("forbidden_target", "synthetic executor only sends requests to "+cfg().BaseURL, http.StatusForbidden)
	}
	acct, errKey := requireKey(req.StorageJSON)
	if errKey != nil {
		return nil, errKey
	}
	header := http.Header{}
	for _, name := range []string{"Content-Type", "Accept", "Anthropic-Version", "Anthropic-Beta"} {
		if v := req.Headers.Get(name); v != "" {
			header.Set(name, v)
		}
	}
	header.Set("Authorization", "Bearer "+acct.APIKey)
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	result, errDo := hostHTTPDo(httpCall{
		CallbackID: req.HostCallbackID,
		Method:     method,
		URL:        req.URL,
		Header:     header,
		Body:       req.Body,
	})
	if errDo != nil {
		return nil, pluginabi.NewError("upstream_unreachable", errDo.Error(), http.StatusBadGateway)
	}
	return okEnvelope(pluginapi.ExecutorHTTPResponse{StatusCode: result.Status, Headers: result.Header, Body: result.Body})
}
