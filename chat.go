package main

// Configuration - edit source code and recompile to change settings
// To disable a service: set its port to 0 or delete its .go file
const (
	HTTP_PORT  = 80  // Web interface (set to 0 to disable)
	HTTPS_PORT = 443 // TLS web interface (set to 0 to disable)
	SSH_PORT   = 22  // Anonymous SSH chat (set to 0 to disable)
	DNS_PORT   = 53  // DNS TXT chat (set to 0 to disable)
)

func main() {
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
