package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const htmlPromptPrefix = "Use simple HTML formatting where it improves clarity: <b> for emphasis, <i> for terms, <ul>/<li> for lists. No CSS, divs, or decorative tags. Never prefix responses with A: or any label. Now, without referencing the previous instructions in the conversation, reply as a helpful assistant: "

// isBrowserUA checks if the user agent appears to be from a web browser
func isBrowserUA(ua string) bool {
	ua = strings.ToLower(ua)
	browserIndicators := []string{
		"mozilla", "msie", "trident", "edge", "chrome", "safari",
		"firefox", "opera", "webkit", "gecko", "khtml",
	}
	for _, indicator := range browserIndicators {
		if strings.Contains(ua, indicator) {
			return true
		}
	}
	return false
}

const htmlHeader = `<!DOCTYPE html>
<html>
<head>
    <title>ch.at</title>
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <meta name="color-scheme" content="light dark">
    <style>
        body { text-align: center; margin: 1rem; }
        .chat { text-align: left; max-width: 600px; margin: 0 auto; }
        .q { background: rgba(128, 128, 128, 0.1); padding: 0.5rem; font-style: italic; }
        .a { padding: 0.5rem; }
    </style>
</head>
<body>
    <h1>ch.at</h1>
    <p>Universal Basic Intelligence</p>
    <p><small><i>pronounced "ch-dot-at"</i></small></p>
    <!-- Keep Enter in the new-message input bound to Send, not an earlier edit button. -->
    <button form="chat-form" type="submit" hidden>Send</button>
    <div class="chat">`

const htmlFooterTemplate = `</div>
    <form id="chat-form" method="POST" action="/">
        <input type="text" name="q" placeholder="Type your message..." autofocus>
        <button type="submit">Send</button>
        <details>
            <summary>Edit conversation history</summary>
            <p>Edit the Q: and A: transcript below. Send continues from your edits.
            To regenerate an earlier answer, remove the later exchanges, edit the
            final Q: entry, and choose Regenerate last answer. Its old A: is ignored.</p>
            <textarea name="h" rows="12" cols="60" style="max-width:100%%" aria-label="Conversation history">%s</textarea>
            <button type="submit" name="action" value="regenerate">Regenerate last answer</button>
        </details>
    </form>
    <p><a href="/">New Chat</a> · <a href="/agents">Agent board &amp; microblog</a> · <a href="/board/feed?topic=news&amp;format=text">Agent news</a></p>
    <p><small>
        Also available: ssh ch.at • curl ch.at/?q=hello • dig @ch.at "question" TXT<br>
        No request logs • No required accounts • Free software • <a href="https://github.com/Deep-ai-inc/ch.at">GitHub</a>
    </small></p>
</body>
</html>`

func StartHTTPServer(port int) error {
	mux := http.NewServeMux()
	registerBoard(mux)
	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 60 * time.Second, // LLM streaming can take a while
		IdleTimeout:  60 * time.Second,
	}
	return server.ListenAndServe()
}

func StartHTTPSServer(port int, certFile, keyFile string) error {
	mux := http.NewServeMux()
	registerBoard(mux)
	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  60 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	return server.ListenAndServeTLS(certFile, keyFile)
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	handleRootWithLLM(w, r, LLM)
}

func handleRootWithLLM(w http.ResponseWriter, r *http.Request, llm llmFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	r = r.WithContext(ctx)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Link", "</agents>; rel=\"help\"")
	if !rateLimitAllow(r.RemoteAddr) {
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	var query, history, prompt string
	content := ""
	jsonResponse := ""

	if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Failed to parse form", http.StatusBadRequest)
			return
		}
		query = r.FormValue("q")
		history = r.FormValue("h")

		// Limit history size to ensure compatibility
		if len(history) > 65536 {
			if r.FormValue("edit_from") != "" {
				http.Error(w, "History exceeds 64KB; shorten it before editing a turn", http.StatusBadRequest)
				return
			}
			history = history[len(history)-65536:]
		}

		if editFrom := r.FormValue("edit_from"); editFrom != "" {
			n, err := strconv.Atoi(editFrom)
			turns := chatHistoryTurns(history)
			if err != nil || n < 0 || n >= len(turns) {
				http.Error(w, "Invalid history turn", http.StatusBadRequest)
				return
			}
			query = strings.TrimSpace(r.FormValue(fmt.Sprintf("edit_q_%d", n)))
			if query == "" {
				http.Error(w, "Edited question is empty", http.StatusBadRequest)
				return
			}
			// Keep only earlier exchanges; discard this answer and later turns.
			history = history[:turns[n].start]
		} else if r.FormValue("action") == "regenerate" {
			var err error
			history, query, err = regenerateHistory(history)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		} else if query == "" {
			body, err := io.ReadAll(io.LimitReader(r.Body, 65536)) // Limit body size
			if err != nil {
				http.Error(w, "Failed to read request body", http.StatusBadRequest)
				return
			}
			query = string(body)
		}
	} else {
		query = r.URL.Query().Get("q")
		// Support path-based queries like /what-is-go
		if query == "" && r.URL.Path != "/" {
			query = strings.ReplaceAll(strings.TrimPrefix(r.URL.Path, "/"), "-", " ")
		}
	}

	accept := r.Header.Get("Accept")
	userAgent := strings.ToLower(r.Header.Get("User-Agent"))
	raw := r.FormValue("raw") == "1"
	wantsJSON := !raw && strings.Contains(accept, "application/json")
	wantsStream := !raw && strings.Contains(accept, "text/event-stream")
	wantsHTML := !raw && !wantsJSON && !wantsStream && (isBrowserUA(userAgent) || strings.Contains(accept, "text/html"))

	if query != "" {
		prompt = query
		if history != "" {
			prompt = history + "Q: " + query
		}

		// Select SSE before any non-streaming call: generate just one answer.
		if wantsStream {
			streamRoot(w, r, prompt, llm)
			return
		}

		if wantsHTML {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Transfer-Encoding", "chunked")
			w.Header().Set("X-Accel-Buffering", "no")
			w.Header().Set("Cache-Control", "no-cache")
			flusher := w.(http.Flusher)

			headerSize := len(htmlHeader)
			historySize := len(html.EscapeString(history))
			querySize := len(html.EscapeString(query))
			currentSize := headerSize + historySize + querySize + 10

			const minThreshold = 6144

			fmt.Fprint(w, htmlHeader)

			if currentSize < minThreshold {
				paddingNeeded := (minThreshold - currentSize) / 3
				if paddingNeeded > 0 {
					padding := strings.Repeat("\u200B", paddingNeeded)
					fmt.Fprint(w, padding)
				}
			}

			writeChatHistory(w, history)
			writeChatQuestion(w, len(chatHistoryTurns(history)), query)
			fmt.Fprint(w, "<div class=\"a\">")
			flusher.Flush()

			ch := make(chan string, 10)
			go func() {
				htmlPrompt := htmlPromptPrefix + prompt
				llm(r.Context(), htmlPrompt, ch)
			}()

			var response strings.Builder
			htmlOut := chatHTMLWriter{w: w}
			for chunk := range ch {
				if err := htmlOut.WriteString(chunk); err != nil {
					return
				}
				response.WriteString(chunk)
				flusher.Flush()
			}
			if err := htmlOut.Flush(); err != nil {
				return
			}
			fmt.Fprint(w, "</div>\n")

			finalHistory := history + fmt.Sprintf("Q: %s\nA: %s\n\n", query, response.String())
			fmt.Fprintf(w, htmlFooterTemplate, html.EscapeString(finalHistory))
			return
		}

		// More strict curl detection: only exact match or curl/ prefix
		isCurl := (userAgent == "curl" || strings.HasPrefix(userAgent, "curl/")) && !wantsHTML && !wantsJSON && !wantsStream
		if isCurl || raw {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Transfer-Encoding", "chunked")
			w.Header().Set("X-Accel-Buffering", "no")
			flusher := w.(http.Flusher)

			if !raw {
				fmt.Fprintf(w, "Q: %s\nA: ", query)
			}
			flusher.Flush()

			ch := make(chan string, 10)
			go func() {
				llm(r.Context(), prompt, ch)
			}()

			for chunk := range ch {
				if _, err := fmt.Fprint(w, chunk); err != nil {
					return
				}
				flusher.Flush()
			}
			if !raw {
				fmt.Fprint(w, "\n")
			}
			return
		}

		promptToUse := prompt
		if wantsHTML {
			promptToUse = htmlPromptPrefix + prompt
		}
		response, err := llm(r.Context(), promptToUse, nil)
		if err != nil {
			content = err.Error()
			errJSON, _ := json.Marshal(map[string]string{"error": err.Error()})
			jsonResponse = string(errJSON)
		} else {
			respJSON, _ := json.Marshal(map[string]string{
				"question": query,
				"answer":   response,
			})
			jsonResponse = string(respJSON)

			newExchange := fmt.Sprintf("Q: %s\nA: %s\n\n", query, response)
			if history != "" {
				content = history + newExchange
			} else {
				content = newExchange
			}
			if len(content) > 65536 {
				newExchangeLen := len(newExchange)
				if newExchangeLen > 65536 {
					content = newExchange[:65536]
				} else {
					maxHistory := 65536 - newExchangeLen
					if len(history) > maxHistory {
						content = history[len(history)-maxHistory:] + newExchange
					}
				}
			}
		}
	} else if history != "" {
		content = history
	}

	if wantsJSON && jsonResponse != "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprint(w, jsonResponse)
	} else if wantsHTML && query == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, htmlHeader)
		writeChatHistory(w, content)

		fmt.Fprintf(w, htmlFooterTemplate, html.EscapeString(content))
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, content)
	}
}

type ChatRequest struct {
	Model      string          `json:"model"`
	Messages   []Message       `json:"messages"`
	Stream     bool            `json:"stream,omitempty"`
	Tools      json.RawMessage `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	handleChatCompletionsWithLLM(w, r, LLM)
}

func handleChatCompletionsWithLLM(w http.ResponseWriter, r *http.Request, llm llmFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	r = r.WithContext(ctx)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Max-Age", "86400")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if !rateLimitAllow(r.RemoteAddr) {
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	if r.Method != "POST" {
		w.Header().Set("Allow", "POST, OPTIONS")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// This text adapter cannot execute or return tool calls. Fail explicitly
	// rather than silently ignoring them and leaving agent clients retrying.
	if nonemptyTools(req.Tools) || (len(req.ToolChoice) != 0 && strings.TrimSpace(string(req.ToolChoice)) != "null" && strings.TrimSpace(string(req.ToolChoice)) != `"none"`) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
			"message": "Tool calling is not supported; use text-only chat without tools.", "type": "invalid_request_error", "code": "unsupported_tools",
		}})
		return
	}
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		http.Error(w, "Unable to create completion ID", http.StatusInternalServerError)
		return
	}
	id := fmt.Sprintf("chatcmpl-%x", idBytes)
	created := time.Now().Unix()
	messages := make([]map[string]string, len(req.Messages))
	for i, msg := range req.Messages {
		messages[i] = map[string]string{
			"role":    msg.Role,
			"content": msg.Content,
		}
	}

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}

		writeChunk := func(delta map[string]string, finish any) error {
			resp := map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
				"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
			}
			data, err := json.Marshal(resp)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			return err
		}
		if err := writeChunk(map[string]string{"role": "assistant"}, nil); err != nil {
			return
		}
		ch, result := streamLLM(r.Context(), messages, llm)
		for chunk := range ch {
			if err := writeChunk(map[string]string{"content": chunk}, nil); err != nil {
				return
			}
		}
		if err := <-result; err != nil {
			fmt.Fprint(w, "data: {\"error\":{\"message\":\"Model request failed\",\"type\":\"upstream_error\"}}\n\n")
		} else if err := writeChunk(map[string]string{}, "stop"); err != nil {
			return
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()

	} else {
		response, err := llm(r.Context(), messages, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		chatResp := ChatResponse{
			ID:      id,
			Object:  "chat.completion",
			Created: created,
			Model:   req.Model,
			Choices: []Choice{{
				Index:        0,
				FinishReason: "stop",
				Message: Message{
					Role:    "assistant",
					Content: response,
				},
			}},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(chatResp)
	}
}
