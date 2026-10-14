package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var chatTestPeer atomic.Uint32

func chatRequest(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	n := chatTestPeer.Add(1)
	r.RemoteAddr = fmt.Sprintf("198.51.%d.%d:1234", n/250, n%250+1)
	return r
}

func fixedChatLLM(calls *atomic.Int32, chunks ...string) llmFunc {
	return func(ctx context.Context, input interface{}, ch chan<- string) (string, error) {
		calls.Add(1)
		if ch == nil {
			return strings.Join(chunks, ""), nil
		}
		defer close(ch)
		for _, chunk := range chunks {
			select {
			case ch <- chunk:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "", nil
	}
}

func TestRootRawAndSingleGeneration(t *testing.T) {
	for _, tc := range []struct{ name, target, ua, accept, want string }{
		{"curl", "/?q=hello", "curl/8", "", "Q: hello\nA: reply\n"},
		{"curl raw", "/?q=hello&raw=1", "curl/8", "", "reply"},
		{"generic raw", "/?q=hello&raw=1", "client", "text/plain", "reply"},
		{"browser raw", "/?q=hello&raw=1", "Mozilla/5.0", "text/html", "reply"},
		{"raw overrides json", "/?q=hello&raw=1", "client", "application/json", "reply"},
		{"SSE", "/?q=hello", "curl/8", "text/event-stream", "data: reply\n\ndata: [DONE]\n\n"},
		{"browser SSE", "/?q=hello", "Mozilla/5.0", "text/event-stream", "data: reply\n\ndata: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chatRequest("GET", tc.target, "")
			r.Header.Set("User-Agent", tc.ua)
			r.Header.Set("Accept", tc.accept)
			w := httptest.NewRecorder()
			var calls atomic.Int32
			handleRootWithLLM(w, r, fixedChatLLM(&calls, "reply"))
			if w.Code != 200 || w.Body.String() != tc.want || calls.Load() != 1 {
				t.Fatalf("status %d calls %d body %q", w.Code, calls.Load(), w.Body.String())
			}
		})
	}
}

func TestRootSSEMultilineAndError(t *testing.T) {
	r := chatRequest("GET", "/?q=hello", "")
	r.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	var calls atomic.Int32
	handleRootWithLLM(w, r, fixedChatLLM(&calls, "one\n\nevent: injected\r\nthree"))
	if want := "data: one\ndata: \ndata: event: injected\ndata: three\n\ndata: [DONE]\n\n"; w.Body.String() != want {
		t.Fatal(w.Body.String())
	}
	w = httptest.NewRecorder()
	handleRootWithLLM(w, r, func(_ context.Context, _ interface{}, ch chan<- string) (string, error) {
		close(ch)
		return "", errors.New("private upstream details")
	})
	if !strings.Contains(w.Body.String(), "event: error\n") || strings.Contains(w.Body.String(), "private") {
		t.Fatal(w.Body.String())
	}
}

func TestChatCompletionsStreamTermination(t *testing.T) {
	r := chatRequest("POST", "/v1/chat/completions", `{"model":"default","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	w := httptest.NewRecorder()
	var calls atomic.Int32
	handleChatCompletionsWithLLM(w, r, fixedChatLLM(&calls, "Hello", "\n\"world\""))
	frames := strings.Split(strings.TrimSpace(w.Body.String()), "\n\n")
	if w.Code != 200 || len(frames) != 5 || frames[4] != "data: [DONE]" || calls.Load() != 1 {
		t.Fatal(w.Code, w.Body.String(), calls.Load())
	}
	id := ""
	var created float64
	for i, frame := range frames[:4] {
		var payload struct {
			ID      string
			Object  string
			Created float64
			Model   string
			Choices []struct {
				Index        int
				Delta        map[string]string
				FinishReason *string `json:"finish_reason"`
			}
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &payload); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			id = payload.ID
			created = payload.Created
		}
		if payload.ID != id || !strings.HasPrefix(id, "chatcmpl-") || payload.Created != created || payload.Object != "chat.completion.chunk" || payload.Model != "default" || len(payload.Choices) != 1 {
			t.Fatal(payload)
		}
		choice := payload.Choices[0]
		switch i {
		case 0:
			if choice.Delta["role"] != "assistant" {
				t.Fatal(choice)
			}
		case 1:
			if choice.Delta["content"] != "Hello" {
				t.Fatal(choice)
			}
		case 2:
			if choice.Delta["content"] != "\n\"world\"" {
				t.Fatal(choice)
			}
		case 3:
			if len(choice.Delta) != 0 || choice.FinishReason == nil || *choice.FinishReason != "stop" {
				t.Fatal(choice)
			}
		}
		if i < 3 && choice.FinishReason != nil {
			t.Fatal("premature finish", choice)
		}
	}
}

func TestChatCompletionsHistoryAndNonstreamFinish(t *testing.T) {
	want := []map[string]string{{"role": "system", "content": "Be brief"}, {"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}, {"role": "user", "content": "continue"}}
	body, _ := json.Marshal(map[string]any{"model": "default", "messages": want})
	w := httptest.NewRecorder()
	handleChatCompletionsWithLLM(w, chatRequest("POST", "/v1/chat/completions", string(body)), func(ctx context.Context, input interface{}, ch chan<- string) (string, error) {
		if !reflect.DeepEqual(input, want) || ch != nil {
			t.Errorf("wrong messages: %#v", input)
		}
		return "answer", nil
	})
	var response ChatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Choices) != 1 || response.Choices[0].FinishReason != "stop" || response.Choices[0].Message.Role != "assistant" || response.Choices[0].Message.Content != "answer" {
		t.Fatal(response)
	}
}

func TestLegacyChatCompletionsRequireToolBackend(t *testing.T) {
	for _, tools := range []string{`"tools":[{"type":"function","function":{"name":"test"}}]`, `"tool_choice":"auto"`, `"tools":{}`} {
		w := httptest.NewRecorder()
		var calls atomic.Int32
		handleChatCompletionsWithLLM(w, chatRequest("POST", "/v1/chat/completions", `{"messages":[],`+tools+`}`), fixedChatLLM(&calls, "no"))
		if w.Code != 400 || calls.Load() != 0 || !strings.Contains(w.Body.String(), "unsupported_tools") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	var calls atomic.Int32
	handleChatCompletionsWithLLM(w, chatRequest("POST", "/v1/chat/completions", `{"messages":[{"role":"user","content":"hello"}],"tools":[],"tool_choice":"none"}`), fixedChatLLM(&calls, "yes"))
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestChatCompletionsStreamErrorNotSuccess(t *testing.T) {
	w := httptest.NewRecorder()
	handleChatCompletionsWithLLM(w, chatRequest("POST", "/v1/chat/completions", `{"stream":true,"messages":[]}`), func(_ context.Context, _ interface{}, ch chan<- string) (string, error) {
		close(ch)
		return "", errors.New("secret provider response")
	})
	body := w.Body.String()
	if !strings.Contains(body, `"type":"upstream_error"`) || strings.Contains(body, `"finish_reason":"stop"`) || strings.Contains(body, "secret") || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatal(body)
	}
}

type failingChatWriter struct{ header http.Header }

func (w *failingChatWriter) Header() http.Header       { return w.header }
func (w *failingChatWriter) WriteHeader(int)           {}
func (w *failingChatWriter) Write([]byte) (int, error) { return 0, errors.New("client disconnected") }
func (w *failingChatWriter) Flush()                    {}

func TestRootDisconnectCancelsGeneration(t *testing.T) {
	finished := make(chan struct{})
	w := &failingChatWriter{header: make(http.Header)}
	r := chatRequest("GET", "/?q=hello&raw=1", "")
	handleRootWithLLM(w, r, func(ctx context.Context, _ interface{}, ch chan<- string) (string, error) {
		defer close(finished)
		defer close(ch)
		for {
			select {
			case ch <- "reply":
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	})
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("model still running after disconnect")
	}
}

func TestChatStreamClosesOverHTTP(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleChatCompletionsWithLLM(w, r, fixedChatLLM(&calls, "answer"))
	}))
	defer s.Close()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Post(s.URL, "application/json", strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || !strings.HasSuffix(string(body), "data: [DONE]\n\n") || calls.Load() != 1 {
		t.Fatalf("%s %v", body, err)
	}
}

func TestEditableHistoryRegeneratesOnce(t *testing.T) {
	history := "Q: first\nA: original\n\nQ: edited second\nA: stale answer\n\n"
	form := url.Values{"h": {history}, "action": {"regenerate"}, "q": {"must be ignored"}}
	r := chatRequest("POST", "/", form.Encode())
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	var calls atomic.Int32
	handleRootWithLLM(w, r, func(ctx context.Context, input interface{}, ch chan<- string) (string, error) {
		calls.Add(1)
		defer close(ch)
		want := htmlPromptPrefix + "Q: first\nA: original\n\nQ: edited second"
		if input != want {
			t.Errorf("wrong prompt %q", input)
		}
		ch <- "replacement"
		return "", nil
	})
	body := w.Body.String()
	if w.Code != 200 || calls.Load() != 1 || strings.Contains(body, "stale answer") || strings.Contains(body, "must be ignored") || !strings.Contains(body, "Edit conversation history") || !strings.Contains(body, "replacement") {
		t.Fatal(w.Code, body)
	}
}

func TestHistoryEmptyAndEscaping(t *testing.T) {
	for _, history := range []string{"", "not a transcript", "Q: \nA: old"} {
		form := url.Values{"h": {history}, "action": {"regenerate"}}
		r := chatRequest("POST", "/", form.Encode())
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		var calls atomic.Int32
		handleRootWithLLM(w, r, fixedChatLLM(&calls, "no"))
		if w.Code != 400 || calls.Load() != 0 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	form := url.Values{"h": {"Q: hello\nA: </textarea><script>alert(1)</script><b>safe</b>\n\n"}}
	r := chatRequest("POST", "/", form.Encode())
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	var calls atomic.Int32
	handleRootWithLLM(w, r, fixedChatLLM(&calls, "no"))
	if calls.Load() != 0 || strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "</textarea><script>") || !strings.Contains(w.Body.String(), "<b>safe</b>") {
		t.Fatal(w.Body.String())
	}
}

func TestEditEarlierQuestionDiscardsLaterTurns(t *testing.T) {
	history := "Q: first\nA: one\n\nQ: second\nA: two\n\nQ: third\nA: three\n\n"
	for _, tc := range []struct{ turn, edited, wantPrefix string }{
		{"0", "changed first", ""},
		{"1", "changed second", "Q: first\nA: one\n\n"},
		{"2", "changed third", "Q: first\nA: one\n\nQ: second\nA: two\n\n"},
	} {
		form := url.Values{"h": {history}, "edit_from": {tc.turn}, "edit_q_" + tc.turn: {tc.edited}, "q": {"ignored new question"}}
		r := chatRequest("POST", "/", form.Encode())
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Accept", "text/html")
		w := httptest.NewRecorder()
		var calls atomic.Int32
		handleRootWithLLM(w, r, func(ctx context.Context, input interface{}, ch chan<- string) (string, error) {
			calls.Add(1)
			defer close(ch)
			want := htmlPromptPrefix + tc.edited
			if tc.wantPrefix != "" {
				want = htmlPromptPrefix + tc.wantPrefix + "Q: " + tc.edited
			}
			if input != want {
				t.Errorf("turn %s prompt %q want %q", tc.turn, input, want)
			}
			ch <- "new answer"
			return "", nil
		})
		if w.Code != 200 || calls.Load() != 1 || strings.Contains(w.Body.String(), "ignored new question") || strings.Contains(w.Body.String(), "A: three") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	// One shared history field, not quadratic per-turn hidden transcripts.
	var rendered strings.Builder
	writeChatHistory(&rendered, history)
	if strings.Contains(rendered.String(), `name="h"`) || strings.Count(rendered.String(), `form="chat-form"`) != 6 {
		t.Fatal(rendered.String())
	}
	for _, value := range []string{"-1", "3", "garbage"} {
		form := url.Values{"h": {history}, "edit_from": {value}}
		r := chatRequest("POST", "/", form.Encode())
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		var calls atomic.Int32
		handleRootWithLLM(w, r, fixedChatLLM(&calls, "no"))
		if w.Code != 400 || calls.Load() != 0 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestChatHTMLFormattingAcrossChunks(t *testing.T) {
	for _, chunks := range [][]string{
		{"<b>bold</b><script>alert(1)</script><img src=x onerror=alert(1)>&"},
		{"<", "b", ">bold</", "b>", "<scr", "ipt>alert(1)</script>", "<img src=x onerror=alert(1)>", "&"},
	} {
		var out strings.Builder
		w := chatHTMLWriter{w: &out}
		for _, chunk := range chunks {
			if err := w.WriteString(chunk); err != nil {
				t.Fatal(err)
			}
		}
		w.Flush()
		want := "<b>bold</b>&lt;script&gt;alert(1)&lt;/script&gt;&lt;img src=x onerror=alert(1)&gt;&amp;"
		if out.String() != want {
			t.Fatal(out.String())
		}
	}
}
