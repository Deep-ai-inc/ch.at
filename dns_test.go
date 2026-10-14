//go:build boardtest

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/miekg/dns"
)

func TestDNSPrompt(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		delegated  bool
	}{
		{"why-is-gua-musang-the-king-of-durian.ask.ch.at.", "why is gua musang the king of durian", true},
		{"why.is.gua.musang.the.king.of.durian.ask.ch.at.", "why is gua musang the king of durian", true},
		{"WhAt-Is-2+2.AsK.Ch.At.", "what is 2+2", true},
		{"one--two.three.ask.ch.at", "one two three", true},
		{"ask.ch.at.", "", true},
		{"---.ask.ch.at.", "", true},
		{"what-is-2+2.", "what is 2+2", false},
		{"What-is-2+2.CH.AT.", "What is 2+2", false},
		{"what.is-2+2.ch.at.", "what.is 2+2", false},
		{"task.ch.at.", "task", false},
		{"ask.ch.at.evil.", "ask.ch.at.evil", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, delegated := dnsPrompt(tc.name)
			if got != tc.want || delegated != tc.delegated {
				t.Fatalf("got %q, %v", got, delegated)
			}
		})
	}
}

type chatDNSWriter struct {
	msg     *dns.Msg
	ip      string
	network string
}

func (w *chatDNSWriter) LocalAddr() net.Addr { return &net.UDPAddr{} }
func (w *chatDNSWriter) RemoteAddr() net.Addr {
	if w.network == "tcp" {
		return &net.TCPAddr{IP: net.ParseIP(w.ip), Port: 1234}
	}
	return &net.UDPAddr{IP: net.ParseIP(w.ip), Port: 1234}
}
func (w *chatDNSWriter) WriteMsg(m *dns.Msg) error   { w.msg = m; return nil }
func (w *chatDNSWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *chatDNSWriter) Close() error                { return nil }
func (w *chatDNSWriter) TsigStatus() error           { return nil }
func (w *chatDNSWriter) TsigTimersOnly(bool)         {}
func (w *chatDNSWriter) Hijack()                     {}

func TestChatDNSResponses(t *testing.T) {
	for i, tc := range []struct {
		name                     string
		qtype                    uint16
		code, answers, authority int
	}{
		{"ask.ch.at.", dns.TypeNS, dns.RcodeSuccess, 1, 0},
		{"ASK.CH.AT.", dns.TypeSOA, dns.RcodeSuccess, 1, 0},
		{"ask.ch.at.", dns.TypeTXT, dns.RcodeSuccess, 0, 1},
		{"word.ask.ch.at.", dns.TypeNS, dns.RcodeSuccess, 0, 1},
		{"word.ask.ch.at.", dns.TypeAAAA, dns.RcodeSuccess, 0, 1},
		{"word.ask.ch.at.", dns.TypeDS, dns.RcodeSuccess, 0, 1},
		{"---.ask.ch.at.", dns.TypeTXT, dns.RcodeSuccess, 0, 1},
		{`word\.word.ask.ch.at.`, dns.TypeTXT, dns.RcodeFormatError, 0, 0},
		{"WhAt.is-2+2.AsK.Ch.At.", dns.TypeTXT, dns.RcodeSuccess, 1, 0},
		{"what-is-2+2.", dns.TypeTXT, dns.RcodeSuccess, 1, 0},
	} {
		t.Run(tc.name+dns.TypeToString[tc.qtype], func(t *testing.T) {
			r := new(dns.Msg)
			r.SetQuestion(tc.name, tc.qtype)
			w := &chatDNSWriter{ip: fmt.Sprintf("192.0.2.%d", 50+i), network: "tcp"}
			called := false
			handleChatDNS(w, r, func(ctx context.Context, input interface{}, ch chan<- string) (string, error) {
				called = true
				defer close(ch)
				if input != "Answer in 500 characters or less, no markdown formatting: what is 2+2" {
					return "", fmt.Errorf("wrong prompt: %s", input)
				}
				ch <- "4"
				return "", nil
			})
			m := w.msg
			if m == nil || m.Rcode != tc.code || len(m.Answer) != tc.answers || len(m.Ns) != tc.authority || !m.Authoritative || m.RecursionAvailable {
				t.Fatalf("unexpected response: %v", m)
			}
			if called != (tc.qtype == dns.TypeTXT && tc.answers == 1) {
				t.Fatal("wrong LLM dispatch")
			}
			if _, err := m.Pack(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDelegatedUDPRequestsTCPBeforeModel(t *testing.T) {
	for _, edns := range []bool{false, true} {
		r := new(dns.Msg)
		r.SetQuestion("hello.ask.ch.at.", dns.TypeTXT)
		if edns {
			r.SetEdns0(1232, false)
		}
		w := &chatDNSWriter{ip: "192.0.2.100"}
		handleChatDNS(w, r, func(context.Context, interface{}, chan<- string) (string, error) {
			t.Error("UDP must not invoke the model")
			return "", nil
		})
		if !w.msg.Truncated || w.msg.Rcode != dns.RcodeSuccess || len(w.msg.Answer) != 0 {
			t.Fatal(w.msg)
		}
	}
}

func TestChatDNSInvalidRequests(t *testing.T) {
	for i, kind := range []string{"empty", "multiple", "class", "opcode", "provider", "rate"} {
		t.Run(kind, func(t *testing.T) {
			r := new(dns.Msg)
			r.SetQuestion("hello.ask.ch.at.", dns.TypeTXT)
			want := dns.RcodeFormatError
			w := &chatDNSWriter{ip: fmt.Sprintf("192.0.2.%d", 80+i), network: "tcp"}
			switch kind {
			case "empty":
				r.Question = nil
			case "multiple":
				r.Question = append(r.Question, r.Question[0])
			case "class":
				r.Question[0].Qclass = dns.ClassCHAOS
				want = dns.RcodeRefused
			case "opcode":
				r.Opcode = dns.OpcodeStatus
				want = dns.RcodeNotImplemented
			case "provider":
				want = dns.RcodeServerFailure
			case "rate":
				for j := 0; j < 10; j++ {
					rateLimitAllow(w.RemoteAddr().String())
				}
				want = dns.RcodeRefused
			}
			handleChatDNS(w, r, func(ctx context.Context, input interface{}, ch chan<- string) (string, error) {
				defer close(ch)
				if kind != "provider" {
					t.Error("unexpected LLM request")
				}
				return "", errors.New("private provider error")
			})
			if w.msg.Rcode != want || len(w.msg.Answer) != 0 {
				t.Fatal(w.msg)
			}
		})
	}
}

func TestDNSAnswerBoundsAndCancellation(t *testing.T) {
	for _, partial := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		finished := make(chan struct{})
		answer, err := generateDNSAnswer(ctx, "test", func(ctx context.Context, _ interface{}, ch chan<- string) (string, error) {
			defer close(finished)
			defer close(ch)
			if partial {
				ch <- "partial"
			}
			<-ctx.Done()
			return "", ctx.Err()
		})
		cancel()
		<-finished
		if partial && (err != nil || !strings.Contains(answer, "partial")) {
			t.Fatalf("%q %v", answer, err)
		}
		if !partial && err == nil {
			t.Fatal("timeout must fail")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	answer, err := generateDNSAnswer(ctx, "test", func(ctx context.Context, _ interface{}, ch chan<- string) (string, error) {
		defer close(finished)
		defer close(ch)
		ch <- strings.Repeat("é", 300)
		<-ctx.Done()
		return "", ctx.Err()
	})
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("LLM not canceled")
	}
	if err != nil || len(answer) > 500 || !utf8.ValidString(answer) || !strings.HasSuffix(answer, "...") {
		t.Fatalf("%q %v", answer, err)
	}
}

func TestChatDNSWireAndTCPFallback(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		tcp.Close()
		t.Fatal(err)
	}
	answer := strings.Repeat(`text\123"é`, 45)
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		handleChatDNS(w, r, func(ctx context.Context, _ interface{}, ch chan<- string) (string, error) {
			defer close(ch)
			ch <- answer
			return "", nil
		})
	})
	for _, s := range []*dns.Server{{Listener: tcp, Net: "tcp", Handler: handler}, {PacketConn: udp, Net: "udp", Handler: handler}} {
		ready := make(chan struct{})
		s.NotifyStartedFunc = func() { close(ready) }
		go func() { _ = s.ActivateAndServe() }()
		<-ready
		t.Cleanup(func() { _ = s.Shutdown() })
	}
	r := new(dns.Msg)
	r.SetQuestion(strings.Repeat("a", 63)+"."+strings.Repeat("b", 63)+".ch.at.", dns.TypeTXT)
	// A standard resolver must transparently retry delegated chat over TCP.
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, tcp.Addr().String())
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	txt, err := resolver.LookupTXT(ctx, "hello.ask.ch.at")
	if err != nil || len(txt) != 1 || txt[0] != answer {
		t.Fatalf("recursive TCP fallback: %q %v", txt, err)
	}
	for _, network := range []string{"udp", "tcp", "edns"} {
		client := &dns.Client{Net: network, Timeout: time.Second}
		if network == "edns" {
			client.Net = "udp"
			r.SetEdns0(4096, false)
		}
		m, _, err := client.Exchange(r, tcp.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if network == "udp" {
			if !m.Truncated || m.Len() > 512 {
				t.Fatal("missing safe TCP fallback", m)
			}
		} else {
			if m.Truncated || string(dnsBoardBody(t, m)) != answer {
				t.Fatal("TXT wire corruption", m)
			}
			if network == "edns" && (m.IsEdns0() == nil || m.IsEdns0().UDPSize() != 1232) {
				t.Fatal("EDNS size not capped")
			}
		}
	}
}
