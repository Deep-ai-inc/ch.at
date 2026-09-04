package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type testCapability struct {
	Actor  string `json:"actor_id"`
	Name   string `json:"name"`
	Write  string `json:"write_url"`
	Rotate string `json:"rotate_url"`
}

func mintIdentity(t *testing.T, b *agentBoard, name, key string, status int) testCapability {
	t.Helper()
	w := boardRequest(b, "/board/mint?"+url.Values{"name": {name}, "key": {key}}.Encode())
	if w.Code != status {
		t.Fatalf("mint: %d %s", w.Code, w.Body)
	}
	var c testCapability
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if c.Actor == "" || c.Write == "" || c.Rotate == "" || strings.Contains(w.Body.String(), "key_hash") {
		t.Fatal("invalid capability", w.Body)
	}
	return c
}

func TestBoardIdentityContinuity(t *testing.T) {
	b := newAgentBoard()
	key := strings.Repeat("a", 64)
	c := mintIdentity(t, b, "claude", key, 201)
	if retry := mintIdentity(t, b, "claude", key, 200); retry.Actor != c.Actor {
		t.Fatal("mint retry changed actor")
	}
	for _, path := range []string{"/board/mint?name=claude", "/board/mint?name=claude&key=" + strings.Repeat("b", 64)} {
		if w := boardRequest(b, path); w.Code != 409 {
			t.Fatal(w.Body)
		}
	}
	w := boardRequest(b, c.Write+"&topic=t&text=First&nonce=same")
	if w.Code != 201 {
		t.Fatal(w.Body)
	}
	var m boardMessage
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if !m.VerifiedSameActor || m.ActorID != c.Actor || m.Name != "claude" {
		t.Fatal(m)
	}
	// Anonymous claims cannot render as the reserved verified display name.
	w = boardRequest(b, "/board/write?topic=t&name=claude&text=Anonymous&nonce=same")
	if w.Code != 201 {
		t.Fatal(w.Body)
	}
	var anon boardMessage
	if err := json.Unmarshal(w.Body.Bytes(), &anon); err != nil {
		t.Fatal(err)
	}
	if anon.VerifiedSameActor || anon.ActorID != "" || anon.Name != "unverified: claude" {
		t.Fatal(anon)
	}
	newKey := strings.Repeat("b", 64)
	if w = boardRequest(b, c.Rotate+"&new_key="+key); w.Code != 400 {
		t.Fatal("no-op rotation accepted")
	}
	w = boardRequest(b, c.Rotate+"&new_key="+newKey)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	var rotated testCapability
	if err := json.Unmarshal(w.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.Actor != c.Actor {
		t.Fatal("rotation changed actor")
	}
	if w = boardRequest(b, c.Write+"&topic=t&text=First&nonce=same"); w.Code != 403 {
		t.Fatal("old key still works", w.Body)
	}
	if w = boardRequest(b, c.Rotate+"&new_key="+newKey); w.Code != 200 {
		t.Fatal("uncertain rotation cannot retry", w.Body)
	}
	if w = boardRequest(b, rotated.Write+"&topic=t&text=First&nonce=same"); w.Code != 200 {
		t.Fatal("rotation lost nonce", w.Body)
	}
	if w = boardRequest(b, rotated.Write+"&topic=t&name=someone-else&text=x&nonce=x"); w.Code != 400 {
		t.Fatal(w.Body)
	}
	w = boardRequest(b, "/board/identity?actor="+c.Actor)
	if w.Code != 200 || strings.Contains(w.Body.String(), key) || strings.Contains(w.Body.String(), newKey) || strings.Contains(w.Body.String(), "key_hash") {
		t.Fatal(w.Body)
	}
	if w = boardRequest(b, "/board/feed"); strings.Contains(w.Body.String(), "key_hash") || strings.Contains(w.Body.String(), newKey) {
		t.Fatal("feed leaks credentials")
	}
}

func TestBoardIdentityGuards(t *testing.T) {
	for _, path := range []string{"/board/mint?name=Upper", "/board/mint?name=valid&key=weak", "/board/write?topic=t&text=x&nonce=n&actor=bad&key=bad", "/board/rotate?actor=bad&key=bad"} {
		w := boardRequest(newAgentBoard(), path)
		if w.Code != 400 && w.Code != 403 {
			t.Fatal(path, w.Code)
		}
	}
	for _, path := range []string{"/board/mint?name=bot", "/board/rotate?actor=bad&key=bad"} {
		for _, method := range []string{"HEAD", "POST", "GET"} {
			b := newAgentBoard()
			r := httptest.NewRequest(method, path, nil)
			if method == "GET" {
				r.Header.Set("Sec-Purpose", "prefetch")
			}
			w := httptest.NewRecorder()
			b.ServeHTTP(w, r)
			if w.Code != 400 && w.Code != 405 {
				t.Fatal(method, w.Code)
			}
			if len(b.identities) != 0 {
				t.Fatal("speculative mint")
			}
		}
	}
	b := newAgentBoard()
	for i := 0; i < 3; i++ {
		mintIdentity(t, b, fmt.Sprintf("bot-%d", i), "", 201)
	}
	if w := boardRequest(b, "/board/mint?name=fourth"); w.Code != 429 {
		t.Fatal("mint rate limit", w.Body)
	}
}

func TestBoardConcurrentMint(t *testing.T) {
	b := newAgentBoard()
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := boardRequest(b, "/board/mint?name=one&key="+strings.Repeat("a", 64))
			if w.Code != 200 && w.Code != 201 {
				t.Errorf("mint: %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if len(b.identities) != 1 {
		t.Fatal("duplicate name reservations")
	}
}

func TestBoardGeneratedRotationAndCaps(t *testing.T) {
	b := newAgentBoard()
	c := mintIdentity(t, b, "actor", "", 201)
	w := boardRequest(b, c.Rotate+"&format=text")
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Robots-Tag") == "" {
		t.Fatal(w.Code, w.Header())
	}
	var next testCapability
	if err := json.Unmarshal(w.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	if next.Actor != c.Actor || next.Write == c.Write {
		t.Fatal("generated rotation did not replace key")
	}
	if w = boardRequest(b, next.Rotate+"&new_key=weak"); w.Code != 400 {
		t.Fatal(w.Body)
	}
	if w = boardRequest(b, c.Rotate); w.Code != 403 {
		t.Fatal("old rotation key accepted")
	}
	b.writes = 120
	if w = boardRequest(b, next.Rotate); w.Code != 429 {
		t.Fatal(w.Body)
	}
	b = newAgentBoard()
	for i := 0; i < boardMaxIdentities; i++ {
		b.identities[fmt.Sprint(i)] = boardIdentity{}
	}
	if w = boardRequest(b, "/board/mint?name=full"); w.Code != 507 {
		t.Fatal(w.Body)
	}
}

func TestBoardRestartLosesAllState(t *testing.T) {
	b := newAgentBoard()
	key := strings.Repeat("a", 64)
	c := mintIdentity(t, b, "actor", key, 201)
	w := boardRequest(b, c.Write+"&topic=t&text=Original&nonce=one")
	if w.Code != 201 {
		t.Fatal(w.Body)
	}
	var m boardMessage
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	restarted := newAgentBoard()
	if restarted.boot == b.boot {
		t.Fatal("restart reused session")
	}
	if w = boardRequest(restarted, "/board"); !strings.Contains(w.Body.String(), "\"durable\":false") {
		t.Fatal(w.Body)
	}
	if w = boardRequest(restarted, m.URL); w.Code != 404 {
		t.Fatal("post survived restart")
	}
	if w = boardRequest(restarted, c.Write+"&topic=t&text=Original&nonce=one"); w.Code != 403 {
		t.Fatal("capability survived restart")
	}
	if w = boardRequest(restarted, "/board/identity?actor="+c.Actor); w.Code != 404 {
		t.Fatal("identity survived restart")
	}
	replacement := mintIdentity(t, restarted, "actor", key, 201)
	if replacement.Actor == c.Actor {
		t.Fatal("actor ID reused")
	}
	if w = boardRequest(restarted, replacement.Write+"&topic=t&text=Original&nonce=one"); w.Code != 201 {
		t.Fatal("nonce survived restart", w.Body)
	}
}
