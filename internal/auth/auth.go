// Package auth implements password verification, cookie sessions and
// login rate limiting. Everything is kept in memory; sessions do not survive a
// server restart, which is intentional for a remote-console product.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const CookieName = "vmserver_session"

var ErrInvalidCredentials = errors.New("invalid username or password")
var ErrLockedOut = errors.New("too many failed attempts, try again later")

// Credentials abstracts the user store so auth does not import config.
type Credentials interface {
	// Lookup returns the bcrypt hash and admin flag for username.
	Lookup(username string) (hash string, admin bool, ok bool)
}

type Session struct {
	ID        string
	Username  string
	Admin     bool
	CreatedAt time.Time
	LastSeen  time.Time
	RemoteIP  string
}

type Options struct {
	SessionTTL    time.Duration
	IdleTimeout   time.Duration
	MaxAttempts   int
	AttemptWindow time.Duration
	Secure        bool
}

type Manager struct {
	creds Credentials
	opts  Options

	mu       sync.Mutex
	sessions map[string]*Session
	attempts map[string][]time.Time
}

func NewManager(creds Credentials, opts Options) *Manager {
	m := &Manager{
		creds:    creds,
		opts:     opts,
		sessions: make(map[string]*Session),
		attempts: make(map[string][]time.Time),
	}
	go m.reaper()
	return m
}

func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// RandomPassword returns a URL-safe random string suitable as an initial
// admin password.
func RandomPassword(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (m *Manager) lockedOut(ip string, now time.Time) bool {
	window := now.Add(-m.opts.AttemptWindow)
	kept := m.attempts[ip][:0]
	for _, t := range m.attempts[ip] {
		if t.After(window) {
			kept = append(kept, t)
		}
	}
	m.attempts[ip] = kept
	return len(kept) >= m.opts.MaxAttempts
}

// Login verifies the credentials and, on success, creates a session and sets
// the cookie on w.
func (m *Manager) Login(w http.ResponseWriter, r *http.Request, username, password string) (*Session, error) {
	ip := clientIP(r)
	now := time.Now()

	m.mu.Lock()
	if m.lockedOut(ip, now) {
		m.mu.Unlock()
		return nil, ErrLockedOut
	}
	m.mu.Unlock()

	hash, admin, ok := m.creds.Lookup(username)
	if !ok {
		// Burn roughly the same time as a real bcrypt compare so user
		// enumeration via timing is harder.
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOa5cS0Xx/Mvz9J2Vd3l7Jw0JYrZ0eZK2"), []byte(password))
		ok = false
	} else {
		ok = bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if !ok {
		m.attempts[ip] = append(m.attempts[ip], now)
		return nil, ErrInvalidCredentials
	}
	delete(m.attempts, ip)

	id, err := RandomPassword(32)
	if err != nil {
		return nil, err
	}
	s := &Session{ID: id, Username: username, Admin: admin, CreatedAt: now, LastSeen: now, RemoteIP: ip}
	m.sessions[id] = s
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.opts.Secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(m.opts.SessionTTL / time.Second),
	})
	return s, nil
}

func (m *Manager) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil {
		m.mu.Lock()
		delete(m.sessions, c.Value)
		m.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: m.opts.Secure, SameSite: http.SameSiteStrictMode})
}

// SessionFromRequest returns the live session for the request, or nil.
func (m *Manager) SessionFromRequest(r *http.Request) *Session {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[c.Value]
	if !ok {
		return nil
	}
	now := time.Now()
	if now.Sub(s.CreatedAt) > m.opts.SessionTTL || now.Sub(s.LastSeen) > m.opts.IdleTimeout {
		delete(m.sessions, c.Value)
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(s.ID), []byte(c.Value)) != 1 {
		return nil
	}
	s.LastSeen = now
	cp := *s
	return &cp
}

// Valid reports whether the session with the given id is still alive. It is
// used by long-lived WebSocket connections to enforce idle/absolute timeouts.
func (m *Manager) Valid(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return false
	}
	now := time.Now()
	return now.Sub(s.CreatedAt) <= m.opts.SessionTTL && now.Sub(s.LastSeen) <= m.opts.IdleTimeout
}

// Touch marks the session as active.
func (m *Manager) Touch(id string) {
	m.mu.Lock()
	if s, ok := m.sessions[id]; ok {
		s.LastSeen = time.Now()
	}
	m.mu.Unlock()
}

func (m *Manager) reaper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		m.mu.Lock()
		for id, s := range m.sessions {
			if now.Sub(s.CreatedAt) > m.opts.SessionTTL || now.Sub(s.LastSeen) > m.opts.IdleTimeout {
				delete(m.sessions, id)
			}
		}
		for ip := range m.attempts {
			if !m.lockedOut(ip, now) && len(m.attempts[ip]) == 0 {
				delete(m.attempts, ip)
			}
		}
		m.mu.Unlock()
	}
}
