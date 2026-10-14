package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func structuredBackendFor(server *httptest.Server) *structuredChatBackend {
	return &structuredChatBackend{URL: server.URL + "/v1/chat/completions", APIKey: "operator-test-key", Model: "configured-model"}
}
func structuredRequest(body string) *http.Request {
	r := chatRequest("POST", "/v1/chat/completions", body)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer client-value-must-not-be-forwarded")
	return r
}

func TestStructuredToolsAndResultsArePreserved(t *testing.T) {
	var calls atomic.Int32
	messages := []any{
		map[string]any{"role": "system", "content": "Be concise"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"filePath":"fixture.txt"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "fixture marker"},
	}
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "description": "Read a file on the client", "parameters": map[string]any{"type": "object", "properties": map[string]any{"filePath": map[string]any{"type": "string"}}, "required": []any{"filePath"}}}}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer operator-test-key" {
			t.Error("wrong upstream credentials")
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(got["messages"], messages) || !reflect.DeepEqual(got["tools"], tools) {
			t.Errorf("structured request changed: %#v", got)
		}
		if got["model"] != "configured-model" || got["max_tokens"] != float64(500) || got["n"] != float64(1) || got["tool_choice"] != "auto" || got["parallel_tool_calls"] != true {
			t.Errorf("bad settings: %#v", got)
		}
		io.WriteString(w, `{"id":"result-1","object":"chat.completion","model":"configured-model","choices":[{"index":0,"message":{"role":"assistant","content":"marker received"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`)
	}))
	defer upstream.Close()
	body, _ := json.Marshal(map[string]any{"messages": messages, "tools": tools, "tool_choice": "auto", "parallel_tool_calls": true, "model": "unapproved-expensive-model", "max_tokens": 9000})
	w := httptest.NewRecorder()
	handleStructuredChat(w, structuredRequest(string(body)), structuredBackendFor(upstream))
	if w.Code != 200 || calls.Load() != 1 || !strings.Contains(w.Body.String(), `"total_tokens":13`) || !strings.Contains(w.Body.String(), "marker received") {
		t.Fatal(w.Code, w.Body.String(), calls.Load())
	}
}

func TestStructuredToolArgumentStreamAndUsage(t *testing.T) {
	var calls atomic.Int32
	frames := []string{
		`{"id":"stream-1","object":"chat.completion.chunk","created":1,"model":"configured-model","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"stream-1","object":"chat.completion.chunk","created":1,"model":"configured-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"file"}}]},"finish_reason":null}]}`,
		`{"id":"stream-1","object":"chat.completion.chunk","created":1,"model":"configured-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"Path\":\"fixture.txt\"}"}}]},"finish_reason":null}]}`,
		`{"id":"stream-1","object":"chat.completion.chunk","created":1,"model":"configured-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"stream-1","object":"chat.completion.chunk","created":1,"model":"configured-model","choices":[],"usage":{"prompt_tokens":8,"completion_tokens":5,"total_tokens":13}}`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request map[string]any
		json.NewDecoder(r.Body).Decode(&request)
		if request["stream"] != true || request["stream_options"].(map[string]any)["include_usage"] != true {
			t.Error("stream options lost")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			io.WriteString(w, "data: "+frame+"\n\n")
			w.(http.Flusher).Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	w := httptest.NewRecorder()
	handleStructuredChat(w, structuredRequest(`{"messages":[{"role":"user","content":"read fixture"}],"stream":true,"stream_options":{"include_usage":true}}`), structuredBackendFor(upstream))
	want := ""
	for _, frame := range frames {
		want += "data: " + frame + "\n\n"
	}
	want += "data: [DONE]\n\n"
	if w.Code != 200 || w.Body.String() != want || calls.Load() != 1 {
		t.Fatal(w.Code, w.Body.String(), calls.Load())
	}
}

func TestStructuredNonstreamToolCallAndLengthReason(t *testing.T) {
	response := `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"read","arguments":"{\"filePath\":\"fixture.txt\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"total_tokens":10}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, response) }))
	defer upstream.Close()
	for _, reason := range []string{"tool_calls", "length"} {
		response = strings.ReplaceAll(response, "\"finish_reason\":\"tool_calls\"", "\"finish_reason\":\""+reason+"\"")
		w := httptest.NewRecorder()
		handleStructuredChat(w, structuredRequest(`{"messages":[{"role":"user","content":"hello"}]}`), structuredBackendFor(upstream))
		if w.Code != 200 || w.Body.String() != response {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestStructuredRejectsBeforeCallingProvider(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	for _, body := range []string{`null`, `[]`, `{}`, `{"messages":[]}`, `{"messages":[{}],"stream":"yes"}`, `{"messages":[{}],"n":2}`, `{"messages":[{}],"max_tokens":-1}`, strings.Repeat("x", structuredRequestLimit+1)} {
		w := httptest.NewRecorder()
		handleStructuredChat(w, structuredRequest(body), structuredBackendFor(upstream))
		if w.Code < 400 || calls.Load() != 0 {
			t.Fatal(w.Code, calls.Load())
		}
	}
}

func TestStructuredErrorsDoNotLeakProviderDetails(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		stream bool
	}{
		{401, "private-provider-url-and-key", false},
		{200, `{"error":{"message":"private-provider-url-and-key"}}`, false},
		{200, "data: {\"error\":{\"message\":\"private-provider-url-and-key\"}}\n\n", true},
		{200, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n", true},
	} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) }))
		body, _ := json.Marshal(map[string]any{"messages": []any{map[string]string{"role": "user", "content": "hello"}}, "stream": tc.stream})
		w := httptest.NewRecorder()
		handleStructuredChat(w, structuredRequest(string(body)), structuredBackendFor(upstream))
		upstream.Close()
		if strings.Contains(w.Body.String(), "private-provider") || !strings.Contains(w.Body.String(), "upstream_error") || strings.Contains(w.Body.String(), `"finish_reason":"stop"`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestStructuredCancellationAndNoRedirect(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	r := structuredRequest(`{"messages":[{"role":"user","content":"hello"}]}`).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleStructuredChat(httptest.NewRecorder(), r, structuredBackendFor(upstream))
	}()
	<-started
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream not canceled")
	}
	<-done
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 307) }))
	defer redirect.Close()
	w := httptest.NewRecorder()
	handleStructuredChat(w, structuredRequest(`{"messages":[{"role":"user","content":"hello"}]}`), structuredBackendFor(redirect))
	if w.Code != 502 || redirected.Load() != 0 {
		t.Fatal("followed upstream redirect", w.Code, redirected.Load())
	}
}

func TestConfiguredPublicChatRouteUsesStructuredBackend(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"id":"configured","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"tool-1","type":"function","function":{"name":"get_time","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer upstream.Close()
	t.Setenv("CHAT_API_URL", upstream.URL)
	t.Setenv("CHAT_API_KEY", "")
	t.Setenv("CHAT_MODEL", "configured-model")
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)
	server := httptest.NewServer(mux)
	defer server.Close()
	response, err := server.Client().Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"time?"}],"tools":[{"type":"function","function":{"name":"get_time","parameters":{"type":"object","properties":{}}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || calls.Load() != 1 || !strings.Contains(string(body), `"finish_reason":"tool_calls"`) {
		t.Fatal(response.StatusCode, string(body), err, calls.Load())
	}
}

func TestStructuredEnvironmentConfiguration(t *testing.T) {
	t.Setenv("CHAT_API_URL", "http://127.0.0.1:8080/v1/chat/completions")
	t.Setenv("CHAT_API_KEY", "local-test-key")
	t.Setenv("CHAT_MODEL", "local-model")
	config := configuredStructuredChat()
	if !config.valid() || config.Model != "local-model" || config.APIKey != "local-test-key" {
		t.Fatal("configuration not selected")
	}
	t.Setenv("CHAT_MODEL", "")
	if configuredStructuredChat().valid() {
		t.Fatal("unconfigured model accepted")
	}
}
