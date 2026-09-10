package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func dmURL(action, actor, key string, fields ...string) string {
	q := url.Values{"actor": {actor}, "key": {key}}
	for i := 0; i < len(fields); i += 2 {
		q.Set(fields[i], fields[i+1])
	}
	return "/board/dm/" + action + "?" + q.Encode()
}

func dmFixture(t *testing.T) (*agentBoard, string, string, string) {
	t.Helper()
	b := newAgentBoard()
	a := mintIdentity(t, b, "alice", strings.Repeat("a", 64), 201).Actor
	z := mintIdentity(t, b, "bob", strings.Repeat("b", 64), 201).Actor
	c := mintIdentity(t, b, "eve", strings.Repeat("c", 64), 201).Actor
	return b, a, z, c
}

func dmSendTest(t *testing.T, b *agentBoard, from, key, to, text, nonce string) boardDM {
	t.Helper()
	w := boardRequest(b, dmURL("send", from, key, "to", to, "text", text, "nonce", nonce))
	if w.Code != 201 {
		t.Fatalf("send: %d %s", w.Code, w.Body)
	}
	var m boardDM
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBoardDMConversationAndPrivacy(t *testing.T) {
	b, a, z, e := dmFixture(t)
	ak, bk, ek := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	m1 := dmSendTest(t, b, a, ak, z, "private-secret-one", "n1")
	m2 := dmSendTest(t, b, z, bk, a, "private-secret-two", "n1")
	dmSendTest(t, b, e, ek, z, "other-conversation", "n1")
	for _, path := range []string{"/board/feed", "/board/search?q=private", "/board/topics", "/board/read?topic=general", "/board/identity?actor=" + a, "/board"} {
		w := boardRequest(b, path)
		if strings.Contains(w.Body.String(), m1.ID) || strings.Contains(w.Body.String(), m1.Text) {
			t.Fatalf("public leak: %s", path)
		}
	}
	if w := boardRequest(b, "/board/message?id="+m1.ID); w.Code != 404 {
		t.Fatal(w.Code, w.Body)
	}
	if b.seq != 0 {
		t.Fatal("DM advanced public sequence")
	}
	for _, action := range []string{"read", "check", "send"} {
		w := boardRequest(b, dmURL(action, a, ek, "with", "unknown"))
		// send rejects the unknown parameter before authentication; no mailbox data.
		if action != "send" && w.Code != 403 {
			t.Fatal(w.Code, w.Body)
		}
	}
	read := dmURL("read", a, ak, "with", z, "limit", "1")
	w := boardRequest(b, read)
	var page struct {
		Messages []boardDM `json:"messages"`
		Next     string    `json:"next_cursor"`
		Latest   string    `json:"latest_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].ID != m1.ID || page.Next != m1.ID {
		t.Fatal(w.Body)
	}
	w = boardRequest(b, read+"&cursor="+page.Next)
	json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Messages) != 1 || page.Messages[0].ID != m2.ID || page.Next != "" || page.Latest != m2.ID {
		t.Fatal(w.Body)
	}
	w = boardRequest(b, dmURL("read", e, ek))
	if strings.Contains(w.Body.String(), m1.ID) || strings.Contains(w.Body.String(), m2.ID) {
		t.Fatal("third party leak", w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatal(w.Header())
	}
	w = boardRequest(b, dmURL("check", a, ak, "after", "0"))
	var check struct {
		New    int    `json:"new"`
		Latest string `json:"latest_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &check)
	if check.New != 1 || check.Latest != m2.ID {
		t.Fatal(w.Body)
	}
	w = boardRequest(b, dmURL("check", a, ak, "after", m2.ID))
	json.Unmarshal(w.Body.Bytes(), &check)
	if check.New != 0 || check.Latest != m2.ID {
		t.Fatal(w.Body)
	}
	if w = boardRequest(b, dmURL("read", a, ak, "after", "bad")); w.Code != 400 {
		t.Fatal(w.Body)
	}
	if w = boardRequest(b, dmURL("read", a, ak, "limit", "101")); w.Code != 400 {
		t.Fatal(w.Body)
	}
	w = boardRequest(b, dmURL("read", a, ak, "with", a))
	json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Messages) != 0 {
		t.Fatal("self filter included other conversations", w.Body)
	}
	w = boardRequest(b, dmURL("check", z, bk, "with", a))
	json.Unmarshal(w.Body.Bytes(), &check)
	if check.New != 1 || check.Latest != m1.ID {
		t.Fatal("filtered incoming count", w.Body)
	}
	for _, action := range []string{"read", "check", "send"} {
		if w := boardRequest(b, "/board/dm/"+action); w.Code != 403 {
			t.Fatal("anonymous DM access", w.Code, w.Body)
		}
	}
}

func TestBoardDMRetriesLimitsAndExpiry(t *testing.T) {
	b, a, z, e := dmFixture(t)
	key := strings.Repeat("a", 64)
	now := b.now()
	b.now = func() time.Time { return now }
	target := dmURL("send", a, key, "to", z, "text", "hello", "nonce", "n")
	m := dmSendTest(t, b, a, key, z, "hello", "n")
	m = b.dms[0] // Preserve the server-only nonce when constructing capacity fixtures.
	if w := boardRequest(b, target); w.Code != 200 || !strings.Contains(w.Body.String(), m.ID) {
		t.Fatal(w.Code, w.Body)
	}
	for _, fields := range [][]string{{"to", e, "text", "hello", "nonce", "n"}, {"to", z, "text", "changed", "nonce", "n"}} {
		if w := boardRequest(b, dmURL("send", a, key, fields...)); w.Code != 409 {
			t.Fatal(w.Code, w.Body)
		}
	}
	if w := boardRequest(b, dmURL("send", a, key, "to", "unknown", "text", "hi", "nonce", "unknown")); w.Code != 404 {
		t.Fatal(w.Code, w.Body)
	}
	if w := boardRequest(b, dmURL("send", a, strings.Repeat("b", 64), "to", z, "text", "hi", "nonce", "badkey")); w.Code != 403 {
		t.Fatal(w.Code, w.Body)
	}
	b.dmSenders[a] = boardDMSendsPerMinute
	if w := boardRequest(b, dmURL("send", a, key, "to", z, "text", "hi", "nonce", "new")); w.Code != 429 {
		t.Fatal(w.Code, w.Body)
	}
	if w := boardRequest(b, target); w.Code != 200 {
		t.Fatal("retry charged write quota", w.Body)
	}
	now = now.Add(time.Minute)
	b.dms = make([]boardDM, boardMaxMailbox)
	for i := range b.dms {
		b.dms[i] = boardDM{From: e, To: z, ExpiresAt: now.Add(boardRetention)}
	}
	if w := boardRequest(b, dmURL("send", a, key, "to", z, "text", "hi", "nonce", "full")); w.Code != 507 {
		t.Fatal(w.Code, w.Body)
	}
	b.dms = []boardDM{m}
	// Exact retries work at capacity; unrelated new writes do not evict anything.
	for len(b.dms) < boardMaxDMs {
		b.dms = append(b.dms, boardDM{From: e, To: e, ExpiresAt: now.Add(boardRetention)})
	}
	if w := boardRequest(b, target); w.Code != 200 {
		t.Fatal("capacity broke retry", w.Body)
	}
	if w := boardRequest(b, dmURL("send", a, key, "to", a, "text", "self", "nonce", "self")); w.Code != 507 {
		t.Fatal("global cap", w.Code, w.Body)
	}
	b.dms = []boardDM{m}
	now = now.Add(boardRetention)
	if w := boardRequest(b, dmURL("read", z, strings.Repeat("b", 64))); w.Code != 200 || strings.Contains(w.Body.String(), m.ID) {
		t.Fatal(w.Code, w.Body)
	}
	if w := boardRequest(b, target); w.Code != 201 {
		t.Fatal("expired nonce not freed", w.Body)
	}
	fresh := newAgentBoard()
	if w := boardRequest(fresh, dmURL("read", a, key)); w.Code != 403 {
		t.Fatal("restart kept identity", w.Body)
	}
	if fresh.validDMCursor(m.ID) {
		t.Fatal("restart accepted cursor")
	}
}

func TestBoardDMConcurrentRetryAndGETOnly(t *testing.T) {
	b, a, z, _ := dmFixture(t)
	u := dmURL("send", a, strings.Repeat("a", 64), "to", z, "text", "hello", "nonce", "same")
	for _, method := range []string{"HEAD", "POST", "PUT", "DELETE"} {
		r := httptest.NewRequest(method, u, nil)
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		if w.Code != 405 {
			t.Fatal(w.Code)
		}
	}
	r := httptest.NewRequest("GET", u, nil)
	r.Header.Set("Purpose", "prefetch")
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)
	if w.Code != 400 || len(b.dms) != 0 {
		t.Fatal("speculative send executed")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := boardRequest(b, u)
			if w.Code != 200 && w.Code != 201 {
				t.Error(w.Code, w.Body)
			}
		}()
	}
	wg.Wait()
	if len(b.dms) != 1 {
		t.Fatal("retry duplicated messages")
	}
}

func TestBoardDMSenderBudgetAcrossPeers(t *testing.T) {
	b, a, z, _ := dmFixture(t)
	key := strings.Repeat("a", 64)
	for n := 0; n <= boardDMSendsPerMinute; n++ {
		r := httptest.NewRequest("GET", dmURL("send", a, key, "to", z, "text", "hello", "nonce", fmt.Sprint(n)), nil)
		r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", n+10)
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		want := 201
		if n == boardDMSendsPerMinute {
			want = 429
		}
		if w.Code != want {
			t.Fatal(n, w.Code, w.Body)
		}
	}
	if len(b.dms) != boardDMSendsPerMinute {
		t.Fatal("sender budget bypassed")
	}
}

func TestBoardDMTransportParity(t *testing.T) {
	b, a, z, _ := dmFixture(t)
	key := strings.Repeat("a", 64)
	u := dmURL("send", a, key, "to", z, "text", "雪 & + ? private", "nonce", "wire")
	addr := startTestBoardDNS(t, b)
	query := dnsEDNS(u)
	m, _, err := (&dns.Client{Net: "udp", Timeout: 3 * time.Second}).Exchange(query, addr)
	if err != nil || !m.Truncated {
		t.Fatal("expected TCP before mutation", err, m)
	}
	b.mu.Lock()
	count := len(b.dms)
	b.mu.Unlock()
	if count != 0 {
		t.Fatal("UDP executed send")
	}
	m, _, err = (&dns.Client{Net: "tcp", Timeout: 3 * time.Second}).Exchange(query, addr)
	if err != nil {
		t.Fatal(err)
	}
	envelope(t, dnsBoardBody(t, m), 201)
	status, body := boardTransportReply(b, "GET https://ch.at"+u, "192.0.2.2:12")
	if status != 200 {
		t.Fatal(status, string(body))
	}
	for _, action := range []string{"read", "check"} {
		path := dmURL(action, z, strings.Repeat("b", 64), "format", "text")
		status, body = boardTransportReply(b, path, "192.0.2.2:12")
		if status != 200 {
			t.Fatal(status, string(body))
		}
		m, _, err = (&dns.Client{Net: "tcp", Timeout: 3 * time.Second}).Exchange(dnsEDNS(path), addr)
		if err != nil {
			t.Fatal(err)
		}
		envelope(t, dnsBoardBody(t, m), 200)
	}
	for _, n := range []int{0, 2049} {
		w := boardRequest(b, dmURL("send", a, key, "to", z, "text", strings.Repeat("x", n), "nonce", fmt.Sprint(n)))
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body)
		}
	}
}
