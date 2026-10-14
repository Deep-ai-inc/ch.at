package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"9fans.net/go/plan9"
	p9client "9fans.net/go/plan9/client"
)

func ninePTestConn(t *testing.T, llm llmFunc) net.Conn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	peer := chatRequest("GET", "/", "").RemoteAddr
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		listener.Close()
		if err == nil {
			serveNinePConn(conn, peer, llm)
		}
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
		listener.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("9p connection leaked")
		}
	})
	return conn
}
func ninePTestFS(t *testing.T, llm llmFunc) (*p9client.Fsys, *p9client.Conn) {
	t.Helper()
	conn, err := p9client.NewConn(ninePTestConn(t, llm))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fs, err := conn.Attach(nil, "anonymous", "")
	if err != nil {
		t.Fatal(err)
	}
	return fs, conn
}

func TestNinePRealClientPlainAnonymous(t *testing.T) {
	var calls atomic.Int32
	fs, _ := ninePTestFS(t, fixedChatLLM(&calls, "Hello ", "世界"))
	root, err := fs.Open("/", plan9.OREAD)
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := root.Dirreadall()
	root.Close()
	if err != nil || len(dirs) != 2 {
		t.Fatal(dirs, err)
	}
	writer, err := fs.Open("ctl", plan9.OWRITE|plan9.OTRUNC)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"What ", "is Go?", "\n"} {
		if _, err := writer.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	// Duplicate bytes at an old offset must not invoke the model again.
	if n, err := writer.WriteAt([]byte("What "), 0); err != nil || n != 5 {
		t.Fatal(n, err)
	}
	writer.Close() // echo closes its writer before cat opens its reader.
	reader, err := fs.Open("ctl", plan9.OREAD)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "Hello 世界" {
		t.Fatalf("%q %v", data, err)
	}
	buf := make([]byte, 5)
	if n, err := reader.ReadAt(buf, 0); err != nil || n != 5 || string(buf) != "Hello" {
		t.Fatal(n, err, string(buf))
	}
	reader.Close()
	if calls.Load() != 1 {
		t.Fatal("duplicate generation", calls.Load())
	}
	if _, err := fs.Open("../../etc/passwd", plan9.OREAD); err == nil {
		t.Fatal("host path exposed")
	}
	if err := fs.Remove("ctl"); err == nil {
		t.Fatal("remove accepted")
	}
}

func TestNinePConnectionsDoNotShareAnswers(t *testing.T) {
	var callsA, callsB atomic.Int32
	tests := []struct {
		answer string
		calls  *atomic.Int32
		fid    *p9client.Fid
	}{
		{answer: "private A", calls: &callsA}, {answer: "private B", calls: &callsB},
	}
	// Keep both sessions alive and submit both prompts before reading either.
	for i := range tests {
		tc := &tests[i]
		fs, _ := ninePTestFS(t, fixedChatLLM(tc.calls, tc.answer))
		var err error
		tc.fid, err = fs.Open("ctl", plan9.ORDWR)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tc.fid.Write([]byte("hello\n")); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range tests {
		buf := make([]byte, 100)
		n, err := tc.fid.ReadAt(buf, 0)
		if (err != nil && !errors.Is(err, io.EOF)) || string(buf[:n]) != tc.answer {
			t.Fatal(string(buf[:n]), err)
		}
		tc.fid.Close()
	}
	if callsA.Load() != 1 || callsB.Load() != 1 {
		t.Fatal("wrong calls")
	}
}

func p9Exchange(t *testing.T, conn net.Conn, call *plan9.Fcall) *plan9.Fcall {
	t.Helper()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := plan9.WriteFcall(conn, call); err != nil {
		t.Fatal(err)
	}
	r, err := plan9.ReadFcall(conn)
	if err != nil {
		t.Fatal(err)
	}
	if r.Tag != call.Tag {
		t.Fatal("wrong response tag", r)
	}
	return r
}
func p9OpenCtl(t *testing.T, conn net.Conn) {
	t.Helper()
	for _, call := range []*plan9.Fcall{
		{Type: plan9.Tversion, Tag: plan9.NOTAG, Msize: 8192, Version: "9P2000"},
		{Type: plan9.Tattach, Tag: 1, Fid: 1, Afid: plan9.NOFID},
		{Type: plan9.Twalk, Tag: 2, Fid: 1, Newfid: 2, Wname: []string{"ctl"}},
		{Type: plan9.Topen, Tag: 3, Fid: 2, Mode: plan9.ORDWR},
	} {
		if r := p9Exchange(t, conn, call); r.Type == plan9.Rerror {
			t.Fatal(r)
		}
	}
}

func TestNinePFlushAndDisconnect(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})
	conn := ninePTestConn(t, func(ctx context.Context, _ interface{}, ch chan<- string) (string, error) {
		defer close(finished)
		defer close(ch)
		select {
		case <-release:
			ch <- "done"
			return "", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	p9OpenCtl(t, conn)
	if r := p9Exchange(t, conn, &plan9.Fcall{Type: plan9.Twrite, Tag: 4, Fid: 2, Data: []byte("hello\n")}); r.Type != plan9.Rwrite {
		t.Fatal(r)
	}
	if err := plan9.WriteFcall(conn, &plan9.Fcall{Type: plan9.Tread, Tag: 5, Fid: 2, Count: 100}); err != nil {
		t.Fatal(err)
	}
	if r := p9Exchange(t, conn, &plan9.Fcall{Type: plan9.Tflush, Tag: 6, Oldtag: 5}); r.Type != plan9.Rflush {
		t.Fatal(r)
	}
	close(release)
	<-finished
	conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if r, err := plan9.ReadFcall(conn); err == nil {
		t.Fatal("response after flush", r)
	}
	if r := p9Exchange(t, conn, &plan9.Fcall{Type: plan9.Tread, Tag: 7, Fid: 2, Count: 100}); r.Type != plan9.Rread || string(r.Data) != "done" {
		t.Fatal(r)
	}

	canceled := make(chan struct{})
	other := ninePTestConn(t, func(ctx context.Context, _ interface{}, ch chan<- string) (string, error) {
		defer close(ch)
		defer close(canceled)
		<-ctx.Done()
		return "", ctx.Err()
	})
	p9OpenCtl(t, other)
	p9Exchange(t, other, &plan9.Fcall{Type: plan9.Twrite, Tag: 4, Fid: 2, Data: []byte("cancel me\n")})
	other.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel model")
	}
}

func TestNinePNewPromptInvalidatesOldReadOffsets(t *testing.T) {
	fs, _ := ninePTestFS(t, func(ctx context.Context, input interface{}, ch chan<- string) (string, error) {
		defer close(ch)
		ch <- input.(string)
		return "", nil
	})
	write := func(prompt string) {
		fid, err := fs.Open("ctl", plan9.OWRITE)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fid.Write([]byte(prompt + "\n")); err != nil {
			t.Fatal(err)
		}
		fid.Close()
	}
	write("first")
	reader, err := fs.Open("ctl", plan9.OREAD)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	buf := make([]byte, 2)
	if _, err := reader.ReadAt(buf, 0); err != nil || string(buf) != "fi" {
		t.Fatal(string(buf), err)
	}
	write("second")
	if _, err := reader.ReadAt(buf, 2); err == nil {
		t.Fatal("mixed response generations")
	}
	buf = make([]byte, 6)
	if _, err := reader.ReadAt(buf, 0); err != nil || string(buf) != "second" {
		t.Fatal(string(buf), err)
	}
}

func TestNinePBoundsAndMalformedFrames(t *testing.T) {
	var calls atomic.Int32
	conn := ninePTestConn(t, fixedChatLLM(&calls, "unused"))
	p9OpenCtl(t, conn)
	for i := 3; i < 65; i++ {
		r := p9Exchange(t, conn, &plan9.Fcall{Type: plan9.Twalk, Tag: uint16(i + 10), Fid: 1, Newfid: uint32(i)})
		if r.Type != plan9.Rwalk {
			t.Fatal(r)
		}
	}
	if r := p9Exchange(t, conn, &plan9.Fcall{Type: plan9.Twalk, Tag: 100, Fid: 1, Newfid: 65}); r.Type != plan9.Rerror {
		t.Fatal("fid limit missing", r)
	}
	if r := p9Exchange(t, conn, &plan9.Fcall{Type: plan9.Twrite, Tag: 101, Fid: 2, Data: []byte("one\ntwo\n")}); r.Type != plan9.Rerror {
		t.Fatal("multiple prompts accepted", r)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request billed")
	}
	bad := ninePTestConn(t, fixedChatLLM(&calls, "unused"))
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], 0xffffffff)
	bad.SetDeadline(time.Now().Add(time.Second))
	bad.Write(size[:])
	var buf [1]byte
	if _, err := bad.Read(buf[:]); err == nil {
		t.Fatal("oversized frame not closed")
	}
	if _, err := readNinePCall(strings.NewReader("\x03\x00\x00\x00"), 8192); err == nil {
		t.Fatal("short frame accepted")
	}
}

func TestNinePPendingReadsAreBounded(t *testing.T) {
	finished := make(chan struct{})
	conn := ninePTestConn(t, func(ctx context.Context, _ interface{}, ch chan<- string) (string, error) {
		defer close(ch)
		defer close(finished)
		<-ctx.Done()
		return "", ctx.Err()
	})
	p9OpenCtl(t, conn)
	p9Exchange(t, conn, &plan9.Fcall{Type: plan9.Twrite, Tag: 4, Fid: 2, Data: []byte("wait\n")})
	for tag := uint16(10); tag < 26; tag++ {
		if err := plan9.WriteFcall(conn, &plan9.Fcall{Type: plan9.Tread, Tag: tag, Fid: 2, Count: 100}); err != nil {
			t.Fatal(err)
		}
	}
	if r := p9Exchange(t, conn, &plan9.Fcall{Type: plan9.Tread, Tag: 100, Fid: 2, Count: 100}); r.Type != plan9.Rerror {
		t.Fatal("pending read limit missing", r)
	}
	conn.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("bounded session did not cancel its model")
	}
}

func TestProtocolReplyBoundsAndCancellation(t *testing.T) {
	var calls atomic.Int32
	answer, err := startProtocolAnswer(context.Background(), chatRequest("GET", "/", "").RemoteAddr, "hello", fixedChatLLM(&calls, strings.Repeat("é", protocolReplyLimit)))
	if err != nil {
		t.Fatal(err)
	}
	<-answer.done
	if len(answer.text) > protocolReplyLimit || calls.Load() != 1 {
		t.Fatal(len(answer.text), calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	answer, err = startProtocolAnswer(ctx, chatRequest("GET", "/", "").RemoteAddr, "hello", func(ctx context.Context, _ interface{}, ch chan<- string) (string, error) {
		defer close(ch)
		defer close(done)
		<-ctx.Done()
		return "", ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-answer.done
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("model leaked")
	}
	if _, err := startProtocolAnswer(ctx, "peer", strings.Repeat("a", boardMaxURL+1), nil); err == nil {
		t.Fatal("oversized prompt accepted")
	}
}
