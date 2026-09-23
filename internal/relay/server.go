// Package relay lets the console be reached through a small public relay
// (for example a GitHub Codespace forwarded port on *.app.github.dev) when the
// network the viewer is on blocks Tailscale and Cloudflare domains.
//
// The host PC dials out to the relay over a WebSocket and multiplexes streams
// on it with yamux; the relay reverse-proxies browser requests into those
// streams. Nothing on the host PC has to accept inbound connections.
package relay

import (
	"context"
	"crypto/subtle"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

const (
	// AgentPath is where the host PC connects. It is the only path the relay
	// does not forward.
	AgentPath = "/_relay/agent"
	// ClientIPHeader carries the viewer's address from the relay to the host
	// so login lockouts apply per viewer rather than to the relay.
	ClientIPHeader = "X-Vmrelay-Client-Ip"
)

var errOffline = errors.New("host PC is not connected to the relay")

// Server is the relay side: an http.Handler that accepts one agent and
// forwards every other request to it.
type Server struct {
	key   string
	log   *slog.Logger
	tr    *http.Transport
	proxy *httputil.ReverseProxy

	mu   sync.Mutex
	sess *yamux.Session
}

func NewServer(key string, log *slog.Logger) *Server {
	s := &Server{key: key, log: log}
	s.tr = &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return s.open()
		},
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: "vmserver"})
			pr.Out.Host = publicHost(pr.In)
			pr.Out.Header.Set(ClientIPHeader, clientIP(pr.In))
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
		},
		Transport:     s.tr,
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelDebug),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			offline(w)
		},
	}
	return s
}

// Connected reports whether a host PC is attached.
func (s *Server) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess != nil && !s.sess.IsClosed()
}

func (s *Server) open() (net.Conn, error) {
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	if sess == nil || sess.IsClosed() {
		return nil, errOffline
	}
	return sess.Open()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case AgentPath:
		s.serveAgent(w, r)
		return
	case "/_relay/health":
		w.Header().Set("Content-Type", "application/json")
		if s.Connected() {
			io.WriteString(w, `{"host_connected":true}`)
		} else {
			io.WriteString(w, `{"host_connected":false}`)
		}
		return
	}
	if !s.Connected() {
		offline(w)
		return
	}
	s.proxy.ServeHTTP(w, r)
}

func (s *Server) serveAgent(w http.ResponseWriter, r *http.Request) {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.key == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.key)) != 1 {
		s.log.Warn("agent rejected: wrong key", "remote", clientIP(r))
		http.Error(w, "wrong relay key", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	c.SetReadLimit(-1)
	nc := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
	sess, err := yamux.Client(nc, muxConfig())
	if err != nil {
		nc.Close()
		return
	}

	s.mu.Lock()
	old := s.sess
	s.sess = sess
	s.mu.Unlock()
	if old != nil {
		old.Close()
	}
	s.tr.CloseIdleConnections()
	s.log.Info("host PC connected", "remote", clientIP(r))

	<-sess.CloseChan()

	s.mu.Lock()
	if s.sess == sess {
		s.sess = nil
	}
	s.mu.Unlock()
	s.tr.CloseIdleConnections()
	s.log.Info("host PC disconnected")
}

func muxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.ConnectionWriteTimeout = 30 * time.Second
	cfg.MaxStreamWindowSize = 4 << 20
	cfg.StreamOpenTimeout = 30 * time.Second
	return cfg
}

// publicHost is the hostname the browser used, so the host's Origin checks
// see the same value the browser sends.
func publicHost(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		return strings.TrimSpace(strings.Split(h, ",")[0])
	}
	return r.Host
}

// clientIP takes the nearest public address from X-Forwarded-For. Entries
// further left are supplied by the client and cannot be trusted.
func clientIP(r *http.Request) string {
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip != nil && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

var offlinePage = template.Must(template.New("").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="10"><title>VM Server - offline</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0f1117;color:#e6e8ee;font:16px system-ui,sans-serif}
main{max-width:30rem;padding:2rem;background:#171a22;border:1px solid #2a2f3a;border-radius:12px;text-align:center}
p{color:#a9b0bf;line-height:1.5}</style></head>
<body><main><h1>The host PC is not connected</h1>
<p>Start <b>vmserver.exe</b> on the host PC. This page reloads by itself and opens the sign-in page once it connects.</p></main></body></html>`))

func offline(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadGateway)
	_ = offlinePage.Execute(w, nil)
}
