package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
)

// All adapters accept cancellation and own/close their output channel, even on
// errors. Keeping this contract shared makes transports testable without API calls.
type llmFunc func(context.Context, interface{}, chan<- string) (string, error)

func streamLLM(ctx context.Context, input interface{}, llm llmFunc) (<-chan string, <-chan error) {
	ch := make(chan string, 10)
	result := make(chan error, 1)
	go func() {
		_, err := llm(ctx, input, ch)
		result <- err
	}()
	return ch, result
}

func streamRoot(w http.ResponseWriter, r *http.Request, prompt string, llm llmFunc) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, result := streamLLM(r.Context(), prompt, llm)
	lastCR := false
	for chunk := range ch {
		// Every physical line needs its own data: prefix. Never let model
		// newlines inject event fields or end an SSE frame prematurely.
		var normalized strings.Builder
		for i := 0; i < len(chunk); i++ {
			c := chunk[i]
			if c == '\r' {
				normalized.WriteByte('\n')
			} else if c != '\n' || !lastCR {
				normalized.WriteByte(c)
			}
			lastCR = c == '\r'
		}
		chunk = normalized.String()
		for _, line := range strings.Split(chunk, "\n") {
			if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
				return
			}
		}
		if _, err := fmt.Fprint(w, "\n"); err != nil {
			return
		}
		flusher.Flush()
	}
	if err := <-result; err != nil {
		fmt.Fprint(w, "event: error\ndata: Model request failed\n\n")
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func nonemptyTools(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var tools []json.RawMessage
	return json.Unmarshal(raw, &tools) != nil || len(tools) > 0
}

type chatHistoryTurn struct {
	start            int
	question, answer string
}

func chatHistoryTurns(history string) []chatHistoryTurn {
	parts := strings.Split("\n"+history, "\nQ: ")
	start := len(parts[0])
	var turns []chatHistoryTurn
	for _, part := range parts[1:] {
		question, answer := part, ""
		if i := strings.Index(part, "\nA: "); i >= 0 {
			question, answer = part[:i], strings.TrimRight(part[i+4:], "\n")
		}
		turns = append(turns, chatHistoryTurn{start, question, answer})
		start += len("Q: ") + len(part) + 1
	}
	return turns
}

func writeChatQuestion(w io.Writer, turn int, question string) {
	// All controls share the one footer form/history field. Do not embed a
	// growing history prefix per turn: that makes page size quadratic.
	fmt.Fprintf(w, `<div class="q">%s</div>
<details><summary>Edit this question</summary>
<textarea form="chat-form" name="edit_q_%d" rows="3" cols="60" style="max-width:100%%" aria-label="Edit question">%s</textarea>
<button form="chat-form" type="submit" name="edit_from" value="%d">Regenerate from here</button>
</details>
`, html.EscapeString(question), turn, html.EscapeString(question), turn)
}

func writeChatHistory(w io.Writer, history string) {
	for i, turn := range chatHistoryTurns(history) {
		writeChatQuestion(w, i, turn.question)
		fmt.Fprintf(w, "<div class=\"a\">%s</div>\n", safeChatHTML(turn.answer))
	}
}

// The existing no-JavaScript transcript uses Q:/A: line markers. Regeneration
// discards the final answer and all text following it, with exactly one model
// invocation. In the advanced transcript editor, the user removes later turns
// explicitly; per-question edit buttons handle that automatically instead.
func regenerateHistory(history string) (string, string, error) {
	start := strings.LastIndex("\n"+history, "\nQ: ")
	if start < 0 {
		return "", "", fmt.Errorf("History must contain a Q: entry to regenerate")
	}
	question := history[start+len("Q: "):]
	if end := strings.Index(question, "\nA: "); end >= 0 {
		question = question[:end]
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return "", "", fmt.Errorf("The final question is empty")
	}
	return history[:start], question, nil
}

// Only these exact, attribute-free formatting tags may be rendered. Both model
// output and editable transcripts are untrusted; escape everything else. This
// deliberately small allowlist needs no HTML/JS dependency or DOM execution.
var chatHTMLTags = map[string]bool{
	"<b>": true, "</b>": true, "<i>": true, "</i>": true,
	"<ul>": true, "</ul>": true, "<li>": true, "</li>": true,
	"<strong>": true, "</strong>": true, "<em>": true, "</em>": true,
	"<code>": true, "</code>": true, "<pre>": true, "</pre>": true,
	"<p>": true, "</p>": true, "<br>": true, "<br/>": true,
}

type chatHTMLWriter struct {
	w       io.Writer
	pending string
}

func (w *chatHTMLWriter) WriteString(s string) error {
	s = w.pending + s
	w.pending = ""
	for len(s) > 0 {
		start := strings.IndexByte(s, '<')
		if start < 0 {
			_, err := io.WriteString(w.w, html.EscapeString(s))
			return err
		}
		if _, err := io.WriteString(w.w, html.EscapeString(s[:start])); err != nil {
			return err
		}
		s = s[start:]
		end := strings.IndexByte(s, '>')
		if end < 0 {
			// Buffer only a possible allowlisted tag, not an unbounded response.
			if len(s) <= 9 {
				w.pending = s
				return nil
			}
			_, err := io.WriteString(w.w, html.EscapeString(s))
			return err
		}
		tag := s[:end+1]
		if !chatHTMLTags[tag] {
			tag = html.EscapeString(tag)
		}
		if _, err := io.WriteString(w.w, tag); err != nil {
			return err
		}
		s = s[end+1:]
	}
	return nil
}

func (w *chatHTMLWriter) Flush() error {
	_, err := io.WriteString(w.w, html.EscapeString(w.pending))
	w.pending = ""
	return err
}

func safeChatHTML(s string) string {
	var out strings.Builder
	w := chatHTMLWriter{w: &out}
	_ = w.WriteString(s)
	_ = w.Flush()
	return out.String()
}
