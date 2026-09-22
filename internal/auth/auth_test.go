package auth

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeCreds map[string]string

func (f fakeCreds) Lookup(u string) (string, bool, bool) {
	h, ok := f[u]
	return h, u == "admin", ok
}

func newTestManager(t *testing.T) *Manager {
	hash, err := HashPassword("correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(fakeCreds{"admin": hash}, Options{
		SessionTTL: time.Hour, IdleTimeout: time.Hour, MaxAttempts: 3, AttemptWindow: time.Minute,
	})
}

func TestLoginAndSession(t *testing.T) {
	m := newTestManager(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/login", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	sess, err := m.Login(w, r, "admin", "correct-horse")
	if err != nil || sess == nil || !sess.Admin {
		t.Fatalf("login: %v %+v", err, sess)
	}
	cookie := w.Result().Cookies()[0]
	if cookie.Name != CookieName || !cookie.HttpOnly {
		t.Fatalf("cookie %+v", cookie)
	}
	r2 := httptest.NewRequest("GET", "/console", nil)
	r2.AddCookie(cookie)
	if got := m.SessionFromRequest(r2); got == nil || got.Username != "admin" {
		t.Fatalf("session lookup failed: %+v", got)
	}
	m.Logout(httptest.NewRecorder(), r2)
	if m.SessionFromRequest(r2) != nil {
		t.Fatal("session should be gone after logout")
	}
}

func TestLockoutAfterFailures(t *testing.T) {
	m := newTestManager(t)
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest("POST", "/login", nil)
		r.RemoteAddr = "10.0.0.9:1"
		if _, err := m.Login(httptest.NewRecorder(), r, "admin", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	r := httptest.NewRequest("POST", "/login", nil)
	r.RemoteAddr = "10.0.0.9:1"
	if _, err := m.Login(httptest.NewRecorder(), r, "admin", "correct-horse"); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("expected lockout, got %v", err)
	}
	// A different address is unaffected.
	r.RemoteAddr = "10.0.0.10:1"
	if _, err := m.Login(httptest.NewRecorder(), r, "admin", "correct-horse"); err != nil {
		t.Fatalf("other ip: %v", err)
	}
}

func TestClientIPBehindCloudflared(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("CF-Connecting-IP", "203.0.113.7")
	if ip := ClientIP(r); ip != "203.0.113.7" {
		t.Fatalf("loopback+header: %s", ip)
	}
	r.RemoteAddr = "192.168.1.20:5555"
	if ip := ClientIP(r); ip != "192.168.1.20" {
		t.Fatalf("header must be ignored for non-loopback peers: %s", ip)
	}
}
