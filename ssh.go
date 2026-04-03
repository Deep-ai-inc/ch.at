package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"
)

func StartSSHServer(port int) error {
	// SSH server configuration
	config := &ssh.ServerConfig{
		NoClientAuth: true, // Anonymous access
	}

	// Get or create persistent host key
	privateKey, err := getOrCreateHostKey()
	if err != nil {
		return fmt.Errorf("failed to get host key: %v", err)
	}
	config.AddHostKey(privateKey)

	// Listen for connections
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	defer listener.Close()

	// Simple connection limiting
	sem := make(chan struct{}, 100) // Max 100 concurrent SSH connections

	for {
		conn, err := listener.Accept()
		if err != nil {
			// Connection error - continue accepting others
			continue
		}

		select {
		case sem <- struct{}{}:
			go func() {
				defer func() { <-sem }()
				handleConnection(conn, config)
			}()
		default:
			// Too many connections
			conn.Close()
		}
	}
}

func handleConnection(netConn net.Conn, config *ssh.ServerConfig) {
	defer netConn.Close()

	// Rate limiting
	if !rateLimitAllow(netConn.RemoteAddr().String()) {
		netConn.Write([]byte("Rate limit exceeded\r\n"))
		return
	}

	// Perform SSH handshake
	sshConn, chans, reqs, err := ssh.NewServerConn(netConn, config)
	if err != nil {
		// Handshake failed - continue accepting others
		return
	}
	defer sshConn.Close()

	go ssh.DiscardRequests(reqs)

	// Handle channels (sessions)
	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		channel, requests, err := newChannel.Accept()
		if err != nil {
			// Channel error - continue
			continue
		}

		go handleSession(channel, requests)
	}
}

func handleSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()

	// Session-scoped context — cancelled when session ends, which
	// ensures any in-flight LLM goroutines are cleaned up.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle session requests
	go func() {
		for req := range requests {
			switch req.Type {
			case "shell", "pty-req":
				req.Reply(true, nil)
			default:
				req.Reply(false, nil)
			}
		}
	}()

	fmt.Fprintf(channel, "Welcome to ch.at\r\n")
	fmt.Fprintf(channel, "Type your message and press Enter.\r\n")
	fmt.Fprintf(channel, "Exit: type 'exit', Ctrl+C, or Ctrl+D\r\n")
	fmt.Fprintf(channel, "> ")

	// Read line by line
	var input strings.Builder
	buf := make([]byte, 1024)

	for {
		n, err := channel.Read(buf)
		if err != nil {
			// EOF (Ctrl+D) or other error - exit cleanly
			return
		}

		data := string(buf[:n])
		for _, ch := range data {
			if ch == 3 { // Ctrl+C
				fmt.Fprintf(channel, "^C\r\n")
				return
			} else if ch == '\n' || ch == '\r' {
				fmt.Fprintf(channel, "\r\n") // Echo newline
				if input.Len() > 0 {
					query := strings.TrimSpace(input.String())
					input.Reset()

					if query == "exit" {
						return
					}

					// Get LLM response with streaming
					ch := make(chan string)
					go func() {
						if _, err := LLM(ctx, query, ch); err != nil {
							fmt.Fprintf(channel, "Error: %s\r\n", err.Error())
						}
					}()

					// Stream response as it arrives
					for chunk := range ch {
						fmt.Fprint(channel, chunk)
					}

					fmt.Fprintf(channel, "\r\n> ")
				}
			} else if ch == '\b' || ch == 127 { // Backspace or Delete
				if input.Len() > 0 {
					// Remove last rune (UTF-8 safe)
					str := input.String()
					runes := []rune(str)
					input.Reset()
					input.WriteString(string(runes[:len(runes)-1]))
					// Move cursor back, overwrite with space, move back again
					fmt.Fprintf(channel, "\b \b")
				}
			} else {
				// Echo the character back to the user
				fmt.Fprintf(channel, "%c", ch)
				input.WriteRune(ch)
			}
		}
	}
}

// getOrCreateHostKey generates a new ephemeral host key
func getOrCreateHostKey() (ssh.Signer, error) {
	// Generate new ephemeral key each time
	// Users will see "host key changed" warnings on each restart
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	return ssh.NewSignerFromKey(key)
}
