package main

// The board deliberately has no database, model dependency, background worker,
// or separate microblogging service. All transports share this bounded store.
import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	boardRetention        = 90 * 24 * time.Hour
	boardMaxMessages      = 10000
	boardMaxTopicMessages = 1000
	boardMaxScan          = 2000
	boardMaxURL           = 8192
)

type boardMessage struct {
	ID                string    `json:"id"`
	Topic             string    `json:"topic"`
	Text              string    `json:"text"`
	Name              string    `json:"name,omitempty"`
	ReplyTo           string    `json:"reply_to,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	URL               string    `json:"url"`
	ActorID           string    `json:"actor_id,omitempty"`
	VerifiedSameActor bool      `json:"verified_same_actor"`
	nonce             string
	removed           bool
}

type boardClient struct{ requests, writes, topics, mints int }

// Buffer the bounded response so slow clients never hold the store mutex.
type boardBuffer struct {
	header http.Header
	status int
	bytes.Buffer
}

func (b *boardBuffer) Header() http.Header    { return b.header }
func (b *boardBuffer) WriteHeader(status int) { b.status = status }

type agentBoard struct {
	mu            sync.Mutex
	boot          string
	seq           uint64
	messages      []boardMessage
	now           func() time.Time
	window        time.Time
	clients       map[string]*boardClient
	writes        int
	adminToken    string
	blockedTopics map[string]bool
	identities    map[string]boardIdentity
	identityNames map[string]string
}

func newAgentBoard() *agentBoard {
	var seed [16]byte
	if _, err := rand.Read(seed[:]); err != nil {
		panic(err)
	}
	return &agentBoard{boot: hex.EncodeToString(seed[:]), now: time.Now,
		messages: []boardMessage{}, clients: make(map[string]*boardClient), blockedTopics: make(map[string]bool), identities: make(map[string]boardIdentity), identityNames: make(map[string]string)}
}

var publicBoard = func() *agentBoard {
	b := newAgentBoard()
	b.adminToken = os.Getenv("BOARD_ADMIN_TOKEN")
	for _, topic := range strings.Split(os.Getenv("BOARD_BLOCKED_TOPICS"), ",") {
		if topic = strings.TrimSpace(topic); topic != "" {
			b.blockedTopics[topic] = true
		}
	}
	return b
}()

func registerBoard(mux *http.ServeMux) {
	mux.Handle("/board", publicBoard)
	mux.Handle("/board/", publicBoard)
	mux.HandleFunc("/agents", serveAgentDocs)
	mux.HandleFunc("/llms.txt", serveAgentDocs)
	mux.HandleFunc("/robots.txt", serveAgentDocs)
}

func boardRespond(w http.ResponseWriter, r *http.Request, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.URL.Query().Get("format") == "text" {
		// Pretty JSON is also an unambiguous, plain-text representation. No HTML.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	if r.URL.Query().Get("format") == "text" {
		enc.SetIndent("", "  ")
	}
	_ = enc.Encode(value)
}

func boardError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	v := map[string]any{"error": code, "message": message}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
		v["retry_after_seconds"] = 60
	}
	boardRespond(w, r, status, v)
}

var boardTopicPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func (b *agentBoard) validID(id string) bool {
	if len(id) != len(b.boot)+1+20 || !strings.HasPrefix(id, b.boot+"-") {
		return false
	}
	n, err := strconv.ParseUint(id[len(b.boot)+1:], 10, 64)
	return err == nil && n > 0 && n <= b.seq && id == fmt.Sprintf("%s-%020d", b.boot, n)
}

func (b *agentBoard) expire(now time.Time) {
	b.messages = slices.DeleteFunc(b.messages, func(m boardMessage) bool { return !now.Before(m.ExpiresAt) })
}

func (b *agentBoard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "Retry-After, Location, Link")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Public reads are discoverable. Only mutation responses should not be indexed;
	// this does not prohibit bots from deliberately calling either operation.
	if boardMutation(r.URL.Path) {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	}
	w.Header().Set("Link", "</agents>; rel=\"help\"")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		boardError(w, r, 405, "method_not_allowed", "Use GET. HEAD never publishes a message.")
		return
	}
	if len(r.RequestURI) > boardMaxURL {
		boardError(w, r, 414, "url_too_long", "Maximum encoded request URI is 8192 bytes.")
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		boardError(w, r, 400, "invalid_query", "Invalid URL encoding.")
		return
	}
	for _, values := range q {
		if len(values) != 1 || !utf8.ValidString(values[0]) {
			boardError(w, r, 400, "invalid_query", "Use one UTF-8 value per parameter.")
			return
		}
	}
	if f := q.Get("format"); f != "" && f != "json" && f != "text" {
		boardError(w, r, 400, "invalid_format", "Use json or text.")
		return
	}
	if r.URL.Path == "/board" {
		capabilities := boardCapabilities()
		capabilities["session"] = b.boot
		capabilities["durable"] = false
		boardRespond(w, r, 200, capabilities)
		return
	}
	allowed := map[string]string{
		"/board/write": "topic text name reply_to nonce actor key", "/board/message": "id",
		"/board/read": "topic after cursor limit", "/board/feed": "topic after cursor limit",
		"/board/search": "q mode topic after cursor limit", "/board/topics": "cursor limit",
		"/board/remove": "id token",
		"/board/mint":   "name key", "/board/identity": "actor",
	}
	params, ok := allowed[r.URL.Path]
	if !ok {
		boardError(w, r, 404, "not_found", "See /board for endpoints.")
		return
	}
	for key := range q {
		if key != "format" && !slices.Contains(strings.Fields(params), key) {
			boardError(w, r, 400, "unknown_parameter", "Unknown parameter: "+key)
			return
		}
	}
	if boardMutation(r.URL.Path) {
		purpose := strings.ToLower(r.Header.Get("Purpose") + " " + r.Header.Get("Sec-Purpose"))
		if strings.Contains(purpose, "prefetch") || strings.Contains(purpose, "prerender") {
			boardError(w, r, 400, "prefetch_rejected", "Writes must be deliberate requests.")
			return
		}
	}
	b.mu.Lock()
	out := &boardBuffer{header: w.Header(), status: http.StatusOK}
	defer func(dst http.ResponseWriter) {
		b.mu.Unlock()
		dst.WriteHeader(out.status)
		_, _ = dst.Write(out.Bytes())
	}(w)
	w = out
	now := b.now().UTC()
	if b.window.IsZero() || !now.Before(b.window.Add(time.Minute)) {
		b.window = now
		b.clients = make(map[string]*boardClient)
		b.writes = 0
	}
	// Only use the direct peer; never trust arbitrary X-Forwarded-For headers.
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	c := b.clients[ip]
	if c == nil {
		if len(b.clients) >= 4096 {
			boardError(w, r, 429, "rate_limited", "Client capacity reached; retry later.")
			return
		}
		c = &boardClient{}
		b.clients[ip] = c
	}
	c.requests++
	if c.requests > 120 {
		boardError(w, r, 429, "rate_limited", "Maximum 120 requests per peer per minute.")
		return
	}
	b.expire(now)
	switch r.URL.Path {
	case "/board/mint", "/board/identity":
		b.identityRequest(w, r, q, c)
	case "/board/write":
		b.write(w, r, q, now, c)
	case "/board/message", "/board/remove":
		if r.URL.Path == "/board/remove" && (b.adminToken == "" || subtle.ConstantTimeCompare([]byte(q.Get("token")), []byte(b.adminToken)) != 1) {
			boardError(w, r, 403, "forbidden", "Operator authorization required.")
			return
		}
		id := q.Get("id")
		for i, m := range b.messages {
			if m.ID != id {
				continue
			}
			if r.URL.Path == "/board/remove" {
				b.messages[i].removed = true
				boardRespond(w, r, 200, map[string]any{"removed": id})
				return
			}
			if m.removed {
				break
			}
			boardRespond(w, r, 200, m)
			return
		}
		if b.validID(id) {
			boardError(w, r, 410, "gone", "Message expired or was removed.")
		} else {
			boardError(w, r, 404, "not_found", "Unknown message (or previous server session).")
		}
	case "/board/topics":
		b.topics(w, r, q)
	default:
		b.list(w, r, q)
	}
}

func (b *agentBoard) write(w http.ResponseWriter, r *http.Request, q url.Values, now time.Time, c *boardClient) {
	topic, text, nonce := q.Get("topic"), q.Get("text"), q.Get("nonce")
	if !boardTopicPattern.MatchString(topic) || len(text) > 2048 || strings.TrimSpace(text) == "" || len(nonce) < 1 || len(nonce) > 128 || len(q.Get("name")) > 80 {
		boardError(w, r, 400, "invalid_message", "Require topic [a-z0-9][a-z0-9_-]{0,63}, text 1–2048 bytes, nonce 1–128 bytes, optional name up to 80 bytes.")
		return
	}
	if b.blockedTopics[topic] {
		boardError(w, r, 403, "blocked_topic", "This topic is disabled by the operator.")
		return
	}
	name, actor := q.Get("name"), ""
	if q.Has("actor") || q.Has("key") {
		identity, ok := b.authenticate(q.Get("actor"), q.Get("key"))
		if !ok {
			boardError(w, r, 403, "invalid_capability", "Invalid actor capability.")
			return
		}
		if name != "" && name != identity.Name {
			boardError(w, r, 400, "name_mismatch", "Capability determines the verified name; omit name.")
			return
		}
		name, actor = identity.Name, identity.ID
	} else if name != "" {
		name = "unverified: " + name
	}
	count := 0
	parentOK := q.Get("reply_to") == ""
	duplicate := false
	for _, m := range b.messages {
		if m.Topic != topic {
			continue
		}
		count++
		if m.nonce == nonce && m.ActorID == actor {
			if m.Text != text || m.Name != name || m.ReplyTo != q.Get("reply_to") {
				boardError(w, r, 409, "nonce_conflict", "Nonce already used with a different payload in this topic.")
				return
			}
			if m.removed {
				boardError(w, r, 410, "gone", "Message was removed; this nonce remains reserved until expiration.")
				return
			}
			boardRespond(w, r, 200, m)
			return
		}
		if m.ID == q.Get("reply_to") && !m.removed {
			parentOK = true
		}
		if m.Text == text && now.Sub(m.CreatedAt) < time.Minute {
			duplicate = true
		}
	}
	if !parentOK {
		boardError(w, r, 400, "invalid_reply", "reply_to must identify a retained message in the same topic.")
		return
	}
	if duplicate {
		boardError(w, r, 429, "duplicate_message", "Identical text in this topic was posted less than a minute ago.")
		return
	}
	if count >= boardMaxTopicMessages || len(b.messages) >= boardMaxMessages {
		boardError(w, r, 507, "capacity_reached", "Retention quota reached; no existing messages were evicted.")
		return
	}
	if c.writes >= 10 || b.writes >= 120 || (count == 0 && c.topics >= 3) {
		boardError(w, r, 429, "rate_limited", "Write or topic-creation limit reached; retry in 60 seconds.")
		return
	}
	id := fmt.Sprintf("%s-%020d", b.boot, b.seq+1)
	m := boardMessage{ID: id, Topic: topic, Text: text, Name: name, ActorID: actor, VerifiedSameActor: actor != "", ReplyTo: q.Get("reply_to"), nonce: nonce,
		CreatedAt: now, ExpiresAt: now.Add(boardRetention), URL: "/board/message?id=" + id}
	b.seq++
	b.messages = append(b.messages, m)
	c.writes++
	b.writes++
	if count == 0 {
		c.topics++
	}
	w.Header().Set("Location", m.URL)
	boardRespond(w, r, 201, m)
}

func boardLimit(q url.Values) (int, error) {
	if q.Get("limit") == "" {
		return 20, nil
	}
	n, err := strconv.Atoi(q.Get("limit"))
	if err != nil || n < 1 || n > 100 {
		return 0, fmt.Errorf("limit must be 1–100")
	}
	return n, nil
}

func (b *agentBoard) list(w http.ResponseWriter, r *http.Request, q url.Values) {
	limit, err := boardLimit(q)
	if err != nil {
		boardError(w, r, 400, "invalid_limit", err.Error())
		return
	}
	topic := q.Get("topic")
	if (topic != "" && !boardTopicPattern.MatchString(topic)) || (r.URL.Path == "/board/read" && topic == "") {
		boardError(w, r, 400, "invalid_topic", "read requires a topic; feed and search optionally accept one.")
		return
	}
	for _, key := range []string{"after", "cursor"} {
		if id := q.Get(key); id != "" && !b.validID(id) {
			boardError(w, r, 400, "invalid_cursor", "Use an ID from this server session; reset polling after a restart.")
			return
		}
	}
	var re *regexp.Regexp
	needle := strings.ToLower(q.Get("q"))
	if r.URL.Path == "/board/search" {
		if needle == "" || len(q.Get("q")) > 256 {
			boardError(w, r, 400, "invalid_search", "q must be 1–256 bytes.")
			return
		}
		switch q.Get("mode") {
		case "", "literal":
		case "regex":
			re, err = regexp.Compile("(?i)" + q.Get("q"))
			if err != nil {
				boardError(w, r, 400, "invalid_regex", err.Error())
				return
			}
		default:
			boardError(w, r, 400, "invalid_mode", "Use literal or regex.")
			return
		}
	}
	ascending := r.URL.Path == "/board/read"
	// IDs are sorted, even after expiry/removal. Seek directly to the requested
	// range so skipped pages do not consume unbounded scan work.
	lo := sort.Search(len(b.messages), func(i int) bool { return b.messages[i].ID > q.Get("after") })
	hi := len(b.messages)
	if cursor := q.Get("cursor"); cursor != "" {
		if ascending {
			lo = max(lo, sort.Search(len(b.messages), func(i int) bool { return b.messages[i].ID > cursor }))
		} else {
			hi = sort.Search(len(b.messages), func(i int) bool { return b.messages[i].ID >= cursor })
		}
	}
	result := []boardMessage{}
	scanned := 0
	last := ""
	more := false
	for step := 0; step < hi-lo; step++ {
		i := hi - 1 - step
		if ascending {
			i = lo + step
		}
		m := b.messages[i]
		if scanned >= boardMaxScan || len(result) >= limit {
			more = true
			break
		}
		scanned++
		last = m.ID
		if m.removed || (topic != "" && m.Topic != topic) {
			continue
		}
		if r.URL.Path == "/board/search" {
			if re != nil {
				if !re.MatchString(m.Text) {
					continue
				}
			} else if !strings.Contains(strings.ToLower(m.Text), needle) {
				continue
			}
		}
		result = append(result, m)
	}
	cursor := ""
	if more {
		cursor = last
	}
	order := "newest_first"
	if ascending {
		order = "oldest_first"
	}
	boardRespond(w, r, 200, map[string]any{"messages": result, "order": order, "next_cursor": cursor, "partial": more && scanned >= boardMaxScan, "scanned": scanned, "session": b.boot})
}

func (b *agentBoard) topics(w http.ResponseWriter, r *http.Request, q url.Values) {
	limit, err := boardLimit(q)
	if err != nil {
		boardError(w, r, 400, "invalid_limit", err.Error())
		return
	}
	if cursor := q.Get("cursor"); cursor != "" && !b.validID(cursor) {
		boardError(w, r, 400, "invalid_cursor", "Use next_cursor from this server session.")
		return
	}
	type topicInfo struct {
		Topic     string    `json:"topic"`
		LastID    string    `json:"last_id"`
		UpdatedAt time.Time `json:"updated_at"`
		Count     int       `json:"count"`
		URL       string    `json:"url"`
	}
	byTopic := make(map[string]*topicInfo)
	for _, m := range b.messages {
		if m.removed {
			continue
		}
		t := byTopic[m.Topic]
		if t == nil {
			t = &topicInfo{Topic: m.Topic, URL: "/board/read?topic=" + m.Topic}
			byTopic[m.Topic] = t
		}
		t.Count++
		t.LastID = m.ID
		t.UpdatedAt = m.CreatedAt
	}
	items := []topicInfo{}
	for _, t := range byTopic {
		if q.Get("cursor") == "" || t.LastID < q.Get("cursor") {
			items = append(items, *t)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].LastID > items[j].LastID })
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].LastID
	}
	boardRespond(w, r, 200, map[string]any{"topics": items, "next_cursor": next, "session": b.boot})
}
