package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// httpCall describes one outbound request made through the host HTTP bridge.
// The host owns the real transport, so proxy settings, request logging and
// cancellation keep working.
type httpCall struct {
	CallbackID string
	Method     string
	URL        string
	Header     http.Header
	Body       []byte
}

type httpResult struct {
	Status int
	Header http.Header
	Body   []byte
}

type hostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method"`
	URL            string      `json:"url"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

func (c httpCall) payload() hostHTTPRequest {
	return hostHTTPRequest{
		HostCallbackID: c.CallbackID,
		Method:         c.Method,
		URL:            c.URL,
		Headers:        c.Header,
		Body:           c.Body,
	}
}

// hostHTTPDo performs a buffered request through host.http.do.
func hostHTTPDo(call httpCall) (httpResult, error) {
	raw, errCall := host.Call(pluginabi.MethodHostHTTPDo, call.payload())
	if errCall != nil {
		return httpResult{}, fmt.Errorf("host http request failed: %w", errCall)
	}
	var resp pluginapi.HTTPResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return httpResult{}, fmt.Errorf("decode host http response: %w", errUnmarshal)
	}
	return httpResult{Status: resp.StatusCode, Header: resp.Headers, Body: resp.Body}, nil
}

type hostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type hostStreamRead struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type streamIDRequest struct {
	StreamID string `json:"stream_id"`
}

// hostHTTPDoStream opens a streaming request. The returned response carries
// the upstream status and headers; the body is read with hostHTTPStreamRead.
func hostHTTPDoStream(call httpCall) (hostHTTPStreamResponse, error) {
	raw, errCall := host.Call(pluginabi.MethodHostHTTPDoStream, call.payload())
	if errCall != nil {
		return hostHTTPStreamResponse{}, fmt.Errorf("host http stream request failed: %w", errCall)
	}
	var resp hostHTTPStreamResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return hostHTTPStreamResponse{}, fmt.Errorf("decode host http stream response: %w", errUnmarshal)
	}
	if resp.StreamID == "" {
		return hostHTTPStreamResponse{}, fmt.Errorf("host http stream response has no stream id")
	}
	return resp, nil
}

func hostHTTPStreamRead(streamID string) (hostStreamRead, error) {
	raw, errCall := host.Call(pluginabi.MethodHostHTTPStreamRead, streamIDRequest{StreamID: streamID})
	if errCall != nil {
		return hostStreamRead{}, fmt.Errorf("host http stream read failed: %w", errCall)
	}
	var chunk hostStreamRead
	if errUnmarshal := json.Unmarshal(raw, &chunk); errUnmarshal != nil {
		return hostStreamRead{}, fmt.Errorf("decode host http stream chunk: %w", errUnmarshal)
	}
	return chunk, nil
}

func hostHTTPStreamClose(streamID string) {
	_, _ = host.Call(pluginabi.MethodHostHTTPStreamClose, streamIDRequest{StreamID: streamID})
}

type streamEmitRequest struct {
	StreamID    string           `json:"stream_id"`
	Payload     []byte           `json:"payload,omitempty"`
	Error       string           `json:"error,omitempty"`
	ErrorDetail *pluginabi.Error `json:"error_detail,omitempty"`
}

// emitStreamChunk forwards one payload to the client through the host's
// executor stream. It fails once the client has gone away.
func emitStreamChunk(streamID string, payload []byte) error {
	_, errCall := host.Call(pluginabi.MethodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: payload})
	return errCall
}

// closeExecutorStream ends the client stream, optionally with an error.
func closeExecutorStream(streamID string, detail *pluginabi.Error) {
	req := streamEmitRequest{StreamID: streamID}
	if detail != nil {
		req.Error = detail.Message
		req.ErrorDetail = detail
	}
	_, _ = host.Call(pluginabi.MethodHostStreamClose, req)
}

// authFile is the metadata of one credential known to the host.
type authFile = pluginapi.HostAuthFileEntry

func hostAuthList() ([]authFile, error) {
	raw, errCall := host.Call(pluginabi.MethodHostAuthList, map[string]any{})
	if errCall != nil {
		return nil, fmt.Errorf("list auth files: %w", errCall)
	}
	var resp struct {
		Files []authFile `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth list: %w", errUnmarshal)
	}
	return resp.Files, nil
}

// hostAuthJSON returns the raw credential file for an auth index.
func hostAuthJSON(authIndex string) ([]byte, error) {
	raw, errCall := host.Call(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if errCall != nil {
		return nil, fmt.Errorf("read auth file: %w", errCall)
	}
	var resp pluginapi.HostAuthGetResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth file: %w", errUnmarshal)
	}
	return resp.JSON, nil
}

func hostAuthSave(name string, body []byte) (pluginapi.HostAuthSaveResponse, error) {
	raw, errCall := host.Call(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: body})
	if errCall != nil {
		return pluginapi.HostAuthSaveResponse{}, fmt.Errorf("save auth file: %w", errCall)
	}
	var resp pluginapi.HostAuthSaveResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return pluginapi.HostAuthSaveResponse{}, fmt.Errorf("decode auth save response: %w", errUnmarshal)
	}
	return resp, nil
}

// upstreamError converts a non-2xx Synthetic response into a plugin error that
// carries the HTTP status (and Retry-After) so the host can surface it.
func upstreamError(status int, header http.Header, body []byte) *pluginabi.Error {
	detail := pluginabi.NewError("upstream_error", fmt.Sprintf("synthetic returned HTTP %d: %s", status, errorSnippet(body)), status)
	detail.Retryable = status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
	if seconds, errParse := strconv.Atoi(strings.TrimSpace(header.Get("Retry-After"))); errParse == nil && seconds > 0 {
		detail.RetryAfterSeconds = seconds
	}
	return detail
}

// errorSnippet extracts a short human message from an upstream error body.
func errorSnippet(body []byte) string {
	var shaped struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(body, &shaped) == nil {
		switch e := shaped.Error.(type) {
		case string:
			if e != "" {
				return truncate(e, 300)
			}
		case map[string]any:
			if msg, ok := e["message"].(string); ok && msg != "" {
				return truncate(msg, 300)
			}
		}
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "empty response body"
	}
	return truncate(text, 300)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
