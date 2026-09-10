package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

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

func handleDNS(w dns.ResponseWriter, r *dns.Msg) {
	if handleBoardDNS(w, r, publicBoard) {
		return
	}
	if !rateLimitAllow(w.RemoteAddr().String()) {
		return
	}

	if len(r.Question) == 0 {
		return
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	for _, q := range r.Question {
		if q.Qtype != dns.TypeTXT {
			continue
		}

		name := strings.TrimSuffix(strings.TrimSuffix(q.Name, "."), ".ch.at")
		prompt := strings.ReplaceAll(name, "-", " ")

		// Optimize prompt for DNS constraints
		dnsPrompt := "Answer in 500 characters or less, no markdown formatting: " + prompt

		// Stream LLM response with hard deadline — context cancellation
		// ensures the LLM goroutine is cleaned up when the deadline expires.
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		ch := make(chan string)

		go func() {
			LLM(ctx, dnsPrompt, ch)
		}()

		var response strings.Builder
		channelClosed := false

		for {
			select {
			case chunk, ok := <-ch:
				if !ok {
					channelClosed = true
					goto respond
				}
				response.WriteString(chunk)
				if response.Len() >= 500 {
					goto respond
				}
			case <-ctx.Done():
				if response.Len() == 0 {
					response.WriteString("Request timed out")
				} else if !channelClosed {
					response.WriteString("... (incomplete)")
				}
				goto respond
			}
		}

	respond:
		cancel()
		finalResponse := response.String()
		if len(finalResponse) > 500 {
			finalResponse = finalResponse[:497] + "..."
		} else if len(finalResponse) == 500 && !channelClosed {
			// We hit the exact limit but stream is still going
			finalResponse = finalResponse[:497] + "..."
		}

		// Split response into 255-byte chunks for DNS TXT records
		var txtStrings []string
		for i := 0; i < len(finalResponse); i += 255 {
			end := i + 255
			if end > len(finalResponse) {
				end = len(finalResponse)
			}
			txtStrings = append(txtStrings, finalResponse[i:end])
		}

		txt := &dns.TXT{
			Hdr: dns.RR_Header{
				Name:   q.Name,
				Rrtype: dns.TypeTXT,
				Class:  dns.ClassINET,
				Ttl:    60,
			},
			Txt: txtStrings,
		}
		m.Answer = append(m.Answer, txt)
	}

	w.WriteMsg(m)
}
