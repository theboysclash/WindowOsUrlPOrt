// Package httpserver wires authentication, the VM API and the WebSocket VNC
// bridge onto a single HTTPS listener.
package httpserver

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/theboysclash/WindowOsUrlPOrt/internal/auth"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/config"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/relay"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/tailnet"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/tunnel"
	"github.com/theboysclash/WindowOsUrlPOrt/internal/vm"
	"github.com/theboysclash/WindowOsUrlPOrt/web"
)

type Server struct {
	cfg    *config.Config
	auth   *auth.Manager
	vm     *vm.Manager
	tunnel *tunnel.Manager
	tail   *tailnet.Manager
	relay  *relay.Client
	log    *slog.Logger
	tmpl   *template.Template

	handlerOnce sync.Once
	handler     http.Handler

	upgrader websocket.Upgrader
	control  controller
	httpSrv  *http.Server
}

// controller tracks which browser session currently owns keyboard/mouse.
type controller struct {
	mu        sync.Mutex
	sessionID string
	username  string
	since     time.Time
	cancel    context.CancelFunc
	viewers   int
}

func New(cfg *config.Config, am *auth.Manager, vmm *vm.Manager, tun *tunnel.Manager, tail *tailnet.Manager, log *slog.Logger) (*Server, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"json": jsonAttr,
	}).ParseFS(web.FS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, auth: am, vm: vmm, tunnel: tun, tail: tail, log: log, tmpl: tmpl}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  64 * 1024,
		WriteBufferSize: 64 * 1024,
		CheckOrigin:     sameOrigin,
	}
	return s, nil
}

// SetRelay makes the relay connection state visible in the status API.
func (s *Server) SetRelay(c *relay.Client) { s.relay = c }

// Handler returns the routed application handler. It is shared by the local
// HTTPS listener, the Tailscale listener and the relay.
func (s *Server) Handler() http.Handler {
	s.handlerOnce.Do(func() { s.handler = s.buildHandler() })
	return s.handler
}

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(web.FS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheControl(http.FileServer(http.FS(static)))))

	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	mux.Handle("GET /console", s.requireAuth(http.HandlerFunc(s.handleConsole)))
	mux.Handle("GET /setup", s.requireAdmin(http.HandlerFunc(s.handleSetupPage)))
	mux.Handle("GET /ws/vnc", s.requireAuth(http.HandlerFunc(s.handleVNC)))

	api := http.NewServeMux()
	api.HandleFunc("GET /api/status", s.apiStatus)
	api.HandleFunc("GET /api/me", s.apiMe)
	api.HandleFunc("POST /api/control/take", s.apiTakeControl)
	api.HandleFunc("POST /api/control/release", s.apiReleaseControl)
	api.HandleFunc("POST /api/vm/start", s.vmAction(func(ctx context.Context) error { return s.vm.Start(ctx) }))
	api.HandleFunc("POST /api/vm/shutdown", s.vmAction(s.vm.Shutdown))
	api.HandleFunc("POST /api/vm/reset", s.vmAction(s.vm.Reset))
	api.HandleFunc("POST /api/vm/pause", s.vmAction(s.vm.Pause))
	api.HandleFunc("POST /api/vm/resume", s.vmAction(s.vm.Resume))
	api.HandleFunc("POST /api/vm/forcestop", s.vmAction(func(context.Context) error { return s.vm.ForceStop() }))
	api.HandleFunc("GET /api/snapshots", s.apiListSnapshots)
	api.HandleFunc("POST /api/snapshots", s.apiCreateSnapshot)
	api.HandleFunc("POST /api/snapshots/{name}/restore", s.apiRestoreSnapshot)
	api.HandleFunc("DELETE /api/snapshots/{name}", s.apiDeleteSnapshot)
	api.Handle("POST /api/setup", s.requireAdmin(http.HandlerFunc(s.apiSetup)))
	api.Handle("POST /api/vm/eject", s.requireAdmin(http.HandlerFunc(s.apiEject)))
	api.Handle("POST /api/upload/iso", s.requireAdmin(http.HandlerFunc(s.apiUploadISO)))
	api.Handle("GET /api/isos", s.requireAdmin(http.HandlerFunc(s.apiListISOs)))
	api.Handle("POST /api/tunnel", s.requireAdmin(http.HandlerFunc(s.apiTunnel)))
	api.Handle("GET /api/tunnel/log", s.requireAdmin(http.HandlerFunc(s.apiTunnelLog)))
	api.Handle("POST /api/tailscale", s.requireAdmin(http.HandlerFunc(s.apiTailscale)))
	api.Handle("POST /api/users", s.requireAdmin(http.HandlerFunc(s.apiAddUser)))
	api.Handle("POST /api/password", http.HandlerFunc(s.apiChangePassword))
	mux.Handle("/api/", s.requireAuth(csrfGuard(api)))

	return securityHeaders(s.logRequests(mux))
}

// ListenAndServe starts the HTTPS (or HTTP) listener and blocks until ctx is
// cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	s.httpSrv = srv

	var tlsCfg *tls.Config
	switch s.cfg.TLS.Mode {
	case config.TLSOff:
	case config.TLSFiles:
		cert, err := tls.LoadX509KeyPair(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
		if err != nil {
			return fmt.Errorf("load tls files: %w", err)
		}
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	default:
		cert, err := LoadOrCreateSelfSigned(s.cfg.DataDir)
		if err != nil {
			return fmt.Errorf("self-signed certificate: %w", err)
		}
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}

	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	if tlsCfg != nil {
		srv.TLSConfig = tlsCfg
		ln = tls.NewListener(ln, tlsCfg)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// LocalOrigin returns the loopback URL cloudflared should forward to.
func LocalOrigin(cfg *config.Config) string {
	scheme := "https"
	if cfg.TLS.Mode == config.TLSOff {
		scheme = "http"
	}
	_, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		port = "8443"
	}
	return fmt.Sprintf("%s://127.0.0.1:%s", scheme, port)
}

// URLs returns the addresses users can type into a browser to reach the server.
func (s *Server) URLs() []string {
	scheme := "https"
	if s.cfg.TLS.Mode == config.TLSOff {
		scheme = "http"
	}
	_, port, err := net.SplitHostPort(s.cfg.ListenAddr)
	if err != nil {
		return []string{scheme + "://" + s.cfg.ListenAddr}
	}
	host, _, _ := net.SplitHostPort(s.cfg.ListenAddr)
	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(host, port))}
	}
	urls := []string{fmt.Sprintf("%s://localhost:%s", scheme, port)}
	for _, ip := range LocalIPs() {
		if ip.To4() != nil {
			urls = append(urls, fmt.Sprintf("%s://%s:%s", scheme, ip, port))
		}
	}
	return urls
}

// --- middleware ---

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := s.auth.SessionFromRequest(r)
		if sess == nil {
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws/") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not logged in"})
				return
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(withSession(r.Context(), sess)))
	})
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sess := sessionFrom(r.Context()); sess == nil || !sess.Admin {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin only"})
				return
			}
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// csrfGuard rejects state-changing API calls that did not originate from this
// site. The session cookie is SameSite=Strict as a second layer.
func csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request blocked"})
				return
			}
			if o := r.Header.Get("Origin"); o != "" && !originMatchesHost(o, r.Host) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin mismatch"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	return o == "" || originMatchesHost(o, r.Host)
}

func originMatchesHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; connect-src 'self' wss: ws:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/api/status" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(rw, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rw.code, "remote", auth.ClientIP(r), "ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}

// Hijack is needed so the WebSocket upgrader works through the wrapper.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack not supported")
	}
	return h.Hijack()
}
