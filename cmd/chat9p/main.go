// chat9p is a small native, anonymous 9p2000 client. It does not require a mount,
// local root privileges, an SSH bridge, TLS, or a server account.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"9fans.net/go/plan9"
	"9fans.net/go/plan9/client"
)

func main() {
	addr := flag.String("addr", "ch.at:564", "9p TCP address")
	flag.Parse()
	if err := run(*addr, strings.Join(flag.Args(), " "), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(addr, prompt string, out io.Writer) error {
	if strings.TrimSpace(prompt) == "" || len(prompt) > 8192 || strings.ContainsAny(prompt, "\r\n") {
		return fmt.Errorf("provide one nonempty question of at most 8192 bytes")
	}
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(70 * time.Second))
	protocol, err := client.NewConn(conn)
	if err != nil {
		return err
	}
	defer protocol.Close()
	fs, err := protocol.Attach(nil, "anonymous", "")
	if err != nil {
		return err
	}
	writer, err := fs.Open("ctl", plan9.OWRITE|plan9.OTRUNC)
	if err != nil {
		return err
	}
	_, err = writer.Write([]byte(prompt + "\n"))
	closeErr := writer.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	reader, err := fs.Open("ctl", plan9.OREAD)
	if err != nil {
		return err
	}
	defer reader.Close()
	_, err = io.Copy(out, reader)
	return err
}
