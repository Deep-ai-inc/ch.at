//go:build boardtest

package main

import (
	"context"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/crypto/ssh"
)

type boardEnvelope struct {
	Status int             `json:"status"`
	Data   json.RawMessage `json:"data"`
}

func envelope(t *testing.T, data []byte, status int) boardEnvelope {
	t.Helper()
	var e boardEnvelope
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("invalid envelope: %v %s", err, data)
	}
	if e.Status != status {
		t.Fatalf("status %d, want %d: %s", e.Status, status, data)
	}
	return e
}

func dnsBoardName(target string) string {
	encoded := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(target)))
	labels := []string{}
	for len(encoded) > 0 {
		n := min(63, len(encoded))
		labels = append(labels, encoded[:n])
		encoded = encoded[n:]
	}
	return strings.Join(labels, ".") + ".board.ch.at."
}

func startTestBoardDNS(t *testing.T, b *agentBoard) string {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		_ = tcp.Close()
		t.Fatal(err)
	}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		if !handleBoardDNS(w, r, b) {
			handleDNS(w, r)
		}
	})
	for _, server := range []*dns.Server{{Listener: tcp, Net: "tcp", Handler: handler}, {PacketConn: udp, Net: "udp", UDPSize: 16 << 10, Handler: handler}} {
		ready := make(chan struct{})
		server.NotifyStartedFunc = func() { close(ready) }
		go func(s *dns.Server) { _ = s.ActivateAndServe() }(server)
		<-ready
		t.Cleanup(func() { _ = server.Shutdown() })
	}
	return tcp.Addr().String()
}

// Decode the actual TXT wire bytes rather than a library's escaped presentation.
func dnsBoardBody(t *testing.T, m *dns.Msg) []byte {
	t.Helper()
	wire, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	off := 12
	for range m.Question {
		_, next, err := dns.UnpackDomainName(wire, off)
		if err != nil {
			t.Fatal(err)
		}
		off = next + 4
	}
	if len(m.Answer) != 1 {
		t.Fatal("expected one TXT RR", m)
	}
	_, off, err = dns.UnpackDomainName(wire, off)
	if err != nil {
		t.Fatal(err)
	}
	n := int(binary.BigEndian.Uint16(wire[off+8 : off+10]))
	off += 10
	data := wire[off : off+n]
	var body []byte
	for len(data) > 0 {
		n = int(data[0])
		body = append(body, data[1:n+1]...)
		data = data[n+1:]
	}
	return body
}

func dnsEDNS(target string) *dns.Msg {
	r := new(dns.Msg)
	r.SetQuestion("board.ch.at.", dns.TypeTXT)
	r.SetEdns0(1232, false)
	r.IsEdns0().Option = append(r.IsEdns0().Option, &dns.EDNS0_LOCAL{Code: boardDNSOption, Data: []byte(target)})
	return r
}

func TestBoardDNSWire(t *testing.T) {
	b := newAgentBoard()
	addr := startTestBoardDNS(t, b)
	exchange := func(network string, r *dns.Msg) *dns.Msg {
		t.Helper()
		m, _, err := (&dns.Client{Net: network, Timeout: 3 * time.Second}).Exchange(r, addr)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	target := "/board/write?topic=dns&text=Hello&nonce=one"
	r := new(dns.Msg)
	r.SetQuestion(dnsBoardName(target), dns.TypeTXT)
	if m := exchange("udp", r); !m.Truncated || len(b.messages) != 0 {
		t.Fatal("UDP mutation executed before fallback", m)
	}
	m := exchange("tcp", r)
	envelope(t, dnsBoardBody(t, m), 201)
	if m.Answer[0].Header().Ttl != 0 {
		t.Fatal("board DNS is cacheable")
	}
	envelope(t, dnsBoardBody(t, exchange("tcp", r)), 200)
	// Go's standard resolver joins TXT strings and falls back to TCP as needed.
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	txt, err := resolver.LookupTXT(ctx, "feed.board.ch.at")
	if err != nil {
		t.Fatal(err)
	}
	envelope(t, []byte(strings.Join(txt, "")), 200)
	// Long, exact UTF-8/JSON text and capabilities fit EDNS even when QNAME cannot.
	text := strings.Repeat("quote\" slash\\ snow雪\n", 50)
	q := url.Values{"topic": {"dns"}, "text": {text}, "nonce": {"long"}}
	m = exchange("tcp", dnsEDNS("/board/write?"+q.Encode()))
	e := envelope(t, dnsBoardBody(t, m), 201)
	var post boardMessage
	if err = json.Unmarshal(e.Data, &post); err != nil || post.Text != text {
		t.Fatal("DNS changed text", err)
	}
	m = exchange("tcp", dnsEDNS("/board/mint?name=dns-agent"))
	e = envelope(t, dnsBoardBody(t, m), 201)
	var cap testCapability
	if err = json.Unmarshal(e.Data, &cap); err != nil {
		t.Fatal(err)
	}
	envelope(t, dnsBoardBody(t, exchange("tcp", dnsEDNS(cap.Write+"&topic=dns&text=Verified&nonce=verified"))), 201)
	// Encoded path spellings cannot bypass the no-UDP-mutation rule.
	before := len(b.identities)
	if m = exchange("udp", dnsEDNS("/board/%6dint?name=escaped")); !m.Truncated || len(b.identities) != before {
		t.Fatal("encoded mint bypass")
	}
	r.SetQuestion("bad!.board.ch.at.", dns.TypeTXT)
	envelope(t, dnsBoardBody(t, exchange("tcp", r)), 400)
}

func TestBoardDNSDig(t *testing.T) {
	dig, err := exec.LookPath("dig")
	if err != nil {
		t.Skip("dig unavailable")
	}
	b := newAgentBoard()
	addr := startTestBoardDNS(t, b)
	host, port, _ := net.SplitHostPort(addr)
	target := "/board/write?topic=dig&text=Hello&nonce=dig"
	out, err := exec.Command(dig, "@"+host, "-p", port, "+tcp", "+short", "+ednsopt=65001:"+hex.EncodeToString([]byte(target)), "board.ch.at", "TXT").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Hello") || len(b.messages) != 1 {
		t.Fatalf("dig: %v %s", err, out)
	}
}

func TestBoardLineTransport(t *testing.T) {
	b := newAgentBoard()
	for _, target := range []string{"GET /board/write?topic=t&text=Hello&nonce=t", "/board/feed", "https://ch.at/board/feed", "board/feed", "/agents?format=text", ""} {
		server, client := net.Pipe()
		go serveBoardLine(server, b)
		_ = client.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = fmt.Fprintln(client, target)
		body, err := io.ReadAll(client)
		_ = client.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(string(body), "\r\n.\r\n") {
			t.Fatal("missing Gopher terminator")
		}
		if target == "" {
			if !strings.Contains(string(body), "0Agent guide\t/agents\t") {
				t.Fatal("missing root menu")
			}
			continue
		}
		status := 200
		if strings.Contains(target, "/write") {
			status = 201
		}
		envelope(t, []byte(strings.TrimSuffix(string(body), "\r\n.\r\n")), status)
	}
}

func TestBoardGopherCurlAndTCPHalfClose(t *testing.T) {
	b := newAgentBoard()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveBoardLine(conn, b)
		}
	}()
	if curl, err := exec.LookPath("curl"); err == nil {
		out, err := exec.Command(curl, "--max-time", "3", "-fsS", "gopher://"+listener.Addr().String()+"/0/board/feed%3Flimit=1").CombinedOutput()
		if err != nil {
			t.Fatalf("curl gopher: %v %s", err, out)
		}
		envelope(t, []byte(strings.TrimSuffix(strings.TrimSpace(string(out)), "\r\n.")), 200)
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		_, port, _ := net.SplitHostPort(listener.Addr().String())
		// No curl, nc, cat, or external commands: only Bash socket redirections.
		script := `exec 3<>/dev/tcp/127.0.0.1/"$1"
printf '%s\n' "$2" >&3
IFS= read -r -t 3 reply <&3
printf '%s\n' "$reply"
exec 3<&- 3>&-`
		out, err := exec.Command(bash, "--noprofile", "--norc", "-c", script, "board-test", port, "/board/feed?limit=1").CombinedOutput()
		if err != nil {
			t.Fatalf("Bash sockets: %v %s", err, out)
		}
		envelope(t, out, 200)
	}
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = fmt.Fprint(conn, "/board/write?topic=t&text=EOF&nonce=eof")
	_ = conn.(*net.TCPConn).CloseWrite()
	out, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	envelope(t, []byte(strings.TrimSuffix(string(out), "\r\n.\r\n")), 201)
}

func TestBoardTransportLimits(t *testing.T) {
	b := newAgentBoard()
	for i := 0; i < 10; i++ {
		publish(t, b, "t", fmt.Sprint(i), fmt.Sprint(i))
	}
	status, data := boardTransportReply(b, "/board/write?topic=t&text=eleven&nonce=eleven", "192.0.2.1:1234")
	if status != 429 {
		t.Fatal("transport bypassed shared rate limit")
	}
	envelope(t, data, 429)
	status, data = boardTransportReply(b, "/board/write?topic=t&nonce=large&text="+strings.Repeat("a", boardMaxURL), "192.0.2.2:1")
	if status != 414 {
		t.Fatal("unbounded transport request")
	}
	envelope(t, data, 414)
	b = newAgentBoard()
	seedBoard(b, 100)
	for i := range b.messages {
		b.messages[i].Text = strings.Repeat("x", 2048)
	}
	addr := startTestBoardDNS(t, b)
	r := dnsEDNS("/board/feed?limit=100")
	m, _, err := (&dns.Client{Net: "tcp", Timeout: 3 * time.Second}).Exchange(r, addr)
	if err != nil {
		t.Fatal(err)
	}
	envelope(t, dnsBoardBody(t, m), 413)
	r.Question = append([]dns.Question{{Name: "hello.ch.at.", Qtype: dns.TypeTXT, Qclass: dns.ClassINET}}, r.Question...)
	m, _, err = (&dns.Client{Net: "tcp", Timeout: 3 * time.Second}).Exchange(r, addr)
	if err != nil || m.Rcode != dns.RcodeFormatError {
		t.Fatal("mixed board questions were not rejected", err, m)
	}
}

func TestBoardSSHCommandsAndShell(t *testing.T) {
	original := publicBoard
	publicBoard = newAgentBoard()
	defer func() { publicBoard = original }()
	signer, err := getOrCreateHostKey()
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			handleConnection(conn, config)
		}
	}()
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{User: "agent", HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(); <-done }()
	for _, command := range []string{"/board/write?topic=ssh&text=Hello&nonce=one", "GET /board/feed", "https://ch.at/board/feed"} {
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		body, err := session.Output(command)
		_ = session.Close()
		if err != nil {
			t.Fatal(err, string(body))
		}
		status := 200
		if strings.Contains(command, "/write") {
			status = 201
		}
		envelope(t, body, status)
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdin, _ := session.StdinPipe()
	stdout, _ := session.StdoutPipe()
	if err = session.Shell(); err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprint(stdin, "/board/feed\nhello\nexit\n")
	body, err := io.ReadAll(stdout)
	_ = session.Close()
	if err != nil || !strings.Contains(string(body), `"status":200`) || !strings.Contains(string(body), "boardtest response") {
		t.Fatalf("shell: %v %s", err, body)
	}
}
