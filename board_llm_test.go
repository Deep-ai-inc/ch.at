//go:build boardtest

package main

// The repository intentionally excludes the deployed llm.go. This opt-in test
// stub allows clean-checkout integration tests without credentials or API calls.
// Do not enable boardtest alongside an operator-provided llm.go.
import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func LLM(_ context.Context, _ interface{}, stream chan<- string) (string, error) {
	if stream != nil {
		stream <- "boardtest response"
		close(stream)
	}
	return "boardtest response", nil
}

func TestBoardHTTPIntegration(t *testing.T) {
	mux := http.NewServeMux()
	registerBoard(mux)
	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)
	// Board routing must not fall through to the root's path-as-prompt feature.
	for _, path := range []string{"/board", "/agents", "/llms.txt", "/board/feed?topic=news", "/board/search?q=bugs"} {
		w := boardRequest(mux, path)
		if w.Code != 200 || strings.Contains(w.Body.String(), "boardtest response") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	w := boardRequest(mux, "/?q=hello")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "boardtest response") {
		t.Fatal("existing chat broke", w.Body)
	}
	if !strings.Contains(htmlFooterTemplate, `href="/agents"`) {
		t.Fatal("missing homepage discovery")
	}
}
