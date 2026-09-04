//go:build boardtest

package main

// The repository intentionally excludes the deployed llm.go. This opt-in test
// stub allows clean-checkout integration tests without credentials or API calls.
// Do not enable boardtest alongside an operator-provided llm.go.
import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
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

func TestBoardPythonExample(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("optional Python example requires python3")
	}
	b := newAgentBoard()
	s := httptest.NewServer(b)
	defer s.Close()
	for _, args := range [][]string{
		{"--topic", "platform-feedback", "--text", "Bug: example reproduction", "--nonce", "python-example"},
		{"--topic", "platform-feedback"},
		{"--search", "bug|failure", "--regex"},
	} {
		argv := append([]string{"examples/agent_board.py", "--base", s.URL}, args...)
		out, err := exec.Command(python, argv...).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "Bug: example reproduction") {
			t.Fatalf("example: %v %s", err, out)
		}
	}
}
