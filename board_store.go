package main

// A local JSONL journal, not a database service. One process owns the file.
// Mutations become visible only after append+fsync. Periodic atomic compaction
// preserves current identities, live posts/nonces, removals and ID high-water.
import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

const boardCompactBytes int64 = 64 << 20
const boardMaxJournalBytes int64 = 256 << 20

type boardEvent struct {
	Type     string         `json:"type"`
	Version  int            `json:"version,omitempty"`
	Session  string         `json:"session,omitempty"`
	Sequence uint64         `json:"sequence,omitempty"`
	Message  *boardMessage  `json:"message,omitempty"`
	Nonce    string         `json:"nonce,omitempty"`
	Removed  bool           `json:"removed,omitempty"`
	ID       string         `json:"id,omitempty"`
	Identity *boardIdentity `json:"identity,omitempty"`
}

type boardJournal struct {
	file      *os.File
	lock      *os.File
	path      string
	size      int64
	compactAt int64
	failed    bool
	syncFile  func(*os.File) error
}

func initializeBoardStorage() error {
	path := os.Getenv("BOARD_LOG_PATH")
	if path == "" {
		path = "board.jsonl"
	}
	b, err := openBoardStore(path)
	if err != nil {
		return err
	}
	b.adminToken = publicBoard.adminToken
	b.blockedTopics = publicBoard.blockedTopics
	publicBoard = b
	return nil
}

func (b *agentBoard) closeStore() {
	if b.journal == nil {
		return
	}
	_ = b.journal.file.Close()
	_ = b.journal.lock.Close() // Closing releases the advisory process lock.
}

func openBoardStore(path string) (_ *agentBoard, retErr error) {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = lock.Close()
		}
	}()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("board store already in use: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = f.Close()
		}
	}()
	if err = f.Chmod(0600); err != nil {
		return nil, err
	}
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if stat.Size() > boardMaxJournalBytes {
		return nil, errors.New("board journal exceeds disk backstop; restore or compact offline")
	}
	b := newAgentBoard()
	b.journal = &boardJournal{file: f, lock: lock, path: path, compactAt: boardCompactBytes, syncFile: (*os.File).Sync}
	reader := bufio.NewReaderSize(f, 64<<10)
	var offset int64
	first := true
	for {
		line, readErr := reader.ReadSlice('\n')
		if readErr == io.EOF {
			if len(line) > 0 {
				// A crash can leave only the final uncommitted record incomplete.
				if first {
					return nil, errors.New("incomplete board journal header; refusing to reset identity history")
				}
				if err = f.Truncate(offset); err != nil {
					return nil, err
				}
				if err = f.Sync(); err != nil {
					return nil, err
				}
			}
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read journal at byte %d: %w", offset, readErr)
		}
		var event boardEvent
		if !utf8.Valid(line) {
			return nil, fmt.Errorf("invalid UTF-8 journal record at byte %d", offset)
		}
		if err = json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("invalid journal record at byte %d: %w", offset, err)
		}
		if first {
			if event.Type != "meta" || event.Version != 1 || len(event.Session) != 32 || !validBoardKey(event.Session+event.Session) {
				return nil, errors.New("invalid board journal header")
			}
			b.boot, b.seq = event.Session, event.Sequence
			first = false
		} else if err = b.replay(event); err != nil {
			return nil, fmt.Errorf("journal replay at byte %d: %w", offset, err)
		}
		offset += int64(len(line))
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	b.journal.size = offset
	if first {
		if err = b.appendEvent(boardEvent{Type: "meta", Version: 1, Session: b.boot}); err != nil {
			return nil, err
		}
		if err = syncBoardDirectory(path); err != nil {
			return nil, err
		}
	}
	sort.Slice(b.messages, func(i, j int) bool { return b.messages[i].ID < b.messages[j].ID })
	return b, nil
}

func (b *agentBoard) replay(e boardEvent) error {
	switch e.Type {
	case "message":
		if e.Message == nil {
			return errors.New("missing message")
		}
		m := *e.Message
		if !boardTopicPattern.MatchString(m.Topic) || len(m.Text) > 2048 || strings.TrimSpace(m.Text) == "" || len(e.Nonce) < 1 || len(e.Nonce) > 128 || m.CreatedAt.IsZero() || !m.ExpiresAt.After(m.CreatedAt) || m.URL != "/board/message?id="+m.ID {
			return errors.New("invalid stored message fields")
		}
		if m.VerifiedSameActor != (m.ActorID != "") {
			return errors.New("invalid stored verification flag")
		}
		if m.ActorID != "" {
			i, ok := b.identities[m.ActorID]
			if !ok || m.Name != i.Name {
				return errors.New("stored verified author does not match identity")
			}
		} else if m.Name != "" && (!strings.HasPrefix(m.Name, "unverified: ") || len(m.Name) > len("unverified: ")+80) {
			return errors.New("unlabeled anonymous author")
		}
		if len(m.ID) != len(b.boot)+21 || !strings.HasPrefix(m.ID, b.boot+"-") {
			return errors.New("invalid message ID")
		}
		n, err := strconv.ParseUint(m.ID[len(b.boot)+1:], 10, 64)
		if err != nil || n == 0 || m.ID != fmt.Sprintf("%s-%020d", b.boot, n) {
			return errors.New("invalid message sequence")
		}
		b.seq = max(b.seq, n)
		if !b.now().Before(m.ExpiresAt) {
			return nil
		}
		if len(b.messages) >= boardMaxMessages {
			return errors.New("retained message capacity exceeded on replay")
		}
		topicCount := 0
		for _, existing := range b.messages {
			if existing.ID == m.ID {
				return errors.New("duplicate message ID in journal")
			}
			if existing.Topic == m.Topic {
				topicCount++
				if existing.ActorID == m.ActorID && existing.nonce == e.Nonce {
					return errors.New("duplicate nonce reservation in journal")
				}
			}
		}
		if topicCount >= boardMaxTopicMessages {
			return errors.New("retained topic capacity exceeded on replay")
		}
		m.nonce, m.removed = e.Nonce, e.Removed
		b.messages = append(b.messages, m)
	case "remove":
		if !b.validID(e.ID) {
			return errors.New("removal refers to unknown message")
		}
		for i := range b.messages {
			if b.messages[i].ID == e.ID {
				b.messages[i].removed = true
				break
			}
		}
	case "identity":
		if e.Identity == nil || !validBoardKey(e.Identity.ID) || !boardTopicPattern.MatchString(e.Identity.Name) || !validBoardKey(e.Identity.KeyHash) {
			return errors.New("invalid identity")
		}
		i := *e.Identity
		if owner, ok := b.identityNames[i.Name]; ok && owner != i.ID {
			return errors.New("duplicate identity name")
		}
		if old, ok := b.identities[i.ID]; ok {
			if old.Name != i.Name {
				return errors.New("identity name changed")
			}
		} else if len(b.identities) >= boardMaxIdentities {
			return errors.New("identity capacity exceeded on replay")
		}
		b.identities[i.ID] = i
		b.identityNames[i.Name] = i.ID
	default:
		return errors.New("unknown journal event")
	}
	return nil
}

func (b *agentBoard) commit(w http.ResponseWriter, r *http.Request, e boardEvent) bool {
	if b.journal == nil {
		return true
	} // Isolated unit tests use an explicit RAM store.
	if err := b.appendEvent(e); err != nil {
		boardError(w, r, 503, "storage_unavailable", "Mutation not acknowledged. Preserve nonce/replacement key for retry; operator must inspect storage before restarting.")
		return false
	}
	return true
}

func (b *agentBoard) appendEvent(e boardEvent) error {
	j := b.journal
	if j.failed {
		return errors.New("journal is unavailable")
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if j.size+int64(len(line)) > j.compactAt {
		if err = b.compactStore(); err != nil {
			j.failed = true
			return err
		}
	}
	if j.size+int64(len(line)) > boardMaxJournalBytes {
		return errors.New("journal disk backstop reached")
	}
	n, err := j.file.Write(line)
	j.size += int64(n)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = j.syncFile(j.file)
	}
	if err != nil {
		j.failed = true
	}
	return err
}

func syncBoardDirectory(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (b *agentBoard) compactStore() error {
	j := b.journal
	var snapshot bytes.Buffer
	enc := json.NewEncoder(&snapshot)
	if err := enc.Encode(boardEvent{Type: "meta", Version: 1, Session: b.boot, Sequence: b.seq}); err != nil {
		return err
	}
	for _, i := range b.identities {
		if err := enc.Encode(boardEvent{Type: "identity", Identity: &i}); err != nil {
			return err
		}
	}
	for _, m := range b.messages {
		if b.now().Before(m.ExpiresAt) {
			if err := enc.Encode(boardEvent{Type: "message", Message: &m, Nonce: m.nonce, Removed: m.removed}); err != nil {
				return err
			}
		}
	}
	if int64(snapshot.Len()) > boardMaxJournalBytes {
		return errors.New("compacted journal exceeds disk backstop")
	}
	tmp, err := os.CreateTemp(filepath.Dir(j.path), filepath.Base(j.path)+".compact-*")
	if err != nil {
		return err
	}
	tempName := tmp.Name()
	defer func() { _ = tmp.Close(); _ = os.Remove(tempName) }()
	if _, err = io.Copy(tmp, &snapshot); err != nil {
		return err
	}
	if err = j.syncFile(tmp); err != nil {
		return err
	}
	stat, err := tmp.Stat()
	if err != nil {
		return err
	}
	if err = os.Rename(tempName, j.path); err != nil {
		return err
	}
	// Reopen before dropping the old descriptor; the lock is on a separate inode.
	next, err := os.OpenFile(j.path, os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_ = j.file.Close()
	j.file = next
	j.size = stat.Size()
	j.compactAt = min(boardMaxJournalBytes, max(boardCompactBytes, j.size*2))
	return syncBoardDirectory(j.path)
}
