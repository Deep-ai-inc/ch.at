//go:build boardtest

package main

import (
	"context"
	"encoding/base32"
	"fmt"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func encodedDNSName(text string) string {
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(text))
	labels := []string{"_b32"}
	for len(s) > 0 {
		n := min(63, len(s))
		labels = append(labels, s[:n])
		s = s[n:]
	}
	return strings.Join(labels, ".") + ".ask.ch.at."
}

func TestDNSUTF8Encoding(t *testing.T) {
	for i, prompt := range []string{"你好，世界", "Translate café to 日本語", "Привет мир!", "مرحبا بالعالم", "Preserve My CASE & punctuation: 2+2?", strings.Repeat("long prompt ", 10)} {
		name := encodedDNSName(prompt)
		// DNS 0x20 case randomization must not affect decoding.
		var mixed strings.Builder
		for j, c := range name {
			if j%2 == 0 {
				mixed.WriteString(strings.ToLower(string(c)))
			} else {
				mixed.WriteString(strings.ToUpper(string(c)))
			}
		}
		r := new(dns.Msg)
		r.SetQuestion(mixed.String(), dns.TypeTXT)
		wire, err := r.Pack()
		if err != nil {
			t.Fatal(err)
		}
		r = new(dns.Msg)
		if err := r.Unpack(wire); err != nil {
			t.Fatal(err)
		}
		got, ok, err := decodeDNSPrompt(r.Question[0].Name)
		if !ok || err != nil || got != prompt {
			t.Fatalf("%q %v %v", got, ok, err)
		}
		w := &chatDNSWriter{network: "tcp", ip: fmt.Sprintf("192.0.2.%d", 120+i)}
		handleChatDNS(w, r, func(ctx context.Context, input interface{}, ch chan<- string) (string, error) {
			defer close(ch)
			if input != "Answer in 500 characters or less, no markdown formatting: "+prompt {
				t.Errorf("wrong decoded prompt: %q", input)
			}
			ch <- "answer"
			return "", nil
		})
		if w.msg.Rcode != dns.RcodeSuccess || len(w.msg.Answer) != 1 {
			t.Fatal(w.msg)
		}
	}
}

func TestInvalidDNSEncodingDoesNotCallModel(t *testing.T) {
	for _, name := range []string{
		"_b32.ask.ch.at.", "_b32.0.ask.ch.at.", "_b32.a.ask.ch.at.",
		"_b32.my======.ask.ch.at.", "_b32.my-qq.ask.ch.at.",
		"_b32." + strings.Repeat("m", 64) + ".ask.ch.at.",
		encodedDNSName(string([]byte{0xff})), encodedDNSName("   "),
		"_b32.mz.ask.ch.at.", // Nonzero unused bits: not canonical base32 for f.
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok, err := decodeDNSPrompt(name); !ok || err == nil {
				t.Fatal("accepted invalid encoding")
			}
			r := new(dns.Msg)
			r.SetQuestion(name, dns.TypeTXT)
			w := &chatDNSWriter{ip: "192.0.2.140", network: "tcp"}
			handleChatDNS(w, r, func(_ context.Context, _ interface{}, ch chan<- string) (string, error) {
				close(ch)
				t.Error("called model for invalid encoding")
				return "", nil
			})
			if w.msg.Rcode != dns.RcodeFormatError {
				t.Fatal(w.msg)
			}
		})
	}
	for _, name := range []string{"my.ask.ch.at.", "_b32.my.evil.", "something._b32.my.ask.ch.at."} {
		if _, ok, _ := decodeDNSPrompt(name); ok {
			t.Fatal("guessed an encoding", name)
		}
	}
	// QNAME-minimizing resolvers ask NS questions for intermediate labels.
	r := new(dns.Msg)
	r.SetQuestion("_b32.ask.ch.at.", dns.TypeNS)
	w := &chatDNSWriter{ip: "192.0.2.141"}
	handleChatDNS(w, r, nil)
	if w.msg.Rcode != dns.RcodeSuccess || len(w.msg.Ns) != 1 {
		t.Fatal(w.msg)
	}
}

func TestDNSLongPlainPromptAcrossLabels(t *testing.T) {
	name := "explain.the.difference.between.a.recursive.dns.resolver.and.an.authoritative.dns.server.ask.ch.at."
	prompt, delegated := dnsPrompt(name)
	if len(prompt) <= 63 || !delegated || prompt != "explain the difference between a recursive dns resolver and an authoritative dns server" {
		t.Fatal(prompt)
	}
	r := new(dns.Msg)
	r.SetQuestion(name, dns.TypeTXT)
	if _, err := r.Pack(); err != nil {
		t.Fatal(err)
	}
}
