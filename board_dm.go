package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	boardMaxDMs           = 10000
	boardMaxMailbox       = 1000 // Retained incoming + outgoing messages per actor.
	boardDMSendsPerMinute = 10
)

// Separate storage and ID namespace: public routes cannot enumerate private data,
// and private traffic does not advance the public message sequence.
type boardDM struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	nonce     string
}

func (m boardDM) involves(actor string) bool { return m.From == actor || m.To == actor }

func (b *agentBoard) validDMCursor(id string) bool {
	prefix := b.boot + "-dm-"
	if !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+20 {
		return false
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(id, prefix), 10, 64)
	// Validate syntax/session, not other mailboxes' existence or traffic count.
	return err == nil && n > 0 && id == fmt.Sprintf("%s%020d", prefix, n)
}

// Called under the same mutex, request limits and expiry sweep as public routes.
func (b *agentBoard) dmRequest(w http.ResponseWriter, r *http.Request, q url.Values, now time.Time, c *boardClient) {
	i, ok := b.authenticate(q.Get("actor"), q.Get("key"))
	if !ok {
		boardError(w, r, 403, "invalid_capability", "Invalid actor capability.")
		return
	}
	if r.URL.Path == "/board/dm/send" {
		b.dmSend(w, r, q, i.ID, now, c)
		return
	}
	after := q.Get("after")
	if after == "0" {
		after = ""
	}
	for _, id := range []string{after, q.Get("cursor")} {
		if id != "" && !b.validDMCursor(id) {
			boardError(w, r, 400, "invalid_cursor", "Use a DM ID from this server session; restart loses identities and messages.")
			return
		}
	}
	after = max(after, q.Get("cursor"))
	with := q.Get("with")
	if with != "" {
		if _, exists := b.identities[with]; !exists {
			boardError(w, r, 404, "not_found", "Unknown identity.")
			return
		}
	}
	limit, err := boardLimit(q)
	if err != nil {
		boardError(w, r, 400, "invalid_limit", err.Error())
		return
	}
	messages := []boardDM{}
	latest, next, incoming := after, "", 0
	for _, m := range b.dms {
		if !m.involves(i.ID) || (with != "" && !((m.From == i.ID && m.To == with) || (m.To == i.ID && m.From == with))) || m.ID <= after {
			continue
		}
		if r.URL.Path == "/board/dm/check" {
			if m.To == i.ID {
				incoming++
				latest = m.ID
			}
			continue
		}
		if len(messages) == limit {
			next = latest
			break
		}
		messages = append(messages, m)
		latest = m.ID
	}
	if r.URL.Path == "/board/dm/check" {
		boardRespond(w, r, 200, map[string]any{"new": incoming, "latest_id": latest})
	} else {
		boardRespond(w, r, 200, map[string]any{"messages": messages, "order": "oldest_first", "next_cursor": next, "latest_id": latest})
	}
}

func (b *agentBoard) dmSend(w http.ResponseWriter, r *http.Request, q url.Values, actor string, now time.Time, c *boardClient) {
	to, text, nonce := q.Get("to"), q.Get("text"), q.Get("nonce")
	if strings.TrimSpace(text) == "" || len(text) > 2048 || len(nonce) < 1 || len(nonce) > 128 {
		boardError(w, r, 400, "invalid_message", "Require text 1–2048 bytes and nonce 1–128 bytes.")
		return
	}
	fromCount, toCount := 0, 0
	for _, m := range b.dms {
		if m.From == actor && m.nonce == nonce {
			if m.To != to || m.Text != text {
				boardError(w, r, 409, "nonce_conflict", "DM nonce already used by this sender with a different recipient or text.")
			} else {
				boardRespond(w, r, 200, m)
			}
			return
		}
		if m.involves(actor) {
			fromCount++
		}
		if m.involves(to) {
			toCount++
		}
	}
	if _, exists := b.identities[to]; !exists {
		boardError(w, r, 404, "not_found", "Unknown recipient identity; use actor_id, not a name.")
		return
	}
	if len(b.dms) >= boardMaxDMs || fromCount >= boardMaxMailbox || toCount >= boardMaxMailbox {
		boardError(w, r, 507, "capacity_reached", "DM retention quota reached; no messages were evicted.")
		return
	}
	if b.dmSenders[actor] >= boardDMSendsPerMinute || c.writes >= 10 || b.writes >= 120 {
		boardError(w, r, 429, "rate_limited", "Sender or shared write limit reached; retry in 60 seconds.")
		return
	}
	b.dmSeq++
	m := boardDM{ID: fmt.Sprintf("%s-dm-%020d", b.boot, b.dmSeq), From: actor, To: to, Text: text, nonce: nonce, CreatedAt: now, ExpiresAt: now.Add(boardRetention)}
	b.dms = append(b.dms, m)
	b.dmSenders[actor]++
	c.writes++
	b.writes++
	boardRespond(w, r, 201, m)
}
