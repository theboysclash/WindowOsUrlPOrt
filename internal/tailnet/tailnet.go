// Package tailnet embeds a Tailscale node in the server using tsnet. The
// console becomes reachable on the tailnet at https://<hostname>.<tailnet>.ts.net
// without installing Tailscale on the host, and optionally on the public
// internet through Tailscale Funnel. Because Tailscale uses WireGuard
// peer-to-peer (falling back to relays on port 443) it keeps working on
// networks that block tunnelling domains.
package tailnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"

	"github.com/theboysclash/WindowOsUrlPOrt/internal/config"
)

type State string

const (
	StateDisabled   State = "disabled"
	StateStarting   State = "starting"
	StateNeedsLogin State = "needs_login"
	StateRunning    State = "running"
	StateError      State = "error"
	StateStopped    State = "stopped"
)

type Status struct {
	State State `json:"state"`
	// URL is the HTTPS address inside the tailnet (and on the internet when
	// Funnel is on).
	URL string `json:"url,omitempty"`
	// AuthURL is the one-time link the admin must open to attach this node
	// to a Tailscale account.
	AuthURL  string `json:"auth_url,omitempty"`
	Funnel   bool   `json:"funnel"`
	Hostname string `json:"hostname,omitempty"`
	IP       string `json:"ip,omitempty"`
	Error    string `json:"error,omitempty"`
	// Note carries a human hint, e.g. how to enable Funnel.
	Note string `json:"note,omitempty"`
}

type Manager struct {
	dataDir string
	log     *slog.Logger
	// selfSigned is used on the tailnet listener when Tailscale HTTPS
	// certificates are not enabled for the tailnet.
	selfSigned *tls.Certificate

	mu      sync.Mutex
	cfg     config.Tailscale
	state   State
	url     string
	authURL string
	ip      string
	err     error
	note    string
	srv     *tsnet.Server
	httpSrv *http.Server
	cancel  context.CancelFunc
	done    chan struct{}
}

func NewManager(cfg config.Tailscale, dataDir string, selfSigned *tls.Certificate, log *slog.Logger) *Manager {
	return &Manager{cfg: cfg, dataDir: dataDir, selfSigned: selfSigned, log: log, state: StateDisabled}
}

func (m *Manager) set(fn func()) {
	m.mu.Lock()
	fn()
	m.mu.Unlock()
}

// Start brings the node up in the background and serves handler on it.
// Calling it again restarts with the new configuration.
func (m *Manager) Start(cfg config.Tailscale, handler http.Handler) {
	m.Stop()
	m.mu.Lock()
	m.cfg = cfg
	m.err, m.url, m.authURL, m.ip, m.note = nil, "", "", "", ""
	if !cfg.Enabled {
		m.state = StateDisabled
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	m.state = StateStarting
	done := m.done
	m.mu.Unlock()

	go func() {
		defer close(done)
		if err := m.run(ctx, cfg, handler); err != nil && ctx.Err() == nil {
			m.log.Error("tailscale", "err", err)
			m.set(func() { m.state, m.err = StateError, err })
		}
	}()
}

func (m *Manager) run(ctx context.Context, cfg config.Tailscale, handler http.Handler) error {
	srv := &tsnet.Server{
		Dir:        filepath.Join(m.dataDir, "tailscale"),
		Hostname:   cfg.Hostname,
		AuthKey:    cfg.AuthKey,
		ControlURL: cfg.ControlURL,
		Ephemeral:  false,
		Logf:       logger.Discard,
		UserLogf:   m.dedupLogf(),
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("start tailscale node: %w", err)
	}
	m.set(func() { m.srv = srv })
	defer func() {
		_ = srv.Close()
		m.set(func() {
			if m.srv == srv {
				m.srv = nil
			}
		})
	}()

	lc, err := srv.LocalClient()
	if err != nil {
		return err
	}

	// Up blocks until the node is authenticated and running; meanwhile poll
	// the backend state so the login link can be shown in the UI.
	upErr := make(chan error, 1)
	go func() {
		_, err := srv.Up(ctx)
		upErr <- err
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	loop := true
	for loop {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-upErr:
			if err != nil {
				return fmt.Errorf("tailscale login: %w", err)
			}
			loop = false
		case <-ticker.C:
			st, err := lc.StatusWithoutPeers(ctx)
			if err != nil {
				continue
			}
			if st.BackendState == ipn.NeedsLogin.String() && st.AuthURL != "" {
				m.set(func() {
					m.state, m.authURL = StateNeedsLogin, st.AuthURL
					m.note = "Open the link, sign in to Tailscale and approve this machine."
				})
			}
		}
	}

	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return err
	}
	host := strings.TrimSuffix(st.Self.DNSName, ".")
	ip4, _ := srv.TailscaleIPs()

	var ln net.Listener
	note := ""
	if cfg.Funnel {
		ln, err = srv.ListenFunnel("tcp", ":443")
		if err != nil {
			// Fall back to tailnet-only so the console stays reachable, and
			// tell the admin what Tailscale needs.
			note = "Funnel unavailable: " + shortErr(err) + " Serving on the tailnet only."
			m.log.Warn("tailscale funnel", "err", err)
			ln, err = m.listenTailnet(srv)
		}
	} else {
		ln, err = m.listenTailnet(srv)
	}
	if err != nil {
		return fmt.Errorf("tailscale listen: %w", err)
	}

	url := "https://" + host
	if host == "" {
		url = "https://" + ip4.String()
	}
	m.set(func() {
		m.state, m.url, m.authURL, m.ip, m.note = StateRunning, url, "", ip4.String(), note
	})
	m.log.Info("tailscale ready", "url", url, "funnel", cfg.Funnel && note == "")

	hs := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	m.set(func() { m.httpSrv = hs })
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdownCtx)
	}()
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// listenTailnet serves HTTPS on the tailnet. Tailscale-issued certificates
// are used when the tailnet has HTTPS enabled; otherwise the server's own
// self-signed certificate keeps the console working (with a browser warning).
func (m *Manager) listenTailnet(srv *tsnet.Server) (net.Listener, error) {
	if len(srv.CertDomains()) > 0 {
		return srv.ListenTLS("tcp", ":443")
	}
	ln, err := srv.Listen("tcp", ":443")
	if err != nil {
		return nil, err
	}
	if m.selfSigned == nil {
		return ln, nil
	}
	m.set(func() {
		m.note = "Tailnet HTTPS certificates are off (Admin console > DNS > HTTPS Certificates); using the self-signed certificate."
	})
	return tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*m.selfSigned}, MinVersion: tls.VersionTLS12}), nil
}

// dedupLogf forwards tsnet's user-facing log lines to slog, dropping the
// login reminder tsnet repeats every few seconds while waiting for sign-in.
func (m *Manager) dedupLogf() logger.Logf {
	var mu sync.Mutex
	last := ""
	return func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		mu.Lock()
		dup := line == last
		last = line
		mu.Unlock()
		if !dup {
			m.log.Info("tailscale: " + line)
		}
	}
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.Index(s, "\n"); i > 0 {
		s = s[:i]
	}
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// Stop shuts the node down.
func (m *Manager) Stop() {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.cancel, m.done = nil, nil
	m.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	m.set(func() {
		if m.state != StateDisabled {
			m.state = StateStopped
		}
		m.url, m.authURL = "", ""
	})
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{State: m.state, Funnel: m.cfg.Funnel, Hostname: m.cfg.Hostname, Note: m.note}
	if m.state == StateRunning {
		st.URL, st.IP = m.url, m.ip
	}
	if m.state == StateNeedsLogin {
		st.AuthURL = m.authURL
	}
	if m.err != nil {
		st.Error = m.err.Error()
	}
	return st
}
