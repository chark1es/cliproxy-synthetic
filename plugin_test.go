package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const testKey = "synthetic_test_key_ABCDWXYZ"

type fakeCall struct {
	Method  string
	Payload json.RawMessage
}

type fakeHost struct {
	mu      sync.Mutex
	calls   []fakeCall
	handler func(method string, payload json.RawMessage) (any, error)
}

func (f *fakeHost) Call(method string, payload any) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Method: method, Payload: raw})
	f.mu.Unlock()
	v, err := f.handler(method, raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func (f *fakeHost) callsFor(method string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func useHost(t *testing.T, handler func(method string, payload json.RawMessage) (any, error)) *fakeHost {
	t.Helper()
	f := &fakeHost{handler: handler}
	prev := host
	host = f
	prevCfg := currentConfig.Load()
	currentConfig.Store(defaultConfig())
	modelsMu.Lock()
	modelsCache = map[string]modelsCacheEntry{}
	modelsMu.Unlock()
	t.Cleanup(func() {
		host = prev
		currentConfig.Store(prevCfg)
	})
	return f
}

func decode(t *testing.T, raw []byte, err error) (json.RawMessage, *pluginabi.Error) {
	t.Helper()
	if err != nil {
		var pe *pluginabi.Error
		if errors.As(err, &pe) {
			return nil, pe
		}
		t.Fatalf("unexpected non-plugin error: %v", err)
	}
	var env pluginabi.Envelope
	if e := json.Unmarshal(raw, &env); e != nil {
		t.Fatalf("bad envelope %q: %v", raw, e)
	}
	if !env.OK {
		return nil, env.Error
	}
	return env.Result, nil
}

func storage() []byte {
	b, _ := json.Marshal(map[string]string{"type": "synthetic", "api_key": testKey})
	return b
}

func httpResponse(status int, body string, headers http.Header) pluginapi.HTTPResponse {
	return pluginapi.HTTPResponse{StatusCode: status, Headers: headers, Body: []byte(body)}
}

func TestRegister(t *testing.T) {
	useHost(t, nil)
	req, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte("enabled: true\npriority: 3\nbase_url: https://example.test/\nmodels_cache_seconds: 9\n"), SchemaVersion: 4})
	raw, err := handleMethod(pluginabi.MethodPluginRegister, req)
	result, perr := decode(t, raw, err)
	if perr != nil {
		t.Fatalf("register failed: %v", perr)
	}
	var reg registration
	if err := json.Unmarshal(result, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.SchemaVersion != 4 {
		t.Errorf("schema = %d, want clamped to host's 4", reg.SchemaVersion)
	}
	c := reg.Capabilities
	if !c.AuthProvider || !c.Executor || !c.ModelProvider || !c.QuotaProvider || !c.ManagementAPI {
		t.Errorf("missing capability: %+v", c)
	}
	if got := cfg().BaseURL; got != "https://example.test" {
		t.Errorf("base url = %q", got)
	}
	if cfg().ModelsCacheSeconds != 9 {
		t.Errorf("cache seconds = %d", cfg().ModelsCacheSeconds)
	}
}

func TestParseConfig(t *testing.T) {
	c, err := parseConfig(nil)
	if err != nil || c.BaseURL != defaultBaseURL || c.ModelsCacheSeconds != defaultModelsCacheSeconds {
		t.Fatalf("defaults wrong: %+v %v", c, err)
	}
	for _, bad := range []string{"base_url: ftp://x", "base_url: not a url", "base_url: https://u:p@host", "base_url: https://h/?q=1"} {
		if _, err := parseConfig([]byte(bad)); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	c := defaultConfig()
	if !c.sameOrigin("https://api.synthetic.new/v2/quotas") {
		t.Error("same origin rejected")
	}
	for _, u := range []string{"https://evil.test/", "http://api.synthetic.new/x", "https://api.synthetic.new.evil.test/", "https://user@api.synthetic.new/"} {
		if c.sameOrigin(u) {
			t.Errorf("%s accepted", u)
		}
	}
}

func TestAuthParse(t *testing.T) {
	parse := func(req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, *pluginabi.Error) {
		body, _ := json.Marshal(req)
		raw, err := handleMethod(pluginabi.MethodAuthParse, body)
		result, perr := decode(t, raw, err)
		var resp pluginapi.AuthParseResponse
		if perr == nil {
			if e := json.Unmarshal(result, &resp); e != nil {
				t.Fatal(e)
			}
		}
		return resp, perr
	}

	resp, perr := parse(pluginapi.AuthParseRequest{Provider: "synthetic", FileName: "synthetic-main.json", RawJSON: []byte(`{"type":"Synthetic","api_key":"k1","label":"Main"}`)})
	if perr != nil || !resp.Handled || resp.Auth.Provider != "synthetic" || resp.Auth.Label != "Main" {
		t.Fatalf("synthetic file not handled: %+v %v", resp, perr)
	}
	if string(resp.Auth.StorageJSON) != `{"type":"Synthetic","api_key":"k1","label":"Main"}` {
		t.Errorf("storage json not preserved: %s", resp.Auth.StorageJSON)
	}

	resp, _ = parse(pluginapi.AuthParseRequest{FileName: "x.json", RawJSON: []byte(`{"type":"synthetic","apiKey":"alt"}`)})
	if !resp.Handled || resp.Auth.Label != "x" {
		t.Errorf("alt key name / label fallback failed: %+v", resp)
	}

	if resp, _ = parse(pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"codex","api_key":"k"}`)}); resp.Handled {
		t.Error("handled a codex file")
	}
	if resp, _ = parse(pluginapi.AuthParseRequest{Provider: "codex", RawJSON: []byte(`{"type":"synthetic","api_key":"k"}`)}); resp.Handled {
		t.Error("handled with a different provider hint")
	}
	if resp, _ = parse(pluginapi.AuthParseRequest{RawJSON: []byte(`not json`)}); resp.Handled {
		t.Error("handled invalid json")
	}
	if _, perr = parse(pluginapi.AuthParseRequest{FileName: "empty.json", RawJSON: []byte(`{"type":"synthetic"}`)}); perr == nil {
		t.Error("missing api_key should error")
	}
	if resp, _ = parse(pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"synthetic","api_key":"k","disabled":true}`)}); !resp.Auth.Disabled {
		t.Error("disabled flag lost")
	}
}

func TestAccountFileNameAndMask(t *testing.T) {
	if got := accountFileName("My Work Key!", "k"); got != "synthetic-my-work-key.json" {
		t.Errorf("name = %q", got)
	}
	a, b := accountFileName("", "key-one"), accountFileName("", "key-two")
	if a == b || !strings.HasPrefix(a, "synthetic-key-") {
		t.Errorf("hash names: %q %q", a, b)
	}
	if strings.Contains(a, "key-one") {
		t.Error("name leaks key")
	}
	if got := maskKey(testKey); got != "...WXYZ" || strings.Contains(got, "ABCD") {
		t.Errorf("mask = %q", got)
	}
	if got := maskKey("short"); got != "*****" {
		t.Errorf("short mask = %q", got)
	}
}

func TestPreparePayload(t *testing.T) {
	in := []byte(`{"model":"x","messages":[{"role":"user","content":"a<b & c"}],"temperature":0.2,"stream":false}`)
	out, err := preparePayload(in, "syn:small:text(high)", true, false)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "syn:small:text" || obj["stream"] != true || obj["temperature"] != 0.2 {
		t.Errorf("fields wrong: %s", out)
	}
	if so, _ := obj["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("include_usage missing: %s", out)
	}
	if !strings.Contains(string(out), "a<b & c") {
		t.Errorf("HTML was escaped: %s", out)
	}

	out, _ = preparePayload(in, "m", true, true)
	if strings.Contains(string(out), "stream_options") {
		t.Errorf("anthropic payload got stream_options: %s", out)
	}
	out, _ = preparePayload([]byte(`{"messages":[]}`), "m", false, false)
	if !strings.Contains(string(out), `"stream":false`) || strings.Contains(string(out), "stream_options") {
		t.Errorf("non-stream payload wrong: %s", out)
	}
	if _, err := preparePayload([]byte(`[1]`), "m", false, false); err == nil {
		t.Error("array payload accepted")
	}
}

func executeRequest(format string, stream bool) []byte {
	req := executorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthProvider: "synthetic",
			Model:        "hf:zai-org/GLM-5.3",
			Format:       format,
			Stream:       stream,
			Headers:      http.Header{"Authorization": {"Bearer CLIENT-PROXY-KEY"}, "Anthropic-Beta": {"tools-2024"}, "X-Secret": {"nope"}},
			Payload:      []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			StorageJSON:  storage(),
		},
		StreamID:       "stream-1",
		HostCallbackID: "cb-1",
	}
	raw, _ := json.Marshal(req)
	return raw
}

func TestExecuteOpenAIAndHeaders(t *testing.T) {
	f := useHost(t, func(method string, payload json.RawMessage) (any, error) {
		return httpResponse(200, `{"id":"chatcmpl-1"}`, http.Header{"Content-Type": {"application/json"}}), nil
	})
	raw, err := handleMethod(pluginabi.MethodExecutorExecute, executeRequest("openai", false))
	result, perr := decode(t, raw, err)
	if perr != nil {
		t.Fatalf("execute failed: %v", perr)
	}
	var resp pluginapi.ExecutorResponse
	if err := json.Unmarshal(result, &resp); err != nil || string(resp.Payload) != `{"id":"chatcmpl-1"}` {
		t.Fatalf("payload wrong: %s %v", result, err)
	}
	calls := f.callsFor(pluginabi.MethodHostHTTPDo)
	if len(calls) != 1 {
		t.Fatalf("expected one http call, got %d", len(calls))
	}
	var sent hostHTTPRequest
	_ = json.Unmarshal(calls[0].Payload, &sent)
	if sent.URL != "https://api.synthetic.new/openai/v1/chat/completions" || sent.HostCallbackID != "cb-1" {
		t.Errorf("request wrong: %+v", sent)
	}
	if got := sent.Headers.Get("Authorization"); got != "Bearer "+testKey {
		t.Errorf("Authorization = %q; the client's proxy key must never be forwarded", got)
	}
	if sent.Headers.Get("X-Secret") != "" || sent.Headers.Get("Anthropic-Beta") != "" {
		t.Errorf("unexpected forwarded headers: %v", sent.Headers)
	}
}

func TestExecuteAnthropicFormat(t *testing.T) {
	f := useHost(t, func(method string, payload json.RawMessage) (any, error) {
		return httpResponse(200, `{"type":"message"}`, nil), nil
	})
	raw, err := handleMethod(pluginabi.MethodExecutorExecute, executeRequest("claude", false))
	if _, perr := decode(t, raw, err); perr != nil {
		t.Fatal(perr)
	}
	var sent hostHTTPRequest
	_ = json.Unmarshal(f.callsFor(pluginabi.MethodHostHTTPDo)[0].Payload, &sent)
	if sent.URL != "https://api.synthetic.new/anthropic/v1/messages" {
		t.Errorf("url = %s", sent.URL)
	}
	if sent.Headers.Get("Anthropic-Version") != "2023-06-01" || sent.Headers.Get("Anthropic-Beta") != "tools-2024" {
		t.Errorf("anthropic headers wrong: %v", sent.Headers)
	}
}

func TestExecuteUpstreamErrors(t *testing.T) {
	useHost(t, func(method string, payload json.RawMessage) (any, error) {
		return httpResponse(429, `{"error":{"message":"slow down"}}`, http.Header{"Retry-After": {"7"}}), nil
	})
	_, err := handleMethod(pluginabi.MethodExecutorExecute, executeRequest("openai", false))
	_, perr := decode(t, nil, err)
	if perr == nil || perr.HTTPStatus != 429 || perr.RetryAfterSeconds != 7 || !perr.Retryable || !strings.Contains(perr.Message, "slow down") {
		t.Errorf("error not mapped: %+v", perr)
	}

	useHost(t, func(method string, payload json.RawMessage) (any, error) { return nil, errors.New("dial failed") })
	_, err = handleMethod(pluginabi.MethodExecutorExecute, executeRequest("openai", false))
	if _, perr = decode(t, nil, err); perr == nil || perr.HTTPStatus != http.StatusBadGateway {
		t.Errorf("transport failure should be 502: %+v", perr)
	}

	useHost(t, nil)
	req := executeRequest("openai", false)
	req = []byte(strings.Replace(string(req), "StorageJSON", "Ignored", 1))
	_, err = handleMethod(pluginabi.MethodExecutorExecute, req)
	if _, perr = decode(t, nil, err); perr == nil || perr.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("missing key should be 401: %+v", perr)
	}
}

func TestExecuteStreamPumpsChunks(t *testing.T) {
	closed := make(chan struct{})
	var mu sync.Mutex
	chunks := []hostStreamRead{
		{Payload: []byte(": keep-alive\n\ndata: {\"n\":")},
		{Payload: []byte("1}\n\ndata: {\"n\":2}\n\ndata: [DONE]\n\n")},
		{Done: true},
	}
	f := useHost(t, func(method string, payload json.RawMessage) (any, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			return hostHTTPStreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, StreamID: "up-1"}, nil
		case pluginabi.MethodHostHTTPStreamRead:
			mu.Lock()
			defer mu.Unlock()
			c := chunks[0]
			chunks = chunks[1:]
			return c, nil
		case pluginabi.MethodHostStreamClose:
			close(closed)
		}
		return struct{}{}, nil
	})
	raw, err := handleMethod(pluginabi.MethodExecutorExecuteStream, executeRequest("openai", true))
	result, perr := decode(t, raw, err)
	if perr != nil {
		t.Fatal(perr)
	}
	var out executorStreamResult
	_ = json.Unmarshal(result, &out)
	if out.Headers.Get("Content-Type") != "text/event-stream" {
		t.Errorf("stream headers = %v", out.Headers)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("client stream was never closed")
	}
	emits := f.callsFor(pluginabi.MethodHostStreamEmit)
	if len(emits) != 2 {
		t.Fatalf("emitted %d chunks, want 2 (bare payloads; [DONE] and comments dropped)", len(emits))
	}
	var first, second streamEmitRequest
	_ = json.Unmarshal(emits[0].Payload, &first)
	_ = json.Unmarshal(emits[1].Payload, &second)
	if first.StreamID != "stream-1" || string(first.Payload) != `{"n":1}` || string(second.Payload) != `{"n":2}` {
		t.Errorf("emits wrong (the host adds data: framing itself): %+v %+v", first, second)
	}
	var sent hostHTTPRequest
	_ = json.Unmarshal(f.callsFor(pluginabi.MethodHostHTTPDoStream)[0].Payload, &sent)
	if !strings.Contains(string(sent.Body), `"include_usage":true`) || sent.Headers.Get("Accept") != "text/event-stream" {
		t.Errorf("stream request wrong: %s %v", sent.Body, sent.Headers)
	}
	// The upstream stream must be released.
	deadline := time.Now().Add(2 * time.Second)
	for len(f.callsFor(pluginabi.MethodHostHTTPStreamClose)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(f.callsFor(pluginabi.MethodHostHTTPStreamClose)) == 0 {
		t.Error("upstream stream not closed")
	}
}

func TestExecuteStreamUpstreamError(t *testing.T) {
	useHost(t, func(method string, payload json.RawMessage) (any, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			return hostHTTPStreamResponse{StatusCode: 401, StreamID: "up-1"}, nil
		case pluginabi.MethodHostHTTPStreamRead:
			return hostStreamRead{Payload: []byte(`{"error":"bad key"}`), Done: true}, nil
		}
		return struct{}{}, nil
	})
	_, err := handleMethod(pluginabi.MethodExecutorExecuteStream, executeRequest("openai", true))
	_, perr := decode(t, nil, err)
	if perr == nil || perr.HTTPStatus != 401 || !strings.Contains(perr.Message, "bad key") {
		t.Errorf("stream error not surfaced synchronously: %+v", perr)
	}
}

func TestStreamStopsWhenClientDisconnects(t *testing.T) {
	var mu sync.Mutex
	reads := 0
	f := useHost(t, func(method string, payload json.RawMessage) (any, error) {
		switch method {
		case pluginabi.MethodHostStreamEmit:
			return nil, errors.New("stream is not open")
		case pluginabi.MethodHostHTTPStreamRead:
			mu.Lock()
			reads++
			mu.Unlock()
			return hostStreamRead{Payload: []byte("data: {\"x\":1}\n\n")}, nil
		}
		return struct{}{}, nil
	})
	pumpStream("c", "u", false)
	mu.Lock()
	defer mu.Unlock()
	if reads != 1 || len(f.callsFor(pluginabi.MethodHostHTTPStreamClose)) != 1 {
		t.Errorf("pump kept reading after disconnect: reads=%d", reads)
	}
}

func TestCountTokens(t *testing.T) {
	f := useHost(t, func(method string, payload json.RawMessage) (any, error) {
		return httpResponse(200, `{"input_tokens":5}`, nil), nil
	})
	raw, err := handleMethod(pluginabi.MethodExecutorCountTokens, executeRequest("claude", false))
	if _, perr := decode(t, raw, err); perr != nil {
		t.Fatal(perr)
	}
	var sent hostHTTPRequest
	_ = json.Unmarshal(f.callsFor(pluginabi.MethodHostHTTPDo)[0].Payload, &sent)
	if !strings.HasSuffix(sent.URL, "/anthropic/v1/messages/count_tokens") || strings.Contains(string(sent.Body), `"stream"`) {
		t.Errorf("count_tokens request wrong: %s %s", sent.URL, sent.Body)
	}
	_, err = handleMethod(pluginabi.MethodExecutorCountTokens, executeRequest("openai", false))
	if _, perr := decode(t, nil, err); perr == nil || perr.HTTPStatus != http.StatusNotImplemented {
		t.Errorf("openai count_tokens should be 501: %+v", perr)
	}
}

func TestExecutorHTTPRequestOriginGuard(t *testing.T) {
	f := useHost(t, func(method string, payload json.RawMessage) (any, error) {
		return httpResponse(200, `ok`, nil), nil
	})
	build := func(url string) []byte {
		raw, _ := json.Marshal(executorHTTPRequest{ExecutorHTTPRequest: pluginapi.ExecutorHTTPRequest{
			Method: "GET", URL: url, StorageJSON: storage(), Headers: http.Header{"Authorization": {"Bearer client"}},
		}})
		return raw
	}
	_, err := handleMethod(pluginabi.MethodExecutorHTTPRequest, build("https://evil.test/steal"))
	if _, perr := decode(t, nil, err); perr == nil || perr.HTTPStatus != http.StatusForbidden {
		t.Errorf("foreign origin should be refused: %+v", perr)
	}
	if len(f.callsFor(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Error("request to a foreign origin was sent")
	}
	raw, err := handleMethod(pluginabi.MethodExecutorHTTPRequest, build("https://api.synthetic.new/v2/quotas"))
	if _, perr := decode(t, raw, err); perr != nil {
		t.Fatal(perr)
	}
	var sent hostHTTPRequest
	_ = json.Unmarshal(f.callsFor(pluginabi.MethodHostHTTPDo)[0].Payload, &sent)
	if sent.Headers.Get("Authorization") != "Bearer "+testKey {
		t.Errorf("Authorization = %q", sent.Headers.Get("Authorization"))
	}
}

const realisticQuotas = `{"subscription":{"limit":500,"requests":125,"renewsAt":"2026-10-26T10:27:51.450Z"},"search":{"hourly":{"limit":250,"requests":50,"renewsAt":"2026-10-26T06:27:51.451Z"}},"planWeeklyLimit":{"nextRegenAt":"2026-10-26T08:08:05.000Z","percentRemaining":80,"nextRegenPercent":2}}`

func TestQuotaMapping(t *testing.T) {
	useHost(t, func(method string, payload json.RawMessage) (any, error) {
		return httpResponse(200, realisticQuotas, nil), nil
	})
	req, _ := json.Marshal(quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{Provider: "synthetic", StorageJSON: storage()}})
	raw, err := handleMethod(pluginabi.MethodQuotaFetch, req)
	result, perr := decode(t, raw, err)
	if perr != nil {
		t.Fatal(perr)
	}
	var resp pluginapi.QuotaFetchResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Groups) != 3 {
		t.Fatalf("groups = %d: %s", len(resp.Groups), result)
	}
	buckets := map[string]pluginapi.QuotaBucket{}
	for _, g := range resp.Groups {
		for _, b := range g.Buckets {
			buckets[b.Window] = b
		}
	}
	if b := buckets["subscription"]; b.RemainingFraction != 0.75 || b.ResetTime != "2026-10-26T10:27:51.450Z" {
		t.Errorf("subscription bucket = %+v", b)
	}
	if b := buckets["hourly"]; b.RemainingFraction != 0.8 {
		t.Errorf("hourly bucket = %+v", b)
	}
	if b := buckets["weekly"]; b.RemainingFraction != 0.8 || b.ResetTime != "2026-10-26T08:08:05.000Z" {
		t.Errorf("weekly bucket = %+v", b)
	}

	// Missing sections and a zero limit must not panic or divide by zero.
	r := quotaResponse(synthQuotas{Subscription: &quotaWindow{}})
	if len(r.Groups) != 0 {
		t.Errorf("zero-limit window produced a bucket: %+v", r.Groups)
	}
	if r := quotaResponse(synthQuotas{Subscription: &quotaWindow{Limit: 10, Requests: 99}}); r.Groups[0].Buckets[0].RemainingFraction != 0 {
		t.Error("over-limit usage should clamp to 0")
	}
}

const modelList = `{"data":[
 {"id":"syn:small:text","display_name":"GLM 4.7 Flash","created":1792195200,"context_length":196608,"max_output_length":65536,"input_modalities":["text"],"output_modalities":["text"],"supported_features":["tools","reasoning"],"supported_sampling_parameters":["temperature","top_p"]},
 {"id":"hf:zai-org/GLM-5.3","context_length":524288,"max_output_length":65536,"input_modalities":["text","image"],"output_modalities":["text"]},
 {"display_name":"no id"}]}`

func TestModelsForAuthAndCache(t *testing.T) {
	f := useHost(t, func(method string, payload json.RawMessage) (any, error) {
		return httpResponse(200, modelList, nil), nil
	})
	req, _ := json.Marshal(authModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{StorageJSON: storage()}, HostCallbackID: "cb"})
	for i := 0; i < 2; i++ {
		raw, err := handleMethod(pluginabi.MethodModelForAuth, req)
		result, perr := decode(t, raw, err)
		if perr != nil {
			t.Fatal(perr)
		}
		var resp pluginapi.ModelResponse
		if err := json.Unmarshal(result, &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Provider != "synthetic" || len(resp.Models) != 2 {
			t.Fatalf("models = %+v", resp)
		}
		m := resp.Models[0]
		if m.ID != "syn:small:text" || m.DisplayName != "GLM 4.7 Flash" || m.ContextLength != 196608 || m.OwnedBy != "synthetic" {
			t.Errorf("model mapped wrong: %+v", m)
		}
		joined := strings.Join(m.SupportedParameters, ",")
		for _, want := range []string{"temperature", "tools", "reasoning_effort"} {
			if !strings.Contains(joined, want) {
				t.Errorf("supported parameters %q missing %q", joined, want)
			}
		}
	}
	if n := len(f.callsFor(pluginabi.MethodHostHTTPDo)); n != 1 {
		t.Errorf("model list fetched %d times, want 1 (cached)", n)
	}
}

func TestModelsFallBackToStaleCache(t *testing.T) {
	fail := false
	useHost(t, func(method string, payload json.RawMessage) (any, error) {
		if fail {
			return nil, errors.New("offline")
		}
		return httpResponse(200, modelList, nil), nil
	})
	acct := account{APIKey: testKey}
	if _, err := fetchModels("", acct); err != nil {
		t.Fatal(err)
	}
	modelsMu.Lock()
	e := modelsCache[keyFingerprint(testKey)]
	e.fetched = time.Now().Add(-time.Hour)
	modelsCache[keyFingerprint(testKey)] = e
	modelsMu.Unlock()
	fail = true
	models, err := fetchModels("", acct)
	if err != nil || len(models) != 2 {
		t.Errorf("stale cache not used: %v %v", models, err)
	}
	if _, err := fetchModels("", account{APIKey: "other"}); err == nil {
		t.Error("an uncached key should surface the failure")
	}
}

func managementHandler(accounts map[string]string, onSave func(name string, body []byte)) func(string, json.RawMessage) (any, error) {
	files := []authFile{
		{AuthIndex: "idx1", Name: "synthetic-main.json", Provider: "synthetic", Label: "Main"},
		{AuthIndex: "idx2", Name: "synthetic-off.json", Provider: "synthetic", Disabled: true},
		{AuthIndex: "idx3", Name: "codex.json", Provider: "codex"},
	}
	return func(method string, payload json.RawMessage) (any, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return map[string]any{"files": files}, nil
		case pluginabi.MethodHostAuthGet:
			var req pluginapi.HostAuthGetRequest
			_ = json.Unmarshal(payload, &req)
			if key, ok := accounts[req.AuthIndex]; ok {
				b, _ := json.Marshal(map[string]string{"type": "synthetic", "api_key": key})
				return pluginapi.HostAuthGetResponse{AuthIndex: req.AuthIndex, JSON: b}, nil
			}
			return nil, errors.New("not found")
		case pluginabi.MethodHostAuthSave:
			var req pluginapi.HostAuthSaveRequest
			_ = json.Unmarshal(payload, &req)
			if onSave != nil {
				onSave(req.Name, req.JSON)
			}
			return pluginapi.HostAuthSaveResponse{Name: req.Name, Path: "/auth/" + req.Name}, nil
		case pluginabi.MethodHostHTTPDo:
			var req hostHTTPRequest
			_ = json.Unmarshal(payload, &req)
			switch {
			case strings.HasSuffix(req.URL, "/v2/quotas"):
				if req.Headers.Get("Authorization") == "Bearer bad-key" {
					return httpResponse(401, `{"error":"invalid key"}`, nil), nil
				}
				return httpResponse(200, realisticQuotas, nil), nil
			case strings.HasSuffix(req.URL, "/v2/search"):
				return httpResponse(200, `{"results":[{"url":"https://x","title":"t","text":"body","published":"2025-01-01T00:00:00.000Z"}],"key":"`+req.Headers.Get("Authorization")+`"}`, nil), nil
			}
		}
		return nil, errors.New("unexpected host call " + method)
	}
}

func manage(t *testing.T, method, path string, body any, query string) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, _ := json.Marshal(managementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: method, Path: "/v0/management" + path, Body: raw}})
	if query != "" {
		var m map[string]any
		_ = json.Unmarshal(req, &m)
		m["Query"] = map[string][]string{"auth_index": {query}}
		req, _ = json.Marshal(m)
	}
	out, err := handleMethod(pluginabi.MethodManagementHandle, req)
	result, perr := decode(t, out, err)
	if perr != nil {
		t.Fatalf("management failed: %v", perr)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(resp.Body, &parsed)
	return resp.StatusCode, parsed
}

func TestManagementRegister(t *testing.T) {
	useHost(t, nil)
	raw, err := handleMethod(pluginabi.MethodManagementRegister, nil)
	result, perr := decode(t, raw, err)
	if perr != nil {
		t.Fatal(perr)
	}
	var reg struct {
		Routes    []pluginapi.ManagementRoute `json:"routes"`
		Resources []any                       `json:"resources"`
	}
	_ = json.Unmarshal(result, &reg)
	if len(reg.Routes) != 4 || len(reg.Resources) != 0 {
		t.Errorf("routes=%d resources=%d", len(reg.Routes), len(reg.Resources))
	}
	for _, r := range reg.Routes {
		if r.Menu != "" || !strings.HasPrefix(r.Path, "/synthetic/") {
			t.Errorf("route %+v must be authenticated-only and under /synthetic", r)
		}
	}
}

func TestManagementAccountsAndQuotas(t *testing.T) {
	useHost(t, managementHandler(map[string]string{"idx1": testKey, "idx2": "disabled-key"}, nil))

	status, body := manage(t, "GET", "/synthetic/accounts", nil, "")
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	list := body["accounts"].([]any)
	if len(list) != 2 {
		t.Fatalf("accounts = %v (codex file must be excluded)", list)
	}
	if s, _ := json.Marshal(body); strings.Contains(string(s), "ABCD") || strings.Contains(string(s), testKey) {
		t.Errorf("full key leaked: %s", s)
	}

	status, body = manage(t, "GET", "/synthetic/quotas", nil, "")
	accts := body["accounts"].([]any)
	if status != 200 || len(accts) != 1 {
		t.Fatalf("quotas status=%d accounts=%v (disabled account must be skipped)", status, accts)
	}
	first := accts[0].(map[string]any)
	if first["quotas"] == nil || first["normalized"] == nil || first["error"] != nil {
		t.Errorf("quota entry = %v", first)
	}
	if status, _ = manage(t, "GET", "/synthetic/quotas", nil, "nope"); status != 404 {
		t.Errorf("unknown auth_index should 404, got %d", status)
	}
	if status, _ = manage(t, "GET", "/synthetic/unknown", nil, ""); status != 404 {
		t.Errorf("unknown route should 404, got %d", status)
	}
}

func TestManagementAddAccount(t *testing.T) {
	var savedName string
	var savedBody []byte
	useHost(t, managementHandler(nil, func(name string, body []byte) { savedName, savedBody = name, body }))

	status, body := manage(t, "POST", "/synthetic/accounts", map[string]string{"api_key": testKey, "label": "Work"}, "")
	if status != 201 || savedName != "synthetic-work.json" {
		t.Fatalf("status=%d saved=%q body=%v", status, savedName, body)
	}
	var stored map[string]string
	_ = json.Unmarshal(savedBody, &stored)
	if stored["type"] != "synthetic" || stored["api_key"] != testKey || stored["label"] != "Work" {
		t.Errorf("stored file = %v", stored)
	}
	if body["key_hint"] != "...WXYZ" {
		t.Errorf("response should only hint the key: %v", body)
	}

	savedName = ""
	if status, _ = manage(t, "POST", "/synthetic/accounts", map[string]string{"api_key": "bad-key"}, ""); status != 400 || savedName != "" {
		t.Errorf("invalid key should be rejected before saving: status=%d saved=%q", status, savedName)
	}
	if status, _ = manage(t, "POST", "/synthetic/accounts", map[string]string{"label": "x"}, ""); status != 400 {
		t.Errorf("missing key should 400, got %d", status)
	}
	if status, _ = manage(t, "POST", "/synthetic/accounts", map[string]string{"api_key": testKey, "name": "../evil"}, ""); status != 400 || savedName != "" {
		t.Errorf("path traversal name accepted: status=%d saved=%q", status, savedName)
	}
}

func TestManagementSearch(t *testing.T) {
	useHost(t, managementHandler(map[string]string{"idx1": testKey}, nil))
	status, body := manage(t, "POST", "/synthetic/search", map[string]string{"query": "cliproxy"}, "")
	if status != 200 {
		t.Fatalf("status %d: %v", status, body)
	}
	if body["key"] != "Bearer "+testKey || body["results"] == nil {
		t.Errorf("search did not use the account key / pass results through: %v", body)
	}
	if status, _ = manage(t, "POST", "/synthetic/search", map[string]string{"query": "  "}, ""); status != 400 {
		t.Errorf("blank query should 400, got %d", status)
	}
	if status, _ = manage(t, "POST", "/synthetic/search", map[string]string{"query": "q", "auth_index": "idx9"}, ""); status != 404 {
		t.Errorf("unknown account should 404, got %d", status)
	}
}

func TestPickAccountRotates(t *testing.T) {
	accts := []synthAccount{{File: authFile{AuthIndex: "a"}}, {File: authFile{AuthIndex: "b"}}}
	searchCursor.Store(0)
	var seen []string
	for i := 0; i < 4; i++ {
		a, _ := pickAccount(accts, "")
		seen = append(seen, a.File.AuthIndex)
	}
	if strings.Join(seen, "") != "abab" {
		t.Errorf("rotation = %v", seen)
	}
	if a, ok := pickAccount(accts, "b"); !ok || a.File.AuthIndex != "b" {
		t.Error("explicit auth_index ignored")
	}
}

func TestLoginIsExplainedNotSilentlyFailed(t *testing.T) {
	_, err := handleMethod(pluginabi.MethodAuthLoginStart, nil)
	if _, perr := decode(t, nil, err); perr == nil || !strings.Contains(perr.Message, "API key") {
		t.Errorf("login.start should explain how to add an account: %+v", perr)
	}
}

func TestUnknownMethodAndPanicRecovery(t *testing.T) {
	if _, err := handleMethod("nope.nope", nil); err == nil {
		t.Error("unknown method accepted")
	}
	raw, err := safeHandleMethod(pluginabi.MethodExecutorExecute, []byte(`{"Payload":`))
	if raw != nil || err == nil {
		t.Error("malformed json should return an error, not a result")
	}
	prev := host
	host = nil // a nil host would panic on first use
	defer func() { host = prev }()
	_, err = safeHandleMethod(pluginabi.MethodHostAuthList, nil)
	if err == nil {
		t.Error("expected an error from an unknown method")
	}
	raw, err = safeHandleMethod(pluginabi.MethodQuotaFetch, mustJSON(quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{StorageJSON: storage()}}))
	if raw != nil || err == nil || !strings.Contains(err.Error(), "panic") {
		t.Errorf("panic was not converted to an error: raw=%s err=%v", raw, err)
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func feedAll(t *testing.T, anthropic bool, parts ...string) []string {
	t.Helper()
	f := newStreamFramer(anthropic)
	var out []string
	for _, p := range parts {
		chunks, err := f.feed([]byte(p))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range chunks {
			out = append(out, string(c))
		}
	}
	for _, c := range f.flush() {
		out = append(out, string(c))
	}
	return out
}

func TestStreamFramerOpenAI(t *testing.T) {
	got := feedAll(t, false, "data: {\"a\":1}\r\n\r\n: ping\n", "data: {\"b\"", ":2}\n\nevent: x\nid: 7\n", "data: [DONE]\n\ndata: {\"tail\":true}")
	want := []string{`{"a":1}`, `{"b":2}`, `{"tail":true}`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q want %q", got, want)
	}
	if got := feedAll(t, false, "{\"bare\":1}\n"); len(got) != 1 || got[0] != `{"bare":1}` {
		t.Errorf("bare JSON line = %q", got)
	}
	if got := feedAll(t, false, "data:\n\n"); len(got) != 0 {
		t.Errorf("empty data line produced %q", got)
	}
}

func TestStreamFramerAnthropicKeepsLineAlignedRawSSE(t *testing.T) {
	got := feedAll(t, true, "event: message_start\ndata: {\"t\":1}\n\neve", "nt: ping\ndata: {}\n\n")
	if len(got) != 2 || got[0] != "event: message_start\ndata: {\"t\":1}\n\n" || got[1] != "event: ping\ndata: {}\n\n" {
		t.Errorf("anthropic chunks = %q", got)
	}
	if joined := strings.Join(got, ""); joined != "event: message_start\ndata: {\"t\":1}\n\nevent: ping\ndata: {}\n\n" {
		t.Errorf("anthropic bytes were altered: %q", joined)
	}
}

func TestStreamFramerRejectsRunawayLine(t *testing.T) {
	f := newStreamFramer(false)
	big := make([]byte, maxPendingLine+1)
	for i := range big {
		big[i] = 'a'
	}
	if _, err := f.feed(big); err == nil {
		t.Error("unbounded unterminated line accepted")
	}
}
