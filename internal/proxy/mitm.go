package proxy

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/pdonorio/claude-context-proxy/internal/cert"
	"github.com/pdonorio/claude-context-proxy/internal/config"
)

// ConnectHandler returns an http.HandlerFunc that handles HTTP CONNECT tunnelling.
// For api.anthropic.com it performs MITM TLS interception so token headers can be
// extracted; all other hosts are forwarded as transparent tunnels.
func ConnectHandler(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, cfg *config.Config, onTokens OnTokensFn) http.HandlerFunc {
	var mu sync.Mutex
	leafCache := map[string]tls.Certificate{}

	leafFor := func(host string) (tls.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := leafCache[host]; ok {
			return c, nil
		}
		c, err := cert.LeafCert(host, caCert, caKey)
		if err != nil {
			return tls.Certificate{}, err
		}
		leafCache[host] = c
		return c, nil
	}

	return func(w http.ResponseWriter, r *http.Request) {
		// If CA was not initialised, fall back to a transparent tunnel for all hosts.
		if caCert == nil || caKey == nil {
			host := r.Host
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijacking not supported", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			conn, _, err := hijacker.Hijack()
			if err != nil {
				return
			}
			upstream, err := net.DialTimeout("tcp", host, 10*time.Second)
			if err != nil {
				conn.Close()
				return
			}
			bridge(conn, upstream)
			return
		}

		host := r.Host // "api.anthropic.com:443"
		hostname, _, err := net.SplitHostPort(host)
		if err != nil {
			hostname = host
		}

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking not supported", http.StatusInternalServerError)
			return
		}

		// Acknowledge the CONNECT — raw bytes, headers already flushed by WriteHeader.
		w.WriteHeader(http.StatusOK)

		conn, _, err := hijacker.Hijack()
		if err != nil {
			log.Printf("mitm: hijack %s: %v", host, err)
			return
		}

		// Non-anthropic hosts: transparent TCP tunnel, no inspection.
		if hostname != "api.anthropic.com" {
			upstream, err := net.DialTimeout("tcp", host, 10*time.Second)
			if err != nil {
				log.Printf("mitm: dial %s: %v", host, err)
				conn.Close()
				return
			}
			bridge(conn, upstream)
			return
		}

		// MITM path: generate/reuse a leaf cert for api.anthropic.com.
		tlsCert, err := leafFor(hostname)
		if err != nil {
			log.Printf("mitm: leaf cert for %s: %v", hostname, err)
			conn.Close()
			return
		}

		clientTLS := tls.Server(conn, &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
			NextProtos:   []string{"http/1.1"},
		})
		if err := clientTLS.Handshake(); err != nil {
			// Connection reset / cancelled by client — not an error worth logging.
			clientTLS.Close()
			return
		}
		if cfg.Debug {
			log.Printf("mitm: CONNECT %s — TLS OK, serving requests", host)
		}

		// Serve the decrypted HTTP/1.1 stream through the standard handler.
		ln := newOneConnListener(clientTLS)
		srv := &http.Server{
			Handler:     Handler(Upstream, cfg, onTokens),
			ReadTimeout: 0,
		}
		srv.Serve(ln) //nolint:errcheck

		if cfg.Debug {
			log.Printf("mitm: CONNECT %s — done", host)
		}
	}
}

// bridge copies bytes between two connections until either side closes.
func bridge(a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }() //nolint:errcheck
	go func() { io.Copy(b, a); done <- struct{}{} }() //nolint:errcheck
	<-done
}

// oneConnListener is a net.Listener that vends exactly one connection.
// After Accept returns the connection, subsequent calls block until Close is called.
type oneConnListener struct {
	conn   net.Conn
	connCh chan net.Conn
	done   chan struct{}
	once   sync.Once
}

func newOneConnListener(conn net.Conn) *oneConnListener {
	l := &oneConnListener{
		conn:   conn,
		connCh: make(chan net.Conn, 1),
		done:   make(chan struct{}),
	}
	// Wrap so that when the connection closes we unblock Accept.
	l.connCh <- &notifyConn{Conn: conn, onClose: func() { l.Close() }}
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.connCh:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// notifyConn wraps a net.Conn and calls onClose once when Close is called.
type notifyConn struct {
	net.Conn
	onClose func()
	once    sync.Once
}

func (c *notifyConn) Close() error {
	c.once.Do(c.onClose)
	return c.Conn.Close()
}
