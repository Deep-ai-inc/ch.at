package main

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mellium.im/sasl"
	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"
)

type xmppTestClient struct {
	session  *xmpp.Session
	messages chan xmppMessage
	conn     net.Conn
	done     <-chan struct{}
}

func newXMPPTestClient(t *testing.T, llm llmFunc, tlsConfig *tls.Config) *xmppTestClient {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan struct{})
	peer := chatRequest("GET", "/", "").RemoteAddr
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		listener.Close()
		if err == nil {
			serveXMPPConn(conn, peer, tlsConfig, llm)
		}
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	clientDone := make(chan struct{})
	t.Cleanup(func() {
		conn.Close()
		listener.Close()
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			t.Error("XMPP server leaked")
		}
	})
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	anon := xmpp.SASL("", "", sasl.Anonymous)
	anon.Necessary = 0
	session, err := xmpp.NewClientSession(ctx, jid.MustParse(xmppDomain), conn, anon, xmpp.BindResource())
	if err != nil {
		t.Fatal("anonymous plaintext negotiation", err)
	}
	if session.State()&xmpp.Secure != 0 {
		t.Fatal("test must demonstrate plaintext works")
	}
	conn.SetDeadline(time.Time{})
	client := &xmppTestClient{session: session, conn: conn, messages: make(chan xmppMessage, 16), done: clientDone}
	go func() {
		defer close(clientDone)
		_ = session.Serve(xmpp.HandlerFunc(func(r xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			if start.Name.Local != "message" {
				return nil
			}
			var m xmppMessage
			if err := xml.NewTokenDecoder(xmlstream.MultiReader(xmlstream.Token(*start), r)).Decode(&m); err != nil {
				return err
			}
			client.messages <- m
			return nil
		}))
	}()
	t.Cleanup(func() {
		conn.Close()
		select {
		case <-clientDone:
		case <-time.After(time.Second):
			t.Error("XMPP client leaked")
		}
	})
	return client
}
func (c *xmppTestClient) send(t *testing.T, message xmppMessage) xmppMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.session.Encode(ctx, message); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-c.messages:
		return reply
	case <-ctx.Done():
		t.Fatal("no XMPP reply")
		return xmppMessage{}
	}
}
func (c *xmppTestClient) iq(t *testing.T, payload string) xmppIQ {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := c.session.EncodeIQ(ctx, xmppIQ{IQ: stanza.IQ{ID: "query-1", Type: stanza.GetIQ, To: xmppBot}, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var encoded strings.Builder
	encoder := xml.NewEncoder(&encoded)
	if _, err := xmlstream.Copy(encoder, r); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Flush(); err != nil {
		t.Fatal(err)
	}
	var reply xmppIQ
	if err := xml.Unmarshal([]byte(encoded.String()), &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func TestXMPPRealClientPlainAnonymous(t *testing.T) {
	var calls atomic.Int32
	client := newXMPPTestClient(t, fixedChatLLM(&calls, "Hello ", "世界"), nil)
	assigned := client.session.LocalAddr()
	if !strings.HasPrefix(assigned.Localpart(), "anonymous-") || assigned.Domainpart() != xmppDomain || assigned.Resourcepart() == "" {
		t.Fatal("missing assigned JID", assigned)
	}
	reply := client.send(t, xmppMessage{Message: stanza.Message{ID: "chat-1", Type: stanza.ChatMessage, To: xmppBot}, Body: "Hello"})
	if reply.Body != "Hello 世界" || reply.ID != "chat-1" || reply.From.String() != xmppBot.String() || reply.To.String() != assigned.String() || calls.Load() != 1 {
		t.Fatal(reply, calls.Load())
	}
	if r := client.iq(t, `<ping xmlns="urn:xmpp:ping"/>`); r.Type != stanza.ResultIQ {
		t.Fatal(r)
	}
	if r := client.iq(t, `<query xmlns="http://jabber.org/protocol/disco#info"/>`); r.Type != stanza.ResultIQ || !strings.Contains(r.Payload, "automation") {
		t.Fatal(r)
	}
	if r := client.iq(t, `<query xmlns="jabber:iq:roster"/>`); r.Type != stanza.ResultIQ || !strings.Contains(r.Payload, "chat@ch.at") {
		t.Fatal(r)
	}
	if r := client.iq(t, `<unknown xmlns="urn:unsupported"/>`); r.Type != stanza.ErrorIQ {
		t.Fatal(r)
	}
}

func TestXMPPNoRelayOrClaimedIdentity(t *testing.T) {
	var calls atomic.Int32
	first := newXMPPTestClient(t, fixedChatLLM(&calls, "unused"), nil)
	second := newXMPPTestClient(t, fixedChatLLM(&calls, "unused"), nil)
	if first.session.LocalAddr().String() == second.session.LocalAddr().String() {
		t.Fatal("anonymous identities reused")
	}
	for _, message := range []xmppMessage{
		{Message: stanza.Message{ID: "foreign", Type: stanza.ChatMessage, To: jid.MustParse("someone@elsewhere.example")}, Body: "relay me"},
		{Message: stanza.Message{ID: "spoof", Type: stanza.ChatMessage, To: xmppBot, From: first.session.LocalAddr()}, Body: "not my identity"},
		{Message: stanza.Message{ID: "oversize", Type: stanza.ChatMessage, To: xmppBot}, Body: strings.Repeat("a", boardMaxURL+1)},
	} {
		reply := second.send(t, message)
		if reply.Type != stanza.ErrorMessage || reply.Error == nil || reply.To.String() != second.session.LocalAddr().String() {
			t.Fatal(reply)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("rejected work invoked model", calls.Load())
	}
	select {
	case msg := <-first.messages:
		t.Fatal("message leaked to another connection", msg)
	default:
	}
}

func TestXMPPBusyAndDisconnectCancellation(t *testing.T) {
	started, finished := make(chan struct{}), make(chan struct{})
	client := newXMPPTestClient(t, func(ctx context.Context, _ interface{}, ch chan<- string) (string, error) {
		defer close(ch)
		defer close(finished)
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}, nil)
	if err := client.session.Encode(context.Background(), xmppMessage{Message: stanza.Message{ID: "first", Type: stanza.ChatMessage, To: xmppBot}, Body: "wait"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("model not started")
	}
	reply := client.send(t, xmppMessage{Message: stanza.Message{ID: "second", Type: stanza.ChatMessage, To: xmppBot}, Body: "must not queue"})
	if reply.Type != stanza.ErrorMessage || reply.Error == nil || reply.Error.Condition != stanza.ResourceConstraint {
		t.Fatal(reply)
	}
	client.conn.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("model not canceled on disconnect")
	}
}

func TestXMPPPlainLoginWithOptionalTLSAvailable(t *testing.T) {
	certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer certServer.Close()
	config := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: certServer.TLS.Certificates}
	var calls atomic.Int32
	client := newXMPPTestClient(t, fixedChatLLM(&calls, "plain still works"), config)
	reply := client.send(t, xmppMessage{Message: stanza.Message{ID: "plain", Type: stanza.ChatMessage, To: xmppBot}, Body: "hello"})
	if reply.Body != "plain still works" || calls.Load() != 1 {
		t.Fatal(reply, calls.Load())
	}
}

func TestXMPPOversizedIncompleteStanzaIsClosed(t *testing.T) {
	var calls atomic.Int32
	client := newXMPPTestClient(t, fixedChatLLM(&calls, "unused"), nil)
	client.conn.Write([]byte(`<message to="chat@ch.at"><body>` + strings.Repeat("x", 70<<10)))
	select {
	case <-client.done:
	case <-time.After(time.Second):
		t.Fatal("oversized incomplete stanza kept connection open")
	}
	if calls.Load() != 0 {
		t.Fatal("malformed XML reached model")
	}
}

func TestXMPPAnonymousDoesNotRequireEncryption(t *testing.T) {
	feature := anonymousSASL()
	if feature.Necessary&xmpp.Secure != 0 {
		t.Fatal("anonymous login requires TLS")
	}
	var xmlText strings.Builder
	writer := xml.NewEncoder(&xmlText)
	feature = optionalXMPPTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	required, err := feature.List(context.Background(), writer, xml.StartElement{Name: feature.Name})
	writer.Flush()
	if err != nil || required || strings.Contains(xmlText.String(), "required") {
		t.Fatal(xmlText.String(), required, err)
	}
}
