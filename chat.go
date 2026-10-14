package main

import "log"

// Configuration - edit source code and recompile to change settings
// To disable a service: set its port to 0 or delete its .go file
const (
	HTTP_PORT   = 80   // Web interface (set to 0 to disable)
	HTTPS_PORT  = 443  // TLS web interface (set to 0 to disable)
	SSH_PORT    = 22   // Anonymous SSH chat (set to 0 to disable)
	DNS_PORT    = 53   // DNS TXT chat (set to 0 to disable)
	GOPHER_PORT = 70   // Board text selectors / plain TCP lines (0 disables)
	XMPP_PORT   = 5222 // Anonymous XMPP client connections (0 disables)
	NINEP_PORT  = 564  // Anonymous native 9p2000 (0 disables)
)

func main() {
	if XMPP_PORT > 0 {
		go func() {
			if err := StartXMPPServer(XMPP_PORT); err != nil {
				log.Printf("XMPP listener stopped: %v", err)
			}
		}()
	}
	if NINEP_PORT > 0 {
		go func() {
			if err := StartNinePServer(NINEP_PORT); err != nil {
				log.Printf("9p listener stopped: %v", err)
			}
		}()
	}
	if GOPHER_PORT > 0 {
		go func() { StartGopherServer(GOPHER_PORT) }()
	}
	// SSH Server
	if SSH_PORT > 0 {
		go func() {
			StartSSHServer(SSH_PORT)
		}()
	}

	// DNS Server
	if DNS_PORT > 0 {
		go func() {
			StartDNSServer(DNS_PORT)
		}()
	}

	// HTTP/HTTPS Server
	// TODO: Implement graceful shutdown with signal handling
	if HTTP_PORT > 0 || HTTPS_PORT > 0 {
		if HTTPS_PORT > 0 {
			go func() {
				StartHTTPSServer(HTTPS_PORT, "cert.pem", "key.pem")
			}()
		}

		if HTTP_PORT > 0 {
			StartHTTPServer(HTTP_PORT)
		} else {
			// If only HTTPS is enabled, block forever
			select {}
		}
	} else {
		// If no servers enabled, block forever
		select {}
	}
}
