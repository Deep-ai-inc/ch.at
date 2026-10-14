package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"9fans.net/go/plan9"
)

const ninePMaxMessage = 64 << 10
const ninePReadme = "ch.at anonymous 9p2000\nWrite a UTF-8 prompt followed by newline to ctl, then read ctl.\nOne request at a time; reopen ctl for the next prompt.\nThis connection alone owns its answer. No host files are exposed.\nPlain TCP is unencrypted; use HTTPS/SSH for secrets.\n"

func StartNinePServer(port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	defer listener.Close()
	return serveProtocolConnections(listener, func(conn net.Conn) {
		serveNinePConn(conn, conn.RemoteAddr().String(), LLM)
	})
}

// Same connection-cap pattern as SSH; no account or per-protocol quota system.
func serveProtocolConnections(listener net.Listener, handler func(net.Conn)) error {
	sem := make(chan struct{}, 100)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		select {
		case sem <- struct{}{}:
			go func() { defer func() { <-sem; conn.Close() }(); handler(conn) }()
		default:
			conn.Close()
		}
	}
}

type ninePFid struct {
	path          uint64 // 0 root, 1 ctl, 2 README
	open          bool
	mode          uint8
	input         []byte
	submitted     bool
	answerVersion uint32
}
type ninePRead struct {
	fid    uint32
	cancel context.CancelFunc
}
type ninePSession struct {
	conn      net.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	peer      string
	llm       llmFunc
	msize     uint32
	versioned bool
	fids      map[uint32]*ninePFid // Only the request loop accesses fids/current.
	current   *protocolAnswer
	version   uint32
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[uint16]*ninePRead
	reads     sync.WaitGroup
}

func serveNinePConn(conn net.Conn, peer string, llm llmFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &ninePSession{conn: conn, ctx: ctx, cancel: cancel, peer: peer, llm: llm, msize: ninePMaxMessage, fids: make(map[uint32]*ninePFid), pending: make(map[uint16]*ninePRead)}
	defer func() { cancel(); conn.Close(); s.reads.Wait() }()
	if !rateLimitAllow(peer) {
		return
	}
	for {
		idle := 2 * time.Minute
		if !s.versioned {
			idle = 10 * time.Second
		}
		conn.SetReadDeadline(time.Now().Add(idle))
		request, err := readNinePCall(conn, s.msize)
		if err != nil {
			return
		}
		s.pendingMu.Lock()
		_, duplicate := s.pending[request.Tag]
		s.pendingMu.Unlock()
		if duplicate {
			return
		} // Reusing an outstanding tag makes replies ambiguous.
		if request.Type == plan9.Tflush {
			s.pendingMu.Lock()
			if p := s.pending[request.Oldtag]; p != nil {
				delete(s.pending, request.Oldtag)
				p.cancel()
			}
			s.pendingMu.Unlock()
			if !s.send(&plan9.Fcall{Type: plan9.Rflush, Tag: request.Tag}) {
				return
			}
			continue
		}
		response, err := s.handle(request)
		if err != nil {
			response = &plan9.Fcall{Type: plan9.Rerror, Tag: request.Tag, Ename: err.Error()}
		}
		if response != nil && !s.send(response) {
			return
		}
	}
}

func readNinePCall(r io.Reader, max uint32) (*plan9.Fcall, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(size[:])
	if n < 7 || n > max || n > ninePMaxMessage {
		return nil, errors.New("invalid 9p frame size")
	}
	data := make([]byte, n)
	copy(data, size[:])
	if _, err := io.ReadFull(r, data[4:]); err != nil {
		return nil, err
	}
	return plan9.UnmarshalFcall(data)
}

func (s *ninePSession) send(call *plan9.Fcall) bool {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := plan9.WriteFcall(s.conn, call); err != nil {
		s.cancel()
		s.conn.Close()
		return false
	}
	return true
}
func ninePQid(path uint64, version uint32) plan9.Qid {
	q := plan9.Qid{Path: path}
	if path == 0 {
		q.Type = plan9.QTDIR
	}
	if path == 1 {
		q.Vers = version
	}
	return q
}
func (s *ninePSession) stat(path uint64) []byte {
	d := plan9.Dir{Qid: ninePQid(path, s.version), Uid: "anonymous", Gid: "anonymous", Muid: "anonymous", Mode: 0444}
	switch path {
	case 0:
		d.Name = "/"
		d.Mode = plan9.DMDIR | 0555
	case 1:
		d.Name = "ctl"
		d.Mode = 0666
		if s.current != nil {
			select {
			case <-s.current.done:
				d.Length = uint64(len(s.current.text))
			default:
			}
		}
	case 2:
		d.Name = "README"
		d.Length = uint64(len(ninePReadme))
	}
	data, _ := d.Bytes()
	return data
}
func ninePSlice(data []byte, offset uint64, count uint32) []byte {
	if offset >= uint64(len(data)) {
		return nil
	}
	end := min(uint64(len(data)), offset+uint64(count))
	return data[offset:end]
}

func (s *ninePSession) handle(t *plan9.Fcall) (*plan9.Fcall, error) {
	r := &plan9.Fcall{Type: t.Type + 1, Tag: t.Tag}
	if t.Type == plan9.Tversion {
		if s.versioned {
			return nil, errors.New("open a new connection to renegotiate")
		}
		if t.Version != "9P2000" {
			r.Version = "unknown"
			r.Msize = min(t.Msize, uint32(ninePMaxMessage))
			return r, nil
		}
		if t.Msize < 256 {
			return nil, errors.New("msize must be at least 256")
		}
		s.msize = min(t.Msize, uint32(ninePMaxMessage))
		s.versioned = true
		r.Version = "9P2000"
		r.Msize = s.msize
		return r, nil
	}
	if !s.versioned {
		return nil, errors.New("negotiate 9P2000 first")
	}
	if t.Type == plan9.Tauth {
		return nil, errors.New("authentication is not required; attach anonymously")
	}
	if t.Type == plan9.Tattach {
		if t.Afid != plan9.NOFID || t.Aname != "" {
			return nil, errors.New("use anonymous attach with empty aname")
		}
		if _, ok := s.fids[t.Fid]; ok || len(s.fids) >= 64 {
			return nil, errors.New("fid unavailable")
		}
		s.fids[t.Fid] = &ninePFid{}
		r.Qid = ninePQid(0, 0)
		return r, nil
	}
	f := s.fids[t.Fid]
	if f == nil {
		return nil, errors.New("unknown fid")
	}
	switch t.Type {
	case plan9.Twalk:
		if f.open {
			return nil, errors.New("cannot walk an open fid")
		}
		if t.Newfid != t.Fid {
			if _, ok := s.fids[t.Newfid]; ok || len(s.fids) >= 64 {
				return nil, errors.New("fid unavailable")
			}
		}
		path := f.path
		for _, name := range t.Wname {
			if path != 0 {
				break
			}
			switch name {
			case ".", "..":
				path = 0
			case "ctl":
				path = 1
			case "README":
				path = 2
			default:
				goto walked
			}
			r.Wqid = append(r.Wqid, ninePQid(path, s.version))
		}
	walked:
		if len(t.Wname) > 0 && len(r.Wqid) == 0 {
			return nil, errors.New("file not found")
		}
		// Partial walks report the prefix but do not install a new fid.
		if len(r.Wqid) == len(t.Wname) {
			s.fids[t.Newfid] = &ninePFid{path: path}
		}
	case plan9.Topen:
		if f.open {
			return nil, errors.New("fid already open")
		}
		if t.Mode & ^uint8(3|plan9.OTRUNC) != 0 || t.Mode&3 == plan9.OEXEC {
			return nil, errors.New("unsupported open mode")
		}
		if f.path != 1 && t.Mode != plan9.OREAD {
			return nil, errors.New("read-only file")
		}
		if t.Mode&plan9.OTRUNC != 0 && t.Mode&3 == plan9.OREAD {
			return nil, errors.New("truncate requires write access")
		}
		f.open = true
		f.mode = t.Mode & 3
		r.Qid = ninePQid(f.path, s.version)
		r.Iounit = s.msize - plan9.IOHDRSIZE
	case plan9.Tread:
		if !f.open || f.mode == plan9.OWRITE {
			return nil, errors.New("fid not open for reading")
		}
		count := min(t.Count, s.msize-plan9.IOHDRSIZE)
		if count == 0 {
			return r, nil
		}
		if f.path == 0 {
			a, b := s.stat(1), s.stat(2)
			if t.Offset != 0 && t.Offset != uint64(len(a)) && t.Offset != uint64(len(a)+len(b)) {
				return nil, errors.New("invalid directory offset")
			}
			data := append(a, b...)
			if t.Offset >= uint64(len(data)) {
				return r, nil
			}
			for offset := t.Offset; offset < uint64(len(data)); {
				n := uint64(binary.LittleEndian.Uint16(data[offset:])) + 2
				if uint64(len(r.Data))+n > uint64(count) {
					break
				}
				r.Data = append(r.Data, data[offset:offset+n]...)
				offset += n
			}
			if len(r.Data) == 0 {
				return nil, errors.New("directory read buffer too small")
			}
		} else if f.path == 2 || s.current == nil {
			r.Data = ninePSlice([]byte(ninePReadme), t.Offset, count)
		} else {
			if t.Offset == 0 || f.answerVersion == 0 {
				f.answerVersion = s.version
			} else if f.answerVersion != s.version {
				return nil, errors.New("answer changed; restart reading at offset zero")
			}
			s.pendingMu.Lock()
			if len(s.pending) >= 16 {
				s.pendingMu.Unlock()
				return nil, errors.New("too many pending reads")
			}
			ctx, cancel := context.WithCancel(s.ctx)
			p := &ninePRead{fid: t.Fid, cancel: cancel}
			s.pending[t.Tag] = p
			s.pendingMu.Unlock()
			answer := s.current
			s.reads.Add(1)
			go func() {
				defer s.reads.Done()
				defer cancel()
				select {
				case <-answer.done:
				case <-ctx.Done():
					return
				}
				s.pendingMu.Lock()
				defer s.pendingMu.Unlock()
				if s.pending[t.Tag] != p {
					return
				}
				delete(s.pending, t.Tag)
				r.Data = ninePSlice(answer.text, t.Offset, count)
				s.send(r)
			}()
			return nil, nil
		}
	case plan9.Twrite:
		if !f.open || f.path != 1 || f.mode == plan9.OREAD {
			return nil, errors.New("fid not open for writing")
		}
		if t.Offset < uint64(len(f.input)) {
			end := t.Offset + uint64(len(t.Data))
			if end <= uint64(len(f.input)) && bytes.Equal(f.input[t.Offset:end], t.Data) {
				r.Count = uint32(len(t.Data))
				return r, nil
			}
			return nil, errors.New("conflicting write offset")
		}
		if f.submitted || t.Offset != uint64(len(f.input)) || len(f.input)+len(t.Data) > boardMaxURL+2 {
			return nil, errors.New("reopen ctl; prompt limit is 8192 bytes")
		}
		input := append(append([]byte(nil), f.input...), t.Data...)
		if newline := bytes.IndexByte(input, '\n'); newline >= 0 {
			if newline != len(input)-1 {
				return nil, errors.New("one newline-terminated prompt per open")
			}
			if s.current != nil {
				select {
				case <-s.current.done:
				default:
					return nil, errors.New("previous prompt is still running")
				}
			}
			answer, err := startProtocolAnswer(s.ctx, s.peer, strings.TrimSuffix(string(input[:newline]), "\r"), s.llm)
			if err != nil {
				return nil, err
			}
			s.current = answer
			s.version++
			f.submitted = true
		}
		f.input = input
		r.Count = uint32(len(t.Data))
	case plan9.Tclunk:
		delete(s.fids, t.Fid)
		s.pendingMu.Lock()
		for tag, p := range s.pending {
			if p.fid == t.Fid {
				delete(s.pending, tag)
				p.cancel()
			}
		}
		s.pendingMu.Unlock()
	case plan9.Tstat:
		r.Stat = s.stat(f.path)
	default:
		return nil, errors.New("operation not supported")
	}
	return r, nil
}
