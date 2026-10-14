package main

import (
	"context"
	"io"
	"net"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeConnectionCapMatchesSSH(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	release := make(chan struct{})
	started := make(chan struct{}, 101)
	finished := make(chan struct{}, 101)
	accepted := 0
	defer func() {
		close(release)
		for i := 0; i < accepted; i++ {
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Error("connection handler leaked")
				return
			}
		}
	}()
	var active atomic.Int32
	go serveProtocolConnections(listener, func(net.Conn) {
		active.Add(1)
		defer func() { active.Add(-1); finished <- struct{}{} }()
		started <- struct{}{}
		<-release
	})
	var clients []net.Conn
	defer func() {
		for _, conn := range clients {
			conn.Close()
		}
	}()
	for i := 0; i < 100; i++ {
		conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, conn)
		select {
		case <-started:
			accepted++
		case <-time.After(time.Second):
			t.Fatal("connection not accepted")
		}
	}
	extra, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(time.Second))
	var byteBuffer [1]byte
	if _, err := extra.Read(byteBuffer[:]); err != io.EOF {
		t.Fatal("101st connection was not rejected", err)
	}
	if active.Load() != 100 {
		t.Fatal("wrong active connection cap", active.Load())
	}
}

// Exercise the documented user commands, not just the server's internal API.
// The only model is a local stub and the only network connection is loopback.
func TestNativeProtocolCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the two native command-line clients")
	}
	for _, protocol := range []string{"chat9p", "chatxmpp"} {
		t.Run(protocol, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			var calls atomic.Int32
			llm := fixedChatLLM(&calls, "native anonymous reply")
			done := make(chan struct{})
			peer := chatRequest("GET", "/", "").RemoteAddr
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				if protocol == "chat9p" {
					serveNinePConn(conn, peer, llm)
				} else {
					serveXMPPConn(conn, peer, nil, llm)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, "go", "run", "./cmd/"+protocol, "-addr", listener.Addr().String(), "hello").CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", protocol, err, output)
			}
			if string(output) != "native anonymous reply" || calls.Load() != 1 {
				t.Fatalf("%q calls=%d", output, calls.Load())
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("client did not disconnect")
			}
		})
	}
}
