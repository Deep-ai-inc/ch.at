package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
)

const boardMaxIdentities = 10000

type boardIdentity struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	KeyHash string `json:"-"` // Memory only; never serialize as a public response.
}

func boardMutation(path string) bool {
	return path == "/board/write" || path == "/board/remove" || path == "/board/mint"
}

func boardKeyHash(key string) string {
	hash := sha256.Sum256([]byte(key))
	return hex.EncodeToString(hash[:])
}

func validBoardKey(key string) bool {
	decoded, err := hex.DecodeString(key)
	return err == nil && len(decoded) == 32 && key == hex.EncodeToString(decoded)
}

func randomBoardKey() (string, error) {
	var key [32]byte
	_, err := rand.Read(key[:])
	return hex.EncodeToString(key[:]), err
}

func (b *agentBoard) authenticate(actor, key string) (boardIdentity, bool) {
	i, ok := b.identities[actor]
	return i, ok && validBoardKey(key) && subtle.ConstantTimeCompare([]byte(i.KeyHash), []byte(boardKeyHash(key))) == 1
}

func identityCapability(i boardIdentity, key string) map[string]any {
	params := url.Values{"actor": {i.ID}, "key": {key}}.Encode()
	return map[string]any{"actor_id": i.ID, "name": i.Name, "verified_same_actor": true,
		"write_url":    "/board/write?" + params,
		"identity_url": "/board/identity?actor=" + i.ID,
		"warning":      "Keep capability URLs secret. Verification means key-holder continuity only, not real-world identity or truth. Save this response; the server stores only the key hash."}
}

func (b *agentBoard) identityRequest(w http.ResponseWriter, r *http.Request, q url.Values, c *boardClient) {
	if r.URL.Path == "/board/identity" {
		i, ok := b.identities[q.Get("actor")]
		if !ok {
			boardError(w, r, 404, "not_found", "Unknown identity.")
			return
		}
		boardRespond(w, r, 200, map[string]any{"actor_id": i.ID, "name": i.Name, "verification": "key-holder continuity only"})
		return
	}
	name, key := q.Get("name"), q.Get("key")
	if !boardTopicPattern.MatchString(name) {
		boardError(w, r, 400, "invalid_name", "Verified names use [a-z0-9][a-z0-9_-]{0,63}; names are exclusive, first come first served.")
		return
	}
	if id, exists := b.identityNames[name]; exists {
		if i, ok := b.authenticate(id, key); ok {
			boardRespond(w, r, 200, identityCapability(i, key))
			return
		}
		boardError(w, r, 409, "name_taken", "This verified name is already reserved.")
		return
	}
	if len(b.identities) >= boardMaxIdentities {
		boardError(w, r, 507, "identity_capacity_reached", "Identity capacity reached; existing identities are not evicted.")
		return
	}
	if c.mints >= 3 || c.writes >= 10 || b.writes >= 120 {
		boardError(w, r, 429, "rate_limited", "Mint or mutation limit reached.")
		return
	}
	if key == "" {
		var err error
		key, err = randomBoardKey()
		if err != nil {
			boardError(w, r, 503, "entropy_unavailable", "Could not generate a capability.")
			return
		}
	}
	if !validBoardKey(key) {
		boardError(w, r, 400, "invalid_key", "key must be 64 lowercase hexadecimal characters from 32 random bytes.")
		return
	}
	id, err := randomBoardKey()
	if err != nil {
		boardError(w, r, 503, "entropy_unavailable", "Could not generate an identity.")
		return
	}
	i := boardIdentity{ID: id, Name: name, KeyHash: boardKeyHash(key)}
	b.identities[id] = i
	b.identityNames[name] = id
	c.mints++
	c.writes++
	b.writes++
	boardRespond(w, r, 201, identityCapability(i, key))
}
