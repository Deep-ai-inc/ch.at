package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Optional acceptance test with the real OpenCode binary. Tool execution is
// limited to reading a disposable fixture; there are no paid model calls.
// Set CHAT_OPENCODE_BIN to an absolute path to an installed OpenCode CLI.
func TestOpenCodeAgentToolRoundTrip(t *testing.T) {
	binary := os.Getenv("CHAT_OPENCODE_BIN")
	if binary == "" {
		t.Skip("set CHAT_OPENCODE_BIN for the real CLI acceptance test")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(".", ".opencode-agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	fixture := filepath.Join(root, "fixture.txt")
	const marker = "LOCAL_TOOL_ROUNDTRIP_74be92"
	if err := os.WriteFile(fixture, []byte(marker+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	for _, dir := range []string{home, filepath.Join(home, "config"), filepath.Join(home, "data"), filepath.Join(home, "cache"), filepath.Join(home, "state"), filepath.Join(home, "tmp")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	var requests, issued, returned atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 8 {
			http.Error(w, "unexpected agent loop", 400)
			return
		}
		var request struct {
			Stream bool `json:"stream"`
			Tools  []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
			Messages []struct {
				Role       string          `json:"role"`
				ToolCallID string          `json:"tool_call_id"`
				Content    json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", 400)
			return
		}
		hasRead := false
		for _, tool := range request.Tools {
			if tool.Function.Name == "read" {
				hasRead = true
			}
		}
		for _, message := range request.Messages {
			if message.Role == "tool" && message.ToolCallID == "call_fixture" && strings.Contains(string(message.Content), marker) {
				returned.Add(1)
				writeAgentFixtureReply(w, request.Stream, "Verified "+marker, "", nil)
				return
			}
		}
		if hasRead {
			issued.Add(1)
			arguments, _ := json.Marshal(map[string]string{"filePath": fixture})
			writeAgentFixtureReply(w, request.Stream, "", "read", arguments)
			return
		}
		// OpenCode can make an ancillary title/summary request. It uses the same
		// local provider, never a second real model or external credential.
		writeAgentFixtureReply(w, request.Stream, "Local fixture check", "", nil)
	}))
	defer upstream.Close()
	backend := structuredBackendFor(upstream)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		// Isolate test request-rate accounting from other loopback tests.
		r.RemoteAddr = "198.18.0.60:1234"
		handleStructuredChat(w, r, backend)
	}))
	defer gateway.Close()
	config := map[string]any{
		"$schema": "https://opencode.ai/config.json", "model": "chat/default", "small_model": "chat/default",
		"enabled_providers": []string{"chat"}, "autoupdate": false, "share": "disabled", "snapshot": false,
		"permission": map[string]any{"*": "deny", "read": "allow", "external_directory": "deny"},
		"provider": map[string]any{"chat": map[string]any{
			"npm": "@ai-sdk/openai-compatible", "name": "chat",
			"options": map[string]any{"baseURL": gateway.URL + "/v1", "apiKey": "anonymous"},
			"models":  map[string]any{"default": map[string]any{"name": "Local test backend", "tool_call": true, "limit": map[string]int{"context": 32768, "output": 500}}},
		}},
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "run", "--pure", "--model", "chat/default", "--title", "Local tool test", "--format", "json", "Read fixture.txt using the read tool and report its exact marker. Do not run shell commands or edit files.")
	command.Dir = root
	// Do not inherit provider credentials or the user's OpenCode config/state.
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"), "XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"), "TMPDIR=" + filepath.Join(home, "tmp"),
		"OPENCODE_CONFIG_CONTENT=" + string(configJSON), "OPENCODE_DISABLE_AUTOUPDATE=true",
		"OPENCODE_DISABLE_MODELS_FETCH=true", "OPENCODE_DISABLE_DEFAULT_PLUGINS=true",
		"DO_NOT_TRACK=1", "NO_COLOR=1", "NO_PROXY=127.0.0.1,localhost",
	}
	output, err := command.CombinedOutput()
	if err != nil || issued.Load() != 1 || returned.Load() != 1 || !strings.Contains(string(output), "Verified "+marker) {
		t.Fatalf("OpenCode round trip failed: %v requests=%d issued=%d returned=%d\n%s", err, requests.Load(), issued.Load(), returned.Load(), output)
	}
	data, err := os.ReadFile(fixture)
	if err != nil || string(data) != marker+"\n" {
		t.Fatal("fixture was altered", err)
	}
	t.Logf("Real OpenCode CLI: tool call -> fixture read -> matching tool result -> final answer (%d local upstream calls)", requests.Load())
}

func writeAgentFixtureReply(w http.ResponseWriter, stream bool, text, tool string, arguments []byte) {
	finish := "stop"
	message := map[string]any{"role": "assistant", "content": text}
	if tool != "" {
		finish = "tool_calls"
		message["content"] = nil
		message["tool_calls"] = []any{map[string]any{"id": "call_fixture", "type": "function", "function": map[string]any{"name": tool, "arguments": string(arguments)}}}
	}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "fixture-completion", "object": "chat.completion", "created": 1, "model": "configured-model", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	emit := func(delta map[string]any, reason any) {
		data, _ := json.Marshal(map[string]any{"id": "fixture-completion", "object": "chat.completion.chunk", "created": 1, "model": "configured-model", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}})
		fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
	}
	emit(map[string]any{"role": "assistant"}, nil)
	if tool != "" {
		emit(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_fixture", "type": "function", "function": map[string]any{"name": tool, "arguments": ""}}}}, nil)
		middle := len(arguments) / 2
		for _, part := range [][]byte{arguments[:middle], arguments[middle:]} {
			emit(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": string(part)}}}}, nil)
		}
	} else {
		emit(map[string]any{"content": text}, nil)
	}
	emit(map[string]any{}, finish)
	fmt.Fprint(w, "data: [DONE]\n\n")
}
