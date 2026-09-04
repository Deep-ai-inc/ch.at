package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func openTestStore(t *testing.T, path string) *agentBoard {
	t.Helper()
	b, err := openBoardStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.closeStore)
	return b
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

func TestBoardDurableRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.jsonl")
	b := openTestStore(t, path)
	key := strings.Repeat("a", 64)
	c := mintIdentity(t, b, "researcher", key, 201)
	w := boardRequest(b, c.Write+"&topic=research&text=Measured&nonce=original")
	if w.Code != 201 {
		t.Fatal(w.Body)
	}
	var m boardMessage
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	removed := publish(t, b, "research", "Remove this", "removed")
	b.adminToken = "test-admin"
	if w = boardRequest(b, "/board/remove?id="+removed.ID+"&token=test-admin"); w.Code != 200 {
		t.Fatal(w.Body)
	}
	newKey := strings.Repeat("b", 64)
	if w = boardRequest(b, c.Rotate+"&new_key="+newKey); w.Code != 200 {
		t.Fatal(w.Body)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{key, newKey, "test-admin"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("raw secret persisted")
		}
	}
	oldSession, oldSeq := b.boot, b.seq
	b.closeStore()
	restarted := openTestStore(t, path)
	if restarted.boot != oldSession || restarted.seq != oldSeq {
		t.Fatal("lost stable IDs")
	}
	if w = boardRequest(restarted, m.URL); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w = boardRequest(restarted, removed.URL); w.Code != 410 {
		t.Fatal("removed post resurrected", w.Body)
	}
	if _, ok := restarted.authenticate(c.Actor, key); ok {
		t.Fatal("rotation lost on restart")
	}
	if _, ok := restarted.authenticate(c.Actor, newKey); !ok {
		t.Fatal("identity lost on restart")
	}
	cap := mintIdentity(t, restarted, "researcher", newKey, 200)
	if w = boardRequest(restarted, cap.Write+"&topic=research&text=Measured&nonce=original"); w.Code != 200 {
		t.Fatal("nonce lost", w.Body)
	}
	if w = boardRequest(restarted, "/board/write?topic=research&text=Remove%20this&nonce=removed"); w.Code != 410 {
		t.Fatal("removed nonce lost", w.Body)
	}
	next := publish(t, restarted, "research", "Next", "next")
	if next.ID <= removed.ID {
		t.Fatal("sequence reused")
	}
}

func TestBoardJournalRecoveryAndLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.jsonl")
	b := openTestStore(t, path)
	m := publish(t, b, "t", "Good", "good")
	if other, err := openBoardStore(path); err == nil {
		other.closeStore()
		t.Fatal("second writer acquired store")
	}
	b.closeStore()
	// Simulate a process dying midway through the final record.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(`{"type":"message","message":`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	b = openTestStore(t, path)
	if w := boardRequest(b, m.URL); w.Code != 200 {
		t.Fatal(w.Body)
	}
	publish(t, b, "t", "After recovery", "after")
	b.closeStore()
	// A malformed complete record is corruption, not a tail to silently discard.
	f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("not-json\n")
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if broken, err := openBoardStore(path); err == nil {
		broken.closeStore()
		t.Fatal("corruption silently ignored")
	}
}

func TestBoardStorageFailureDoesNotPublish(t *testing.T) {
	for _, operation := range []string{"write", "mint", "rotate", "remove"} {
		t.Run(operation, func(t *testing.T) {
			b := openTestStore(t, filepath.Join(t.TempDir(), "board.jsonl"))
			m := publish(t, b, "t", "original", "original")
			c := mintIdentity(t, b, "actor", strings.Repeat("a", 64), 201)
			b.adminToken = "operator"
			seq := b.seq
			// fsync failure can leave a replayable record, but must not ACK/apply it.
			b.journal.syncFile = func(*os.File) error { return errors.New("simulated disk failure") }
			paths := map[string]string{"write": "/board/write?topic=t&text=not-acknowledged&nonce=new", "mint": "/board/mint?name=new", "rotate": c.Rotate + "&new_key=" + strings.Repeat("b", 64), "remove": "/board/remove?id=" + m.ID + "&token=operator"}
			w := boardRequest(b, paths[operation])
			if w.Code != 503 {
				t.Fatal(w.Code, w.Body)
			}
			if len(b.messages) != 1 || b.messages[0].removed || b.seq != seq || len(b.identities) != 1 {
				t.Fatal("failed mutation applied")
			}
			if _, ok := b.authenticate(c.Actor, strings.Repeat("a", 64)); !ok {
				t.Fatal("failed rotation applied")
			}
			if w = boardRequest(b, "/board/write?topic=t&text=blocked&nonce=blocked"); w.Code != 503 {
				t.Fatal("writes continued after storage failure")
			}
		})
	}
}

func TestBoardCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.jsonl")
	b := openTestStore(t, path)
	old := publish(t, b, "t", "EXPIRED-BODY", "old")
	c := mintIdentity(t, b, "persistent-actor", strings.Repeat("a", 64), 201)
	now := time.Now().Add(boardRetention + time.Hour)
	b.now = func() time.Time { return now }
	b.journal.compactAt = 1 // Force automatic compaction before the next append.
	m := publish(t, b, "t", "Retained", "retained")
	if m.ID <= old.ID {
		t.Fatal("ID high-water lost")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "EXPIRED-BODY") {
		t.Fatal("compaction retained expired payload")
	}
	b.closeStore()
	b = openTestStore(t, path)
	if w := boardRequest(b, old.URL); w.Code != 410 {
		t.Fatal(w.Body)
	}
	if w := boardRequest(b, m.URL); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if _, ok := b.authenticate(c.Actor, strings.Repeat("a", 64)); !ok {
		t.Fatal("compaction lost identity")
	}
	if b.seq != 2 {
		t.Fatal("compaction lost sequence")
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

func TestBoardInitializeDurableAndFailClosed(t *testing.T) {
	original := publicBoard
	defer func() { publicBoard = original }()
	path := filepath.Join(t.TempDir(), "board.jsonl")
	t.Setenv("BOARD_LOG_PATH", path)
	if err := initializeBoardStorage(); err != nil {
		t.Fatal(err)
	}
	b := publicBoard
	defer b.closeStore()
	if b.journal == nil {
		t.Fatal("production initializer used RAM only")
	}
	w := boardRequest(b, "/board")
	if !strings.Contains(w.Body.String(), `"durable":true`) {
		t.Fatal(w.Body)
	}
	t.Setenv("BOARD_LOG_PATH", filepath.Join(t.TempDir(), "missing-directory", "board.jsonl"))
	if err := initializeBoardStorage(); err == nil {
		t.Fatal("unwritable store accepted")
	}
	if publicBoard != b {
		t.Fatal("failed initialization replaced store")
	}
}

func TestBoardCompactionFailurePreservesCommittedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.jsonl")
	b := openTestStore(t, path)
	m := publish(t, b, "t", "Committed", "one")
	b.journal.compactAt = 1
	b.journal.syncFile = func(*os.File) error { return errors.New("compaction sync failed") }
	if w := boardRequest(b, "/board/write?topic=t&text=Uncommitted&nonce=two"); w.Code != 503 {
		t.Fatal(w.Body)
	}
	b.closeStore()
	reopened := openTestStore(t, path)
	if len(reopened.messages) != 1 || reopened.messages[0].ID != m.ID {
		t.Fatal("failed compaction damaged committed journal")
	}
	files, err := filepath.Glob(path + ".compact-*")
	if err != nil || len(files) != 0 {
		t.Fatal("failed compaction left temporary file", err, files)
	}
}
