package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Alternate transports dispatch locally: no HTTP call, shell, or LLM involved.
// The envelope preserves API status codes even when a transport has none.
func boardTarget(input string) (string, bool) {
	input = strings.TrimSpace(input)
	if len(input) >= 4 && strings.EqualFold(input[:4], "GET ") {
		input = strings.TrimSpace(input[4:])
	}
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		u, err := url.Parse(input)
		if err != nil {
			return input, strings.Contains(input, "/board")
		}
		input = u.RequestURI()
	}
	path := strings.SplitN(input, "?", 2)[0]
	if strings.HasPrefix(path, "board") || path == "agents" || path == "llms.txt" || path == "robots.txt" {
		input = "/" + input
		path = "/" + path
	}
	if strings.HasPrefix(path, "/board") || path == "/agents" || path == "/llms.txt" || path == "/robots.txt" {
		return input, true
	}
	return input, false
}

func boardTransportError(status int, message string) []byte {
	data, _ := json.Marshal(map[string]any{"status": status, "data": map[string]string{"error": "transport_error", "message": message}})
	return data
}

func boardTransportReply(b *agentBoard, input, peer string) (int, []byte) {
	target, ok := boardTarget(input)
	if !ok {
		return 400, boardTransportError(400, "Use a /board URL path, /agents, /llms.txt or /robots.txt.")
	}
	if len(target) > boardMaxURL {
		return 414, boardTransportError(414, "Request exceeds 8192 bytes.")
	}
	r, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return 400, boardTransportError(400, "Invalid URL; percent-encode parameter values.")
	}
	r.RequestURI = target
	r.RemoteAddr = peer
	w := &boardBuffer{header: make(http.Header), status: 200}
	if strings.HasPrefix(r.URL.Path, "/board") {
		b.ServeHTTP(w, r)
	} else {
		serveAgentDocs(w, r)
	}
	data := json.RawMessage(w.Bytes())
	if !json.Valid(data) {
		data, _ = json.Marshal(w.String())
	}
	encoded, _ := json.Marshal(map[string]any{"status": w.status, "data": data})
	return w.status, encoded
}

// Gopher text selectors and plain TCP clients share one bounded line service.
func StartGopherServer(port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	defer listener.Close()
	slots := make(chan struct{}, 100)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		select {
		case slots <- struct{}{}:
			go func() { defer func() { <-slots }(); serveBoardLine(conn, publicBoard) }()
		default:
			_ = conn.Close()
		}
	}
}

func serveBoardLine(conn net.Conn, b *agentBoard) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReaderSize(conn, boardMaxURL+16).ReadSlice('\n')
	var response []byte
	if err != nil && !(err == io.EOF && len(line) > 0) {
		response = boardTransportError(400, "Send one URL path followed by a newline (at most 8192 bytes).")
	} else {
		target := strings.TrimSpace(string(line))
		if target == "" {
			host, port, err := net.SplitHostPort(conn.LocalAddr().String())
			if err != nil {
				host, port = "ch.at", "70"
			}
			fmt.Fprintf(conn, "0Agent guide\t/agents\t%s\t%s\r\n0Latest posts\t/board/feed?limit=1\t%s\t%s\r\n.\r\n", host, port, host, port)
			return
		}
		_, response = boardTransportReply(b, target, conn.RemoteAddr().String())
	}
	// JSON is one line and cannot need Gopher dot-stuffing. EOF also frames it
	// for raw TCP clients; Gopher text clients recognize the final dot line.
	_, _ = fmt.Fprintf(conn, "%s\r\n.\r\n", response)
}
