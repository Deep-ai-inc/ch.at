package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const structuredRequestLimit = 1 << 20
const structuredResponseLimit = 8 << 20
const structuredOutputTokens = 500 // Same output budget as llm.go.example.

// The legacy LLM function returns only text. A configured OpenAI-compatible
// upstream preserves structured tools/results and usage without running tools
// here or breaking an existing private text adapter's build.
type structuredChatBackend struct{ URL, APIKey, Model string }

var defaultStructuredChat *structuredChatBackend // Set only during adapter init.

func configureStructuredChat(endpoint, key, model string) {
	defaultStructuredChat = &structuredChatBackend{URL: endpoint, APIKey: key, Model: model}
}
func configuredStructuredChat() *structuredChatBackend {
	if endpoint := os.Getenv("CHAT_API_URL"); endpoint != "" {
		return &structuredChatBackend{URL: endpoint, APIKey: os.Getenv("CHAT_API_KEY"), Model: os.Getenv("CHAT_MODEL")}
	}
	return defaultStructuredChat
}
func (b *structuredChatBackend) valid() bool {
	if b == nil || b.Model == "" {
		return false
	}
	u, err := url.Parse(b.URL)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.Fragment == ""
}

func chatAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message, "type": code, "code": code}})
}

func handleStructuredChat(w http.ResponseWriter, r *http.Request, backend *structuredChatBackend) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		chatAPIError(w, 405, "invalid_request_error", "Use POST")
		return
	}
	if !rateLimitAllow(r.RemoteAddr) {
		chatAPIError(w, 429, "rate_limit_error", "Rate limit exceeded")
		return
	}
	if !backend.valid() {
		chatAPIError(w, 503, "configuration_error", "Structured model backend is not configured correctly")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, structuredRequestLimit))
	if err != nil {
		chatAPIError(w, 413, "invalid_request_error", "Request exceeds 1 MiB")
		return
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		chatAPIError(w, 400, "invalid_request_error", "Expected a JSON object")
		return
	}
	var messages []json.RawMessage
	if json.Unmarshal(request["messages"], &messages) != nil || len(messages) == 0 {
		chatAPIError(w, 400, "invalid_request_error", "messages must be a nonempty array")
		return
	}
	var stream bool
	if value, ok := request["stream"]; ok && json.Unmarshal(value, &stream) != nil {
		chatAPIError(w, 400, "invalid_request_error", "stream must be a boolean")
		return
	}
	var flusher http.Flusher
	if stream {
		var ok bool
		flusher, ok = w.(http.Flusher)
		if !ok {
			chatAPIError(w, 500, "server_error", "Streaming unavailable")
			return
		}
	}
	if value, ok := request["n"]; ok && string(bytes.TrimSpace(value)) != "1" {
		chatAPIError(w, 400, "invalid_request_error", "Only n=1 is supported")
		return
	}
	// Model selection stays under operator control; clients cannot switch to a
	// more expensive model. Preserve all structured message and tool fields.
	request["model"], _ = json.Marshal(backend.Model)
	request["n"] = json.RawMessage("1")
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if value, ok := request[key]; ok {
			var n int
			if json.Unmarshal(value, &n) != nil || n <= 0 {
				chatAPIError(w, 400, "invalid_request_error", key+" must be a positive integer")
				return
			}
			request[key], _ = json.Marshal(min(n, structuredOutputTokens))
		}
	}
	if _, ok := request["max_completion_tokens"]; ok {
		delete(request, "max_tokens")
	} else if _, ok := request["max_tokens"]; !ok {
		request["max_tokens"], _ = json.Marshal(structuredOutputTokens)
	}
	body, err = json.Marshal(request)
	if err != nil {
		chatAPIError(w, 400, "invalid_request_error", "Unable to encode request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, backend.URL, bytes.NewReader(body))
	if err != nil {
		chatAPIError(w, 503, "configuration_error", "Invalid upstream endpoint")
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	if backend.APIKey != "" {
		upstream.Header.Set("Authorization", "Bearer "+backend.APIKey)
	}
	client := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(upstream)
	if err != nil {
		chatAPIError(w, 502, "upstream_error", "Model request failed")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := http.StatusBadGateway
		if response.StatusCode == 429 {
			status = 429
			w.Header().Set("Retry-After", "60")
		}
		chatAPIError(w, status, "upstream_error", "Model provider rejected the request")
		return
	}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		if err := forwardStructuredStream(w, flusher, response.Body); err != nil {
			cancel()
			// Never fabricate a successful stop on timeout/truncation. Do not
			// disclose provider error bodies, URLs or credentials to clients.
			fmt.Fprint(w, "data: {\"error\":{\"type\":\"upstream_error\",\"message\":\"Model stream failed\"}}\n\ndata: [DONE]\n\n")
			flusher.Flush()
		}
		return
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, structuredResponseLimit+1))
	var result map[string]json.RawMessage
	if err != nil || len(data) > structuredResponseLimit || json.Unmarshal(data, &result) != nil || result["choices"] == nil || result["error"] != nil {
		chatAPIError(w, 502, "upstream_error", "Invalid model response")
		return
	}
	var choices []json.RawMessage
	if json.Unmarshal(result["choices"], &choices) != nil || len(choices) != 1 {
		chatAPIError(w, 502, "upstream_error", "Invalid model response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func forwardStructuredStream(w io.Writer, flusher http.Flusher, body io.Reader) error {
	scanner := bufio.NewScanner(io.LimitReader(body, structuredResponseLimit+1))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var data []string
	total := 0
	finished := false
	emit := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		if strings.TrimSpace(payload) == "[DONE]" {
			if !finished {
				return false, errors.New("missing finish reason")
			}
			_, err := fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			return true, err
		}
		var frame struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &frame) != nil || frame.Error != nil || frame.Choices == nil || len(frame.Choices) > 1 {
			return false, errors.New("invalid model stream frame")
		}
		for _, choice := range frame.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finished = true
			}
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(payload)); err != nil {
			return false, err
		}
		_, err := fmt.Fprintf(w, "data: %s\n\n", compact.Bytes())
		flusher.Flush()
		return false, err
	}
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > structuredResponseLimit {
			return errors.New("model stream too large")
		}
		if line == "" {
			done, err := emit()
			if done || err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if done, err := emit(); done || err != nil {
		return err
	}
	return errors.New("model stream ended without DONE")
}
