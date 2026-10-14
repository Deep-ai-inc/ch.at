package main

import (
	"context"
	"net"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

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
