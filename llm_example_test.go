//go:build llmexample && !boardtest

package main

// Run against the template, not an operator's private adapter:
// cp llm.go.example llm.go && go test -race -tags llmexample ./...
// Every HTTP request is intercepted; these tests make no paid API calls.
import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type exampleRoundTrip func(*http.Request) (*http.Response, error)

func (f exampleRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func exampleTransport(t *testing.T, f exampleRoundTrip) {
	t.Helper()
	old := http.DefaultTransport
	http.DefaultTransport = f
	t.Cleanup(func() { http.DefaultTransport = old })
}
func exampleResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestExampleMessagesAndStream(t *testing.T) {
	want := []map[string]string{{"role": "system", "content": "brief"}, {"role": "user", "content": "hello"}}
	exampleTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("invalid request")
		}
		var body struct {
			Messages  []map[string]string
			Stream    bool
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Messages) != 2 || body.Messages[0]["role"] != "system" || body.Messages[1]["content"] != "hello" || body.MaxTokens != 500 {
			t.Errorf("bad request: %#v", body)
		}
		if body.Stream {
			return exampleResponse(200, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"), nil
		}
		return exampleResponse(200, `{"choices":[{"message":{"content":"hello"}}]}`), nil
	})
	answer, err := LLM(context.Background(), want, nil)
	if err != nil || answer != "hello" {
		t.Fatal(answer, err)
	}
	ch := make(chan string, 1)
	if _, err := LLM(context.Background(), want, ch); err != nil {
		t.Fatal(err)
	}
	if got := <-ch; got != "hello" {
		t.Fatal(got)
	}
	if _, ok := <-ch; ok {
		t.Fatal("stream not closed")
	}
}

func TestExampleAlwaysClosesStream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		input  interface{}
		status int
		body   string
	}{
		{"invalid input", 42, 200, ""},
		{"provider error", "hello", 503, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exampleTransport(t, func(r *http.Request) (*http.Response, error) {
				if tc.name == "invalid input" {
					t.Error("invalid input reached provider")
				}
				return exampleResponse(tc.status, tc.body), nil
			})
			ch := make(chan string)
			if _, err := LLM(context.Background(), tc.input, ch); err == nil {
				t.Fatal("expected error")
			}
			if _, ok := <-ch; ok {
				t.Fatal("stream not closed")
			}
		})
	}
}

func TestExampleHTTPContextCancellation(t *testing.T) {
	entered := make(chan struct{})
	exampleTransport(t, func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	ch := make(chan string)
	go func() { _, err := LLM(ctx, "hello", ch); result <- err }()
	<-entered
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP request not canceled")
	}
	if _, ok := <-ch; ok {
		t.Fatal("stream not closed")
	}
}

func TestExampleBlockedConsumerCancellation(t *testing.T) {
	entered := make(chan struct{})
	exampleTransport(t, func(r *http.Request) (*http.Response, error) {
		close(entered)
		return exampleResponse(200, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	ch := make(chan string) // Deliberately no consumer.
	go func() { _, err := LLM(ctx, "hello", ch); result <- err }()
	<-entered
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked stream send leaked")
	}
	if _, ok := <-ch; ok {
		t.Fatal("stream not closed")
	}
}
