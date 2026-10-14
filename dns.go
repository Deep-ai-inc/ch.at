package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/miekg/dns"
)

const askDNSZone = "ask.ch.at."

func StartDNSServer(port int) error {
	dns.HandleFunc("ch.at.", handleDNS)
	dns.HandleFunc(".", handleDNS)

	addr := fmt.Sprintf(":%d", port)
	packet, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	defer packet.Close()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	udp := &dns.Server{PacketConn: packet, Net: "udp", UDPSize: 16 << 10}
	tcp := &dns.Server{Listener: listener, Net: "tcp"}
	defer udp.Shutdown()
	defer tcp.Shutdown()
	errors := make(chan error, 2)
	go func() { errors <- udp.ActivateAndServe() }()
	go func() { errors <- tcp.ActivateAndServe() }()
	return <-errors
}

// DNS names are case insensitive: resolvers may randomize the query's case.
// Strip the longest suffix first, with a label boundary, never a substring.
// Direct queries retain their historic punctuation; delegated queries use dots
// as word separators as well as hyphens.
func dnsPrompt(name string) (prompt string, delegated bool) {
	name = strings.TrimSuffix(name, ".")
	lower := strings.ToLower(name)
	zone := strings.TrimSuffix(askDNSZone, ".")
	switch {
	case lower == zone:
		return "", true
	case strings.HasSuffix(lower, "."+zone):
		name = strings.TrimSuffix(lower, "."+zone)
		name = strings.ReplaceAll(name, ".", " ")
		delegated = true
	case strings.HasSuffix(lower, ".ch.at"):
		name = name[:len(name)-len(".ch.at")]
	}
	return strings.Join(strings.Fields(strings.ReplaceAll(name, "-", " ")), " "), delegated
}

func askSOA() dns.RR {
	return &dns.SOA{
		Hdr: dns.RR_Header{Name: askDNSZone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60},
		Ns:  "ch.at.", Mbox: "hostmaster.ch.at.", Serial: 1,
		Refresh: 3600, Retry: 600, Expire: 86400, Minttl: 60,
	}
}

func writeChatDNS(w dns.ResponseWriter, r, m *dns.Msg) {
	if !strings.HasPrefix(w.RemoteAddr().Network(), "tcp") {
		size := dns.MinMsgSize
		if opt := r.IsEdns0(); opt != nil {
			size = min(1232, max(dns.MinMsgSize, int(opt.UDPSize())))
			m.SetEdns0(uint16(size), false)
		}
		m.Truncate(size)
	}
	_ = w.WriteMsg(m)
}

func handleDNS(w dns.ResponseWriter, r *dns.Msg) {
	if handleBoardDNS(w, r, publicBoard) {
		return
	}
	handleChatDNS(w, r, LLM)
}

type dnsLLM func(context.Context, interface{}, chan<- string) (string, error)

func handleChatDNS(w dns.ResponseWriter, r *dns.Msg, llm dnsLLM) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	defer writeChatDNS(w, r, m)
	if r.Opcode != dns.OpcodeQuery {
		m.Rcode = dns.RcodeNotImplemented
		return
	}
	if len(r.Question) != 1 {
		m.Rcode = dns.RcodeFormatError
		return
	}
	q := r.Question[0]
	if q.Qclass != dns.ClassINET {
		m.Rcode = dns.RcodeRefused
		return
	}
	prompt, delegated := dnsPrompt(q.Name)
	if delegated {
		// Escaped wire-label punctuation isn't a word separator. Reject rather
		// than silently treating a literal dot or backslash as a new label.
		if strings.Contains(q.Name, `\`) {
			m.Rcode = dns.RcodeFormatError
			return
		}
		if strings.EqualFold(q.Name, askDNSZone) {
			switch q.Qtype {
			case dns.TypeNS:
				m.Answer = []dns.RR{&dns.NS{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 300}, Ns: "ch.at."}}
				return
			case dns.TypeSOA:
				m.Answer = []dns.RR{askSOA()}
				return
			}
		}
		// Empty nonterminals and the apex exist but have no generated answer.
		// SOA in NODATA replies supports negative caching and QNAME minimization.
		if q.Qtype != dns.TypeTXT || prompt == "" {
			m.Ns = []dns.RR{askSOA()}
			return
		}
	}
	if q.Qtype != dns.TypeTXT {
		return
	}
	if !rateLimitAllow(w.RemoteAddr().String()) {
		m.Rcode = dns.RcodeRefused
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	answer, err := generateDNSAnswer(ctx, prompt, llm)
	if err != nil {
		// Don't cache provider failures or expose internal errors as TXT answers.
		m.Rcode = dns.RcodeServerFailure
		return
	}
	m.Answer = []dns.RR{&dns.TXT{
		Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
		Txt: dnsTXTChunks(answer),
	}}
}

func generateDNSAnswer(ctx context.Context, prompt string, llm dnsLLM) (string, error) {
	ch := make(chan string)
	result := make(chan error, 1)
	go func() {
		_, err := llm(ctx, "Answer in 500 characters or less, no markdown formatting: "+prompt, ch)
		result <- err
	}()
	var response strings.Builder
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				var err error
				select {
				case err = <-result:
				case <-ctx.Done():
					err = ctx.Err()
				}
				if err != nil {
					if ctx.Err() != nil && response.Len() > 0 {
						return dnsShorten(response.String() + "... (incomplete)"), nil
					}
					return "", err
				}
				if response.Len() == 0 {
					return "", fmt.Errorf("empty LLM response")
				}
				return response.String(), nil
			}
			response.WriteString(chunk)
			if response.Len() >= 500 {
				return dnsShorten(response.String()), nil
			}
		case <-ctx.Done():
			if response.Len() == 0 {
				return "", ctx.Err()
			}
			return dnsShorten(response.String() + "... (incomplete)"), nil
		}
	}
}

func dnsShorten(s string) string {
	if len(s) < 500 {
		return s
	}
	s = s[:497]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s + "..."
}

func dnsTXTChunks(s string) []string {
	var chunks []string
	for len(s) > 0 {
		n := min(255, len(s))
		// miekg/dns interprets backslashes as escapes when packing TXT data.
		chunks = append(chunks, strings.ReplaceAll(s[:n], `\`, `\\`))
		s = s[n:]
	}
	return chunks
}
