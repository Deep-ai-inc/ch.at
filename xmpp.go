package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"mellium.im/sasl"
	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"
)

const xmppDomain = "ch.at"

var xmppBot = jid.MustParse("chat@" + xmppDomain)

type xmppMessage struct {
	stanza.Message
	Body  string        `xml:"body,omitempty"`
	Error *stanza.Error `xml:"error,omitempty"`
}
type xmppIQ struct {
	stanza.IQ
	Payload string `xml:",innerxml"` // Only server constants are emitted here.
}

func StartXMPPServer(port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	defer listener.Close()
	// Optional STARTTLS reuses the website certificate. Basic anonymous XMPP
	// remains available without a certificate, password or encryption bridge.
	var tlsConfig *tls.Config
	if cert, err := tls.LoadX509KeyPair("cert.pem", "key.pem"); err == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	}
	return serveProtocolConnections(listener, func(conn net.Conn) {
		serveXMPPConn(conn, conn.RemoteAddr().String(), tlsConfig, LLM)
	})
}

// Bound an XML negotiation/stanza, including a peer who never closes an XML
// element. A body length check after DecodeElement alone would be too late.
type xmppBoundedConn struct {
	net.Conn
	mu        sync.Mutex
	remaining int
	deadline  time.Time
}

func (c *xmppBoundedConn) reset(timeout time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remaining = 64 << 10
	c.deadline = time.Now().Add(timeout)
}
func (c *xmppBoundedConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	remaining, deadline := c.remaining, c.deadline
	c.mu.Unlock()
	if remaining <= 0 {
		return 0, errors.New("XMPP stanza exceeds size limit")
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	c.Conn.SetReadDeadline(deadline)
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.remaining -= n
	c.mu.Unlock()
	return n, err
}
func (c *xmppBoundedConn) Write(p []byte) (int, error) {
	c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.Conn.Write(p)
}

func anonymousSASL() xmpp.StreamFeature {
	feature := xmpp.SASLServer(func(*sasl.Negotiator) bool { return true }, sasl.Anonymous)
	// No credentials are exchanged with ANONYMOUS. Unlike password mechanisms,
	// it does not require TLS; this service intentionally supports native TCP.
	feature.Necessary = 0
	return feature
}
func optionalXMPPTLS(config *tls.Config) xmpp.StreamFeature {
	feature := xmpp.StartTLS(config)
	feature.Prohibited |= xmpp.Authn
	feature.List = func(ctx context.Context, w xmlstream.TokenWriter, start xml.StartElement) (bool, error) {
		if err := w.EncodeToken(start); err != nil {
			return false, err
		}
		return false, w.EncodeToken(start.End()) // Deliberately no <required/>.
	}
	return feature
}

func serveXMPPConn(raw net.Conn, peer string, tlsConfig *tls.Config, llm llmFunc) {
	defer raw.Close()
	if !rateLimitAllow(peer) {
		return
	}
	conn := &xmppBoundedConn{Conn: raw}
	conn.reset(10 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handshake, stopHandshake := context.WithTimeout(ctx, 10*time.Second)
	closeOnTimeout := context.AfterFunc(handshake, func() { raw.Close() })
	var seed [16]byte
	if _, err := rand.Read(seed[:]); err != nil {
		closeOnTimeout()
		stopHandshake()
		return
	}
	assigned := jid.MustParse(fmt.Sprintf("anonymous-%x@%s/session", seed, xmppDomain))
	features := []xmpp.StreamFeature{anonymousSASL(), xmpp.BindCustom(func(jid.JID, string) (jid.JID, error) { return assigned, nil })}
	if tlsConfig != nil {
		features = append([]xmpp.StreamFeature{optionalXMPPTLS(tlsConfig)}, features...)
	}
	session, err := xmpp.ReceiveClientSession(handshake, jid.MustParse(xmppDomain), conn, features...)
	closeOnTimeout()
	stopHandshake()
	if err != nil {
		return
	}
	conn.reset(2 * time.Minute)
	busy := make(chan struct{}, 1)
	var workers sync.WaitGroup
	defer func() { cancel(); raw.Close(); workers.Wait() }()
	_ = session.Serve(xmpp.HandlerFunc(func(w xmlstream.TokenReadEncoder, start *xml.StartElement) error {
		defer conn.reset(2 * time.Minute)
		// Include the already-read start token in the new decoder's stack.
		d := xml.NewTokenDecoder(xmlstream.MultiReader(xmlstream.Token(*start), w))
		if start.Name.Space != stanza.NSClient {
			var ignored struct{}
			return d.Decode(&ignored)
		}
		switch start.Name.Local {
		case "message":
			var message xmppMessage
			if err := d.Decode(&message); err != nil {
				return err
			}
			if message.Type != stanza.ChatMessage && message.Type != stanza.NormalMessage && message.Type != "" {
				return nil
			}
			if strings.TrimSpace(message.Body) == "" {
				return nil
			}
			reject := func(condition stanza.Condition, text string) error {
				return w.Encode(xmppMessage{Message: stanza.Message{ID: message.ID, Type: stanza.ErrorMessage, From: xmppBot, To: assigned}, Error: &stanza.Error{Type: stanza.Cancel, Condition: condition, Text: map[string]string{"": text}}})
			}
			if !validXMPPFrom(message.From, assigned) {
				return reject(stanza.Forbidden, "Use your assigned anonymous JID")
			}
			if !localXMPPTo(message.To) {
				return reject(stanza.ServiceUnavailable, "Only chat@ch.at is available; no message relay")
			}
			select {
			case busy <- struct{}{}:
			default:
				return reject(stanza.ResourceConstraint, "Previous prompt is still running")
			}
			answer, err := startProtocolAnswer(ctx, peer, message.Body, llm)
			if err != nil {
				<-busy
				return reject(stanza.ResourceConstraint, err.Error())
			}
			workers.Add(1)
			go func(id string) {
				defer workers.Done()
				defer func() { <-busy }()
				select {
				case <-answer.done:
				case <-ctx.Done():
					return
				}
				if err := session.Encode(ctx, xmppMessage{Message: stanza.Message{ID: id, Type: stanza.ChatMessage, From: xmppBot, To: assigned}, Body: string(answer.text)}); err != nil {
					cancel()
					raw.Close()
				}
			}(message.ID)
		case "presence":
			var presence stanza.Presence
			if err := d.Decode(&presence); err != nil {
				return err
			}
			if !validXMPPFrom(presence.From, assigned) || !localXMPPTo(presence.To) {
				return nil
			}
			if presence.Type == "" {
				return w.Encode(stanza.Presence{From: xmppBot, To: assigned})
			}
			if presence.Type == stanza.SubscribePresence {
				return w.Encode(stanza.Presence{From: xmppBot, To: assigned, Type: stanza.SubscribedPresence})
			}
		case "iq":
			var request struct {
				stanza.IQ
				Elements []struct{ XMLName xml.Name } `xml:",any"`
			}
			if err := d.Decode(&request); err != nil {
				return err
			}
			if request.Type != stanza.GetIQ && request.Type != stanza.SetIQ {
				return nil
			}
			reply := xmppIQ{IQ: stanza.IQ{ID: request.ID, From: jid.MustParse(xmppDomain), To: assigned, Type: stanza.ResultIQ}}
			var payload xml.Name
			if len(request.Elements) == 1 {
				payload = request.Elements[0].XMLName
			}
			legacySession := payload == (xml.Name{Space: "urn:ietf:params:xml:ns:xmpp-session", Local: "session"})
			if !validXMPPFrom(request.From, assigned) || !localXMPPTo(request.To) || len(request.Elements) != 1 || (request.Type == stanza.SetIQ && !legacySession) {
				reply.Type = stanza.ErrorIQ
				reply.Payload = xmppUnsupportedIQ
			} else {
				switch payload {
				case xml.Name{Space: "urn:xmpp:ping", Local: "ping"}:
				case xml.Name{Space: "urn:ietf:params:xml:ns:xmpp-session", Local: "session"}:
				case xml.Name{Space: "jabber:iq:roster", Local: "query"}:
					reply.Payload = `<query xmlns="jabber:iq:roster"><item jid="chat@ch.at" name="ch.at" subscription="both"/></query>`
				case xml.Name{Space: "http://jabber.org/protocol/disco#info", Local: "query"}:
					reply.Payload = `<query xmlns="http://jabber.org/protocol/disco#info"><identity category="automation" type="bot" name="ch.at"/><feature var="urn:xmpp:ping"/><feature var="http://jabber.org/protocol/disco#info"/><feature var="jabber:iq:roster"/></query>`
				default:
					reply.Type = stanza.ErrorIQ
					reply.Payload = xmppUnsupportedIQ
				}
			}
			return w.Encode(reply)
		default:
			var ignored struct{}
			return d.Decode(&ignored)
		}
		return nil
	}))
}

const xmppUnsupportedIQ = `<error type="cancel"><service-unavailable xmlns="urn:ietf:params:xml:ns:xmpp-stanzas"/></error>`

func validXMPPFrom(from, assigned jid.JID) bool {
	return from.String() == "" || from.String() == assigned.String() || from.String() == assigned.Bare().String()
}
func localXMPPTo(to jid.JID) bool {
	return to.String() == "" || to.Bare().String() == xmppDomain || to.Bare().String() == xmppBot.String()
}
