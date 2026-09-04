package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func boardRequest(b http.Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = "192.0.2.1:1234"
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)
	return w
}

func publish(t *testing.T, b *agentBoard, topic, text, nonce string) boardMessage {
	t.Helper()
	w := boardRequest(b, "/board/write?"+url.Values{"topic": {topic}, "text": {text}, "nonce": {nonce}}.Encode())
	if w.Code != 201 {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	var m boardMessage
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.ID == "" || m.URL == "" || m.ExpiresAt.Sub(m.CreatedAt) != boardRetention {
		t.Fatalf("invalid message: %+v", m)
	}
	return m
}

type boardPage struct {
	Messages []boardMessage `json:"messages"`
	Next     string         `json:"next_cursor"`
	Partial  bool           `json:"partial"`
	Scanned  int            `json:"scanned"`
}

func page(t *testing.T, b *agentBoard, path string) boardPage {
	t.Helper()
	w := boardRequest(b, path)
	if w.Code != 200 {
		t.Fatalf("page %s: %d %s", path, w.Code, w.Body)
	}
	var p boardPage
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBoardLifecycle(t *testing.T) {
	b := newAgentBoard()
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	m := publish(t, b, "research", "Hello <script>alert(1)</script>", "one")
	w := boardRequest(b, m.URL)
	if w.Code != 200 || strings.Contains(w.Body.String(), "<script>") {
		t.Fatalf("unsafe response: %s", w.Body)
	}
	q := url.Values{"topic": {"research"}, "text": {m.Text}, "nonce": {"one"}}
	if w = boardRequest(b, "/board/write?"+q.Encode()); w.Code != 200 {
		t.Fatal(w.Body)
	}
	q.Set("text", "changed")
	if w = boardRequest(b, "/board/write?"+q.Encode()); w.Code != 409 {
		t.Fatal(w.Body)
	}
	w = boardRequest(b, "/board/write?topic=research&text=reply&nonce=two&reply_to="+m.ID)
	if w.Code != 201 {
		t.Fatal(w.Body)
	}
	if w = boardRequest(b, "/board/write?topic=other&text=reply&nonce=two&reply_to="+m.ID); w.Code != 400 {
		t.Fatal(w.Body)
	}
	now = now.Add(boardRetention)
	if w = boardRequest(b, m.URL); w.Code != 410 {
		t.Fatal(w.Body)
	}
	if len(b.messages) != 0 {
		t.Fatal("not expired")
	}
	publish(t, b, "research", m.Text, "one") // nonce expires with message
	if w = boardRequest(newAgentBoard(), m.URL); w.Code != 404 {
		t.Fatal("old session resolves")
	}
}

func TestBoardFeedSearchAndPaging(t *testing.T) {
	b := newAgentBoard()
	a := publish(t, b, "news", "DOWNLOAD released. Source: https://example.org", "a")
	c := publish(t, b, "research", "Timeout while running test", "b")
	d := publish(t, b, "news", "retry succeeded", "c")
	p := page(t, b, "/board/feed?limit=2")
	if len(p.Messages) != 2 || p.Messages[0].ID != d.ID || p.Messages[1].ID != c.ID || p.Next != c.ID {
		t.Fatalf("feed: %+v", p)
	}
	p = page(t, b, "/board/feed?limit=2&cursor="+p.Next)
	if len(p.Messages) != 1 || p.Messages[0].ID != a.ID || p.Next != "" {
		t.Fatalf("feed next: %+v", p)
	}
	p = page(t, b, "/board/read?topic=news&limit=1")
	if len(p.Messages) != 1 || p.Messages[0].ID != a.ID {
		t.Fatalf("read: %+v", p)
	}
	p = page(t, b, "/board/read?topic=news&limit=1&cursor="+p.Next)
	if len(p.Messages) != 1 || p.Messages[0].ID != d.ID {
		t.Fatalf("read next: %+v", p)
	}
	p = page(t, b, "/board/search?q=download")
	if len(p.Messages) != 1 || p.Messages[0].ID != a.ID {
		t.Fatal(p)
	}
	p = page(t, b, "/board/search?q=timeout%7Cretry&mode=regex")
	if len(p.Messages) != 2 || p.Messages[0].ID != d.ID {
		t.Fatal(p)
	}
	p = page(t, b, "/board/search?q=timeout%7Cretry")
	if len(p.Messages) != 0 {
		t.Fatal("literal interpreted regex")
	}
	p = page(t, b, "/board/search?q=retry&topic=news&after="+a.ID)
	if len(p.Messages) != 1 || p.Messages[0].ID != d.ID {
		t.Fatal(p)
	}
	p = page(t, b, "/board/feed?topic=news&after="+d.ID)
	if len(p.Messages) != 0 {
		t.Fatal(p)
	}
	w := boardRequest(b, "/board/topics?limit=1")
	var topics struct {
		Topics []struct {
			Topic string `json:"topic"`
			Count int    `json:"count"`
		}
		Next string `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &topics); err != nil {
		t.Fatal(err)
	}
	if len(topics.Topics) != 1 || topics.Topics[0].Topic != "news" || topics.Topics[0].Count != 2 || topics.Next != d.ID {
		t.Fatalf("topics: %s", w.Body)
	}
	w = boardRequest(b, "/board/topics?limit=1&cursor="+topics.Next)
	if !strings.Contains(w.Body.String(), `"topic":"research"`) {
		t.Fatal(w.Body)
	}
}

// Fill directly to exercise scan/retention bounds without bypassing live rate
// limits in production. IDs and expiry have the same form as published messages.
func seedBoard(b *agentBoard, n int) {
	now := b.now().UTC()
	for i := 0; i < n; i++ {
		b.seq++
		id := fmt.Sprintf("%s-%020d", b.boot, b.seq)
		b.messages = append(b.messages, boardMessage{ID: id, Topic: "bulk", Text: "unmatched", CreatedAt: now, ExpiresAt: now.Add(boardRetention), nonce: fmt.Sprint(i), URL: "/board/message?id=" + id})
	}
}

func TestBoardScanContinuation(t *testing.T) {
	b := newAgentBoard()
	seedBoard(b, boardMaxScan+1)
	b.messages[0].Text = "needle"
	p := page(t, b, "/board/search?q=needle")
	if len(p.Messages) != 0 || !p.Partial || p.Next == "" || p.Scanned != boardMaxScan {
		t.Fatal(p)
	}
	p = page(t, b, "/board/search?q=needle&cursor="+p.Next)
	if len(p.Messages) != 1 || p.Partial || p.Next != "" {
		t.Fatal(p)
	}
	b.messages[len(b.messages)-1].Topic = "target"
	p = page(t, b, "/board/read?topic=target")
	if len(p.Messages) != 0 || !p.Partial {
		t.Fatal(p)
	}
	p = page(t, b, "/board/read?topic=target&cursor="+p.Next)
	if len(p.Messages) != 1 || p.Next != "" {
		t.Fatal(p)
	}
}

func TestBoardValidation(t *testing.T) {
	for _, path := range []string{
		"/board/search?q=%5B&mode=regex", "/board/search?q=a&mode=unknown",
		"/board/search", "/board/search?q=" + strings.Repeat("a", 257),
		"/board/read", "/board/read?topic=UPPER", "/board/feed?after=123",
		"/board/feed?limit=0", "/board/feed?limit=101", "/board/feed?limit=no",
		"/board/feed?limit=1&limit=2", "/board/feed?format=html",
		"/board/feed?unknown=x", "/board/feed?topic=%ff", "/board/feed?topic=%ZZ",
		"/board/write?topic=t&text=x", "/board/write?topic=t&nonce=n",
		"/board/write?topic=t&nonce=n&text=%20",
		"/board/write?topic=t&nonce=n&text=" + strings.Repeat("x", 2049),
		"/board/write?topic=t&nonce=" + strings.Repeat("x", 129) + "&text=x",
		"/board/write?topic=t&nonce=n&text=x&name=" + strings.Repeat("x", 81),
	} {
		t.Run(path[:min(len(path), 65)], func(t *testing.T) {
			w := boardRequest(newAgentBoard(), path)
			if w.Code != 400 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
	w := boardRequest(newAgentBoard(), "/board/feed?topic="+strings.Repeat("a", 8192))
	if w.Code != 414 {
		t.Fatal(w.Code)
	}
	if w = boardRequest(newAgentBoard(), "/board/nope"); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestBoardMethodsHeadersAndPrefetch(t *testing.T) {
	b := newAgentBoard()
	for _, method := range []string{"HEAD", "POST", "PUT", "DELETE", "OPTIONS"} {
		r := httptest.NewRequest(method, "/board/write?topic=t&text=x&nonce=n", nil)
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		if w.Code != 405 || w.Header().Get("Allow") != "GET" {
			t.Fatal(w.Code)
		}
	}
	for _, header := range []string{"Purpose", "Sec-Purpose"} {
		r := httptest.NewRequest("GET", "/board/write?topic=t&text=x&nonce=n", nil)
		r.Header.Set(header, "prefetch;prerender")
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	if len(b.messages) != 0 {
		t.Fatal("non-GET or prefetch published")
	}
	for _, path := range []string{"/board", "/board/feed", "/board/search?q=x", "/board/message?id=no", "/board/write?topic=t&text=x&nonce=n"} {
		w := boardRequest(b, path)
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatal(w.Header())
		}
	}
	w := boardRequest(b, "/board/feed?format=text")
	if w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || !json.Valid(w.Body.Bytes()) {
		t.Fatal(w.Header(), w.Body)
	}
}

func TestBoardModerationGETOnly(t *testing.T) {
	b := newAgentBoard()
	b.adminToken = "test-only-not-a-real-secret"
	m := publish(t, b, "research", "remove me", "a")
	for _, suffix := range []string{"", "&token=wrong"} {
		w := boardRequest(b, "/board/remove?id="+m.ID+suffix)
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	w := boardRequest(b, "/board/remove?id="+m.ID+"&token="+b.adminToken)
	if w.Code != 200 || strings.Contains(w.Body.String(), b.adminToken) {
		t.Fatal(w.Body)
	}
	if w = boardRequest(b, m.URL); w.Code != 410 {
		t.Fatal(w.Body)
	}
	if len(page(t, b, "/board/feed").Messages) != 0 || len(page(t, b, "/board/search?q=remove").Messages) != 0 {
		t.Fatal("removed visible")
	}
	if w = boardRequest(b, "/board/write?topic=research&text=remove%20me&nonce=a"); w.Code != 410 {
		t.Fatal(w.Body)
	}
	if w = boardRequest(b, "/board/write?topic=research&text=changed&nonce=a"); w.Code != 409 {
		t.Fatal(w.Body)
	}
	b.blockedTopics["blocked"] = true
	if w = boardRequest(b, "/board/write?topic=blocked&text=x&nonce=b"); w.Code != 403 {
		t.Fatal(w.Body)
	}
	b.adminToken = ""
	if w = boardRequest(b, "/board/remove?id="+m.ID+"&token="); w.Code != 403 {
		t.Fatal(w.Body)
	}
}

func TestBoardRateAndCapacity(t *testing.T) {
	t.Run("write and reset", func(t *testing.T) {
		b := newAgentBoard()
		now := time.Now()
		b.now = func() time.Time { return now }
		for i := 0; i < 10; i++ {
			publish(t, b, "t", fmt.Sprint(i), fmt.Sprint(i))
		}
		w := boardRequest(b, "/board/write?topic=t&text=extra&nonce=extra")
		if w.Code != 429 || w.Header().Get("Retry-After") != "60" || !strings.Contains(w.Body.String(), "retry_after_seconds") {
			t.Fatal(w.Body)
		}
		// Idempotent retries do not consume the write allowance.
		if w = boardRequest(b, "/board/write?topic=t&text=0&nonce=0"); w.Code != 200 {
			t.Fatal(w.Body)
		}
		now = now.Add(time.Minute)
		publish(t, b, "t", "extra", "extra")
	})
	t.Run("topics", func(t *testing.T) {
		b := newAgentBoard()
		for _, topic := range []string{"a", "b", "c"} {
			publish(t, b, topic, "x", "n")
		}
		if w := boardRequest(b, "/board/write?topic=d&text=x&nonce=n"); w.Code != 429 {
			t.Fatal(w.Body)
		}
	})
	t.Run("duplicates", func(t *testing.T) {
		b := newAgentBoard()
		publish(t, b, "t", "x", "a")
		if w := boardRequest(b, "/board/write?topic=t&text=x&nonce=b"); w.Code != 429 {
			t.Fatal(w.Body)
		}
	})
	t.Run("read", func(t *testing.T) {
		b := newAgentBoard()
		for i := 0; i < 120; i++ {
			if w := boardRequest(b, "/board/feed"); w.Code != 200 {
				t.Fatal(w.Body)
			}
		}
		if w := boardRequest(b, "/board/feed"); w.Code != 429 {
			t.Fatal(w.Body)
		}
	})
	t.Run("topic quota", func(t *testing.T) {
		b := newAgentBoard()
		seedBoard(b, boardMaxTopicMessages)
		if w := boardRequest(b, "/board/write?topic=bulk&text=new&nonce=new"); w.Code != 507 {
			t.Fatal(w.Body)
		}
	})
	t.Run("global quota", func(t *testing.T) {
		b := newAgentBoard()
		seedBoard(b, boardMaxMessages)
		if w := boardRequest(b, "/board/write?topic=new&text=new&nonce=new"); w.Code != 507 {
			t.Fatal(w.Body)
		}
	})
	t.Run("peer cap", func(t *testing.T) {
		b := newAgentBoard()
		b.window = b.now()
		for i := 0; i < 4096; i++ {
			b.clients[fmt.Sprint(i)] = &boardClient{}
		}
		if w := boardRequest(b, "/board/feed"); w.Code != 429 {
			t.Fatal(w.Body)
		}
	})
	t.Run("global writes", func(t *testing.T) {
		b := newAgentBoard()
		b.window = b.now()
		b.writes = 120
		if w := boardRequest(b, "/board/write?topic=t&text=new&nonce=new"); w.Code != 429 {
			t.Fatal(w.Body)
		}
	})
}

func TestBoardConcurrentNonce(t *testing.T) {
	b := newAgentBoard()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := boardRequest(b, "/board/write?topic=t&text=same&nonce=same")
			if w.Code != 200 && w.Code != 201 {
				t.Errorf("%d %s", w.Code, w.Body)
			}
		}()
	}
	wg.Wait()
	if len(b.messages) != 1 {
		t.Fatal("duplicate publications", len(b.messages))
	}
}

func TestAgentDiscovery(t *testing.T) {
	mux := http.NewServeMux()
	registerBoard(mux)
	for _, path := range []string{"/agents", "/llms.txt", "/robots.txt", "/board"} {
		w := boardRequest(mux, path)
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), publicBoard.adminToken) && publicBoard.adminToken != "" {
			t.Fatal("secret in docs")
		}
	}
	w := boardRequest(mux, "/agents")
	for _, s := range []string{"platform-feedback", "/board/feed?topic=news", "/board/search", "/board/remove", "public", "untrusted"} {
		if !strings.Contains(w.Body.String(), s) {
			t.Errorf("docs missing %s", s)
		}
	}
	w = boardRequest(mux, "/robots.txt")
	if !strings.Contains(w.Body.String(), "Disallow: /board/write") || !strings.Contains(w.Body.String(), "Disallow: /board/remove") {
		t.Fatal(w.Body)
	}
}

func TestBoardPlainURLWorkflow(t *testing.T) {
	b := newAgentBoard()
	b.adminToken = "local-test-secret"
	s := httptest.NewServer(b)
	defer s.Close()
	get := func(path string, status int) []byte {
		t.Helper()
		// Only a URL: no custom headers, auth, request body or cookies.
		response, err := s.Client().Get(s.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != status {
			t.Fatalf("%s: %d %s", path, response.StatusCode, body)
		}
		return body
	}
	var m boardMessage
	if err := json.Unmarshal(get("/board/write?topic=news&text=Example%20finding&name=unverified&nonce=first", 201), &m); err != nil {
		t.Fatal(err)
	}
	get("/board/write?topic=news&text=Example%20finding&name=unverified&nonce=first", 200)
	get("/board/write?topic=news&text=Follow-up&reply_to="+m.ID+"&nonce=second", 201)
	get("/board/write?topic=platform-feedback&text=Suggestion%3A%20example&nonce=feedback", 201)
	for _, path := range []string{"/board?format=text", "/board/topics?format=text", "/board/read?topic=news&format=text", "/board/feed?topic=news&format=text", "/board/search?q=finding&format=text", m.URL + "&format=text"} {
		if body := get(path, 200); !json.Valid(body) {
			t.Fatalf("invalid response %s", body)
		}
	}
	get("/board/remove?id="+m.ID+"&token="+b.adminToken+"&format=text", 200)
	get(m.URL, 410)
}

type slowBoardWriter struct {
	header  http.Header
	started chan struct{}
	release chan struct{}
}

func (s *slowBoardWriter) Header() http.Header { return s.header }
func (s *slowBoardWriter) WriteHeader(_ int)   {}
func (s *slowBoardWriter) Write(p []byte) (int, error) {
	close(s.started)
	<-s.release
	return len(p), nil
}

func TestBoardSlowClientDoesNotLockStore(t *testing.T) {
	b := newAgentBoard()
	w := &slowBoardWriter{header: make(http.Header), started: make(chan struct{}), release: make(chan struct{})}
	finished := make(chan struct{})
	go func() { defer close(finished); b.ServeHTTP(w, httptest.NewRequest("GET", "/board/feed", nil)) }()
	defer func() { close(w.release); <-finished }()
	<-w.started
	other := make(chan int, 1)
	go func() { other <- boardRequest(b, "/board/feed").Code }()
	select {
	case status := <-other:
		if status != 200 {
			t.Fatal(status)
		}
	case <-time.After(time.Second):
		t.Fatal("slow network client holds store mutex")
	}
}
