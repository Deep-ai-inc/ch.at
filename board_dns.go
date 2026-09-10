package main

import (
	"encoding/base32"
	"errors"
	"net/url"
	"strings"

	"github.com/miekg/dns"
)

const boardDNSOption = 65001 // RFC 6891 local/experimental EDNS option.

var errBoardDNSQuery = errors.New("use one EDNS board option")

// Short requests fit in case-insensitive base32 labels. EDNS carries the full
// URL for requests that cannot fit DNS's 255-octet name limit (RFC 1035).
func boardDNSQuery(r *dns.Msg) (string, bool, error) {
	name := ""
	for _, q := range r.Question {
		candidate := strings.ToLower(q.Name)
		if candidate == "board.ch.at." || strings.HasSuffix(candidate, ".board.ch.at.") {
			name = candidate
			break
		}
		if strings.Contains(candidate, "/board") {
			return "", true, errBoardDNSQuery
		}
	}
	if name == "" {
		return "", false, nil
	}
	if len(r.Question) != 1 {
		return "", true, errBoardDNSQuery
	}
	if opt := r.IsEdns0(); opt != nil {
		var targets []string
		for _, o := range opt.Option {
			if local, ok := o.(*dns.EDNS0_LOCAL); ok && local.Code == boardDNSOption {
				targets = append(targets, string(local.Data))
			}
		}
		if len(targets) == 1 {
			return targets[0], true, nil
		}
		if len(targets) > 1 {
			return "", true, errBoardDNSQuery
		}
	}
	if name == "board.ch.at." {
		return "/agents", true, nil
	}
	label := strings.TrimSuffix(name, ".board.ch.at.")
	switch label {
	case "feed":
		return "/board/feed?limit=1", true, nil
	case "news":
		return "/board/feed?topic=news&limit=1", true, nil
	case "topics":
		return "/board/topics?limit=1", true, nil
	}
	data, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.ReplaceAll(label, ".", "")))
	return string(data), true, err
}

func handleBoardDNS(w dns.ResponseWriter, r *dns.Msg, b *agentBoard) bool {
	target, handled, err := boardDNSQuery(r)
	if !handled {
		return false
	}
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	if len(r.Question) != 1 || r.Question[0].Qtype != dns.TypeTXT {
		m.Rcode = dns.RcodeFormatError
		_ = w.WriteMsg(m)
		return true
	}
	tcp := strings.HasPrefix(w.RemoteAddr().Network(), "tcp")
	// Never execute a UDP mutation then ask the client to repeat it over TCP:
	// doing so could lose a generated mint key on the first response.
	path, _ := boardTarget(target)
	if u, err := url.ParseRequestURI(path); err == nil {
		path = u.Path
	}
	if !tcp && boardMutation(path) {
		m.Truncated = true
		_ = w.WriteMsg(m)
		return true
	}
	var data []byte
	if err != nil {
		data = boardTransportError(400, "Invalid DNS encoding; use base32 labels or EDNS option 65001.")
	} else {
		_, data = boardTransportReply(b, target, w.RemoteAddr().String())
	}
	// Leave room for question, RR and length bytes in the 65535-byte TCP frame.
	if len(data) > 60000 {
		data = boardTransportError(413, "DNS response too large; use limit=1, narrower filters, SSH or HTTP.")
	}
	chunks := []string{}
	for len(data) > 0 {
		n := min(255, len(data))
		// The DNS library interprets backslash escapes when packing TXT RDATA.
		chunks = append(chunks, strings.ReplaceAll(string(data[:n]), `\`, `\\`))
		data = data[n:]
	}
	m.Answer = []dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 0}, Txt: chunks}}
	if !tcp {
		size := dns.MinMsgSize
		if opt := r.IsEdns0(); opt != nil {
			size = min(1232, max(dns.MinMsgSize, int(opt.UDPSize())))
		}
		m.Truncate(size)
	}
	_ = w.WriteMsg(m)
	return true
}
