// chatxmpp connects directly using SASL ANONYMOUS. STARTTLS is an optional
// client choice, not a requirement for the native anonymous service.
package main

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"mellium.im/sasl"
	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"
)

type message struct {
	stanza.Message
	Body  string        `xml:"body,omitempty"`
	Error *stanza.Error `xml:"error,omitempty"`
}

func main() {
	addr := flag.String("addr", "ch.at:5222", "XMPP client-to-server TCP address")
	domain := flag.String("domain", "ch.at", "XMPP server domain")
	startTLS := flag.Bool("starttls", false, "request optional STARTTLS with certificate verification")
	flag.Parse()
	if err := run(*addr, *domain, *startTLS, strings.Join(flag.Args(), " "), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(addr, domain string, startTLS bool, prompt string, out io.Writer) error {
	if strings.TrimSpace(prompt) == "" || len(prompt) > 8192 {
		return fmt.Errorf("provide a nonempty question of at most 8192 bytes")
	}
	server, err := jid.Parse(domain)
	if err != nil || server.Localpart() != "" || server.Resourcepart() != "" {
		return fmt.Errorf("invalid server domain")
	}
	bot, err := jid.Parse("chat@" + domain)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(70 * time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	anon := xmpp.SASL("", "", sasl.Anonymous)
	anon.Necessary = 0
	features := []xmpp.StreamFeature{anon, xmpp.BindResource()}
	if startTLS {
		features = append([]xmpp.StreamFeature{xmpp.StartTLS(&tls.Config{ServerName: domain, MinVersion: tls.VersionTLS12})}, features...)
	}
	session, err := xmpp.NewClientSession(ctx, server, conn, features...)
	if err != nil {
		return err
	}
	if startTLS && session.State()&xmpp.Secure == 0 {
		return fmt.Errorf("server did not negotiate requested STARTTLS")
	}
	answer := make(chan message, 1)
	failed := make(chan error, 1)
	go func() {
		failed <- session.Serve(xmpp.HandlerFunc(func(r xmlstream.TokenReadEncoder, start *xml.StartElement) error {
			if start.Name.Local != "message" {
				return nil
			}
			var m message
			if err := xml.NewTokenDecoder(xmlstream.MultiReader(xmlstream.Token(*start), r)).Decode(&m); err != nil {
				return err
			}
			if m.ID == "question" && m.From.Bare().String() == bot.String() {
				select {
				case answer <- m:
				default:
				}
			}
			return nil
		}))
	}()
	if err := session.Encode(ctx, message{Message: stanza.Message{ID: "question", Type: stanza.ChatMessage, To: bot}, Body: prompt}); err != nil {
		return err
	}
	select {
	case reply := <-answer:
		if reply.Type == stanza.ErrorMessage {
			return fmt.Errorf("XMPP request rejected: %v", reply.Error)
		}
		_, err := io.WriteString(out, reply.Body)
		return err
	case err := <-failed:
		if err == nil {
			return io.ErrUnexpectedEOF
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
